package capcompute

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aurora-capcompute/capcompute/sys"
	"github.com/aurora-capcompute/capcompute/sys/replay"
	"github.com/aurora-capcompute/capcompute/sys/replay/tape/journaled"
)

type spawnRun struct {
	Child   testPID
	Granted []string
	Request SpawnRequest
}

type fakeRunner struct {
	runs    []spawnRun
	results []ResumeResult[testPID]
}

func (r *fakeRunner) run(_ context.Context, child testPID, granted []sys.Capability, request SpawnRequest) (ResumeResult[testPID], error) {
	names := make([]string, len(granted))
	for i, capability := range granted {
		names[i] = capability.Name
	}
	r.runs = append(r.runs, spawnRun{Child: child, Granted: names, Request: request})
	result := r.results[0]
	if len(r.results) > 1 {
		r.results = r.results[1:]
	}
	return result, nil
}

var parentCaps = []sys.Capability{{Name: "mail.send"}, {Name: "clock.now"}}

func newSpawner(runner *fakeRunner) *Spawner[testPID] {
	return NewSpawner(SpawnConfig[testPID]{
		Grants: func(testPID) []sys.Capability { return parentCaps },
		DeriveCred: func(parent testPID, spawnKey string, program string) testPID {
			return testPID{id: parent.id + "/" + program + "@" + spawnKey}
		},
		Run: runner.run,
	}, &recordingDispatcher{})
}

func spawnSyscall(args string) sys.Syscall {
	return sys.Syscall{Abi: sys.ABIVersion, Name: sys.SyscallSpawn, Args: json.RawMessage(args)}
}

func TestSpawnRefusesEscalation(t *testing.T) {
	runner := &fakeRunner{results: []ResumeResult[testPID]{{Status: ResumeCompleted}}}
	spawner := newSpawner(runner)

	ctx := sys.WithIdempotencyKey(context.Background(), "key-1")
	result, err := spawner.Dispatch(ctx, testPID{id: "parent"}, spawnSyscall(`{"program":"child","capabilities":["k8s.delete"]}`), sys.Authorization{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result.Status() != sys.StatusFailed || result.Errno() != sys.ErrnoDenied {
		t.Fatalf("result = %#v, want failed/denied", result)
	}
	if len(runner.runs) != 0 {
		t.Fatal("escalating spawn reached the runner")
	}
}

func TestSpawnDerivesDeterministicChildAndAttenuates(t *testing.T) {
	runner := &fakeRunner{results: []ResumeResult[testPID]{{Status: ResumeCompleted, Output: json.RawMessage(`{"status":"completed","answer":42}`)}}}
	spawner := newSpawner(runner)

	ctx := sys.WithIdempotencyKey(context.Background(), "key-1")
	result, err := spawner.Dispatch(ctx, testPID{id: "parent"}, spawnSyscall(`{"program":"child","input":{"q":1},"capabilities":["mail.send"]}`), sys.Authorization{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result.Status() != sys.StatusResult || string(result.Result()) != `{"status":"completed","answer":42}` {
		t.Fatalf("result = %#v", result)
	}
	if len(runner.runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runner.runs))
	}
	run := runner.runs[0]
	if run.Child.id != "parent/child@key-1" {
		t.Fatalf("child cred = %q; derivation did not use the idempotency key", run.Child.id)
	}
	if len(run.Granted) != 1 || run.Granted[0] != "mail.send" {
		t.Fatalf("granted = %v, want exactly the attenuated subset", run.Granted)
	}
	if run.Request.Entrypoint != "run" {
		t.Fatalf("entrypoint = %q, want the default", run.Request.Entrypoint)
	}
}

func TestSpawnRequiresIdempotencyKey(t *testing.T) {
	runner := &fakeRunner{results: []ResumeResult[testPID]{{Status: ResumeCompleted}}}
	spawner := newSpawner(runner)

	_, err := spawner.Dispatch(context.Background(), testPID{id: "parent"}, spawnSyscall(`{"program":"child"}`), sys.Authorization{})
	if err == nil || !strings.Contains(err.Error(), "idempotency key") {
		t.Fatalf("err = %v, want the below-replay requirement", err)
	}
}

func TestSpawnMapsChildOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		child      ResumeResult[testPID]
		wantStatus sys.SyscallStatus
		wantErrno  sys.Errno
		wantErr    bool
	}{
		"yield":   {child: ResumeResult[testPID]{Status: ResumeYielded}, wantStatus: sys.StatusYield},
		"failed":  {child: ResumeResult[testPID]{Status: ResumeFailed}, wantStatus: sys.StatusFailed, wantErrno: sys.ErrnoInternal},
		"stopped": {child: ResumeResult[testPID]{Status: ResumeStopped}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{results: []ResumeResult[testPID]{tc.child}}
			spawner := newSpawner(runner)
			ctx := sys.WithIdempotencyKey(context.Background(), "key-1")
			result, err := spawner.Dispatch(ctx, testPID{id: "parent"}, spawnSyscall(`{"program":"child"}`), sys.Authorization{})
			if tc.wantErr {
				if err == nil {
					t.Fatal("want a host error (outcome unknown, intent stays open)")
				}
				return
			}
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			if result.Status() != tc.wantStatus || result.Errno() != tc.wantErrno {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

// The child-workflow protocol under the parent's replay layer: a yielded child
// re-enters via the same derived cred, a completed child's result is journaled,
// and a replayed parent never re-spawns.
func TestSpawnUnderReplay(t *testing.T) {
	journal := newMemJournal()
	header := journaled.Header{ABI: sys.ABIVersion, Program: "sha256:parent", Run: "parent"}
	runner := &fakeRunner{results: []ResumeResult[testPID]{
		{Status: ResumeYielded},
		{Status: ResumeCompleted, Output: json.RawMessage(`{"status":"completed","answer":42}`)},
	}}

	chain := func(t *testing.T) sys.Dispatcher[testPID] {
		t.Helper()
		tape, err := journaled.NewTape(journal, header)
		if err != nil {
			t.Fatalf("new tape: %v", err)
		}
		return replay.NewDispatcher[testPID](tape, newSpawner(runner))
	}
	call := spawnSyscall(`{"program":"child"}`)

	// First quantum: the child yields, so the parent's spawn yields.
	result, err := chain(t).Dispatch(context.Background(), testPID{id: "parent"}, call, sys.Authorization{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result.Status() != sys.StatusYield {
		t.Fatalf("result = %#v, want transitive yield", result)
	}

	// Resume: the open spawn intent is retried; the SAME child must be derived.
	result, err = chain(t).Dispatch(context.Background(), testPID{id: "parent"}, call, sys.Authorization{})
	if err != nil {
		t.Fatalf("resume dispatch: %v", err)
	}
	if result.Status() != sys.StatusResult {
		t.Fatalf("result = %#v, want the child's answer", result)
	}
	if len(runner.runs) != 2 {
		t.Fatalf("runs = %d, want 2 (yield, then re-entry)", len(runner.runs))
	}
	if runner.runs[0].Child != runner.runs[1].Child {
		t.Fatalf("re-entry derived a different child: %q vs %q", runner.runs[0].Child.id, runner.runs[1].Child.id)
	}

	// Crash-replay: the spawn is served from the journal; the runner is not called.
	result, err = chain(t).Dispatch(context.Background(), testPID{id: "parent"}, call, sys.Authorization{})
	if err != nil {
		t.Fatalf("replay dispatch: %v", err)
	}
	if string(result.Result()) != `{"status":"completed","answer":42}` {
		t.Fatalf("replayed result = %s", result.Result())
	}
	if len(runner.runs) != 2 {
		t.Fatalf("replay re-spawned: runs = %d", len(runner.runs))
	}
	if err := journaled.Verify(journal); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestSpawnerExposesSpawnCapability(t *testing.T) {
	spawner := newSpawner(&fakeRunner{results: []ResumeResult[testPID]{{}}})
	capabilities := spawner.Capabilities()
	var spawn *sys.Capability
	for i := range capabilities {
		if capabilities[i].Name == sys.SyscallSpawn {
			spawn = &capabilities[i]
		}
	}
	if spawn == nil || len(spawn.InputSchema) == 0 {
		t.Fatalf("capabilities = %+v, want sys.spawn with an input schema", capabilities)
	}
}
