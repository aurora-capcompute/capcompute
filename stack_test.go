package capcompute

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/aurora-capcompute/capcompute/sys"
	"github.com/aurora-capcompute/capcompute/sys/replay/tape/journaled"
)

// stackDriver is the leaf: it records idempotency keys so the test can prove
// the below-replay layers actually receive them.
type stackDriver struct {
	keys []string
}

func (d *stackDriver) Dispatch(ctx context.Context, _ testPID, syscall sys.Syscall, _ sys.Authorization) (sys.SyscallResult, error) {
	key, _ := sys.IdempotencyKey(ctx)
	d.keys = append(d.keys, key)
	return sys.Result(json.RawMessage(`{"from":"` + syscall.Name + `"}`)), nil
}

func (d *stackDriver) Capabilities() []sys.Capability { return flowCaps }

// The Stack's reason to exist: each layer on its correct side of the replay
// boundary. One test per load-bearing ordering property.
func TestStackEnforcesCanonicalOrder(t *testing.T) {
	journal := newMemJournal()
	header := journaled.Header{ABI: sys.ABIVersion, Program: "sha256:test", Process: "p1"}
	driver := &stackDriver{}

	// Everything the chain publishes is granted, plus the reserved syscalls.
	grants := func(cred testPID) []sys.Capability {
		return append([]sys.Capability(nil), append(flowCaps,
			sys.Capability{Name: sys.SyscallDeclassify},
		)...)
	}
	boot := func(t *testing.T) sys.Dispatcher[testPID] {
		t.Helper()
		tape, err := journaled.NewTape(journal, header)
		if err != nil {
			t.Fatalf("new tape: %v", err)
		}
		chain, err := Stack[string, testPID]{
			Grants: grants,
			Taints: NewTaints[string](), // fresh per boot: a crash loses host memory
			Limit:  NewRateLimit(1000, 1000),
		}.ForProcess(tape, driver)
		if err != nil {
			t.Fatalf("for run: %v", err)
		}
		return chain
	}

	chain := boot(t)

	// Validator on top: an ungranted name is denied and never journaled.
	denied := call(t, chain, "p1", "not.granted")
	if denied.Errno() != sys.ErrnoDenied {
		t.Fatalf("ungranted = %#v, want denied", denied)
	}
	if journal.Length() != 0 {
		t.Fatal("a denied call reached the journal")
	}

	// Labeler below replay: labels land in the journaled completion.
	call(t, chain, "p1", "internet.read")
	completion, err := journal.Load(1)
	if err != nil || completion.Result == nil || !slices.Contains(completion.Result.Labels(), "untrusted_web") {
		t.Fatalf("journaled completion = %+v (err %v), want untrusted_web label", completion, err)
	}

	// FlowMonitor above replay: the taint denies the protected capability…
	if result := call(t, chain, "p1", "k8s.delete"); result.Errno() != sys.ErrnoDenied {
		t.Fatalf("tainted k8s.delete = %#v, want denied", result)
	}
	recordsBeforeCrash := journal.Length()

	// …and a crash-rebooted host (fresh Taints, fresh tape, same journal)
	// re-derives the same denial from replayed results alone, replaying
	// without re-executing anything.
	driverCalls := len(driver.keys)
	rebooted := boot(t)
	call(t, rebooted, "p1", "internet.read")
	if result := call(t, rebooted, "p1", "k8s.delete"); result.Errno() != sys.ErrnoDenied {
		t.Fatalf("post-crash k8s.delete = %#v, want denied (taint not rebuilt)", result)
	}
	if len(driver.keys) != driverCalls {
		t.Fatal("replay re-executed a driver call")
	}
	if journal.Length() != recordsBeforeCrash {
		t.Fatal("replay appended records")
	}
	for _, key := range driver.keys {
		if key == "" {
			t.Fatal("a below-replay layer ran without an idempotency key")
		}
	}
	if err := journaled.Verify(journal); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// Spawn attenuation must hold through the whole assembled chain, not only the
// Spawner in isolation: a guest that asks a spawned child for a capability the
// parent does not hold is denied, and the child runner is never reached. This
// proves the Spawner is wired below the replay layer (so it has an idempotency
// key) yet still behind the Validator.
func TestStackEnforcesSpawnAttenuation(t *testing.T) {
	journal := newMemJournal()
	header := journaled.Header{ABI: sys.ABIVersion, Program: "sha256:test", Process: "parent"}
	driver := &stackDriver{}
	runner := &fakeRunner{results: []ResumeResult[testPID]{{Status: ResumeCompleted, Output: json.RawMessage(`{"status":"completed"}`)}}}

	// The parent holds exactly flowCaps. The Validator's grant set additionally
	// admits sys.spawn (the chain advertises it); the Spawner then attenuates.
	grants := func(testPID) []sys.Capability {
		return append(append([]sys.Capability(nil), flowCaps...), sys.Capability{Name: sys.SyscallSpawn})
	}
	tape, err := journaled.NewTape(journal, header)
	if err != nil {
		t.Fatalf("new tape: %v", err)
	}
	chain, err := Stack[string, testPID]{
		Grants: grants,
		Taints: NewTaints[string](),
		Spawn: &SpawnConfig[testPID]{
			Grants:     func(testPID) []sys.Capability { return flowCaps },
			DeriveCred: func(parent testPID, spawnKey, program string) testPID { return testPID{id: parent.id + "/" + program} },
			Run:        runner.run,
		},
	}.ForProcess(tape, driver)
	if err != nil {
		t.Fatalf("for process: %v", err)
	}

	ctx := context.Background()
	// "danger.rm" is not in flowCaps, so the parent cannot delegate it.
	escalate := sys.Syscall{Abi: sys.ABIVersion, Name: sys.SyscallSpawn, Args: json.RawMessage(`{"program":"child","capabilities":["danger.rm"]}`)}
	result, err := chain.Dispatch(ctx, testPID{id: "parent"}, escalate, sys.Authorization{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result.Status() != sys.StatusFailed || result.Errno() != sys.ErrnoDenied {
		t.Fatalf("escalating spawn = %#v, want failed/denied", result)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("escalating spawn reached the child runner: %+v", runner.runs)
	}

	// A spawn restricted to a capability the parent DOES hold is admitted and
	// the child receives exactly that attenuated subset.
	ok := sys.Syscall{Abi: sys.ABIVersion, Name: sys.SyscallSpawn, Args: json.RawMessage(`{"program":"child","capabilities":["mail.send"]}`)}
	result, err = chain.Dispatch(ctx, testPID{id: "parent"}, ok, sys.Authorization{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result.Status() != sys.StatusResult {
		t.Fatalf("legal spawn = %#v, want result", result)
	}
	if len(runner.runs) != 1 || len(runner.runs[0].Granted) != 1 || runner.runs[0].Granted[0] != "mail.send" {
		t.Fatalf("child granted = %+v, want exactly [mail.send]", runner.runs)
	}
}

func TestStackRequiresItsPieces(t *testing.T) {
	tape, _ := journaled.NewTape(newMemJournal(), journaled.Header{ABI: sys.ABIVersion, Program: "p", Process: "r"})
	if _, err := (Stack[string, testPID]{Taints: NewTaints[string]()}).ForProcess(tape, &recordingDispatcher{}); err == nil {
		t.Fatal("stack without Grants accepted")
	}
	if _, err := (Stack[string, testPID]{Grants: grantsFixture()}).ForProcess(tape, &recordingDispatcher{}); err == nil {
		t.Fatal("stack without Taints accepted")
	}
	if _, err := (Stack[string, testPID]{Grants: grantsFixture(), Taints: NewTaints[string]()}).ForProcess(tape, nil); err == nil {
		t.Fatal("stack without drivers accepted")
	}
}
