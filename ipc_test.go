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

func newMessenger(mailbox Mailbox[string]) *Messenger[string, testPID] {
	return NewMessenger(IPCConfig[string, testPID]{
		Grants:   func(testPID) []sys.Capability { return parentCaps }, // mail.send, clock.now
		Mailbox:  mailbox,
		ParsePID: func(to string) (string, error) { return to, nil },
	}, &recordingDispatcher{})
}

func ipcDispatch(t *testing.T, m *Messenger[string, testPID], pid, key, name, args string) sys.SyscallResult {
	t.Helper()
	ctx := sys.WithIdempotencyKey(context.Background(), key)
	call := sys.Syscall{Abi: sys.ABIVersion, Name: name}
	if args != "" {
		call.Args = json.RawMessage(args)
	}
	result, err := m.Dispatch(ctx, testPID{id: pid}, call, sys.Authorization{})
	if err != nil {
		t.Fatalf("dispatch %s: %v", name, err)
	}
	return result
}

func TestIPCSendRecvRoundTrip(t *testing.T) {
	messenger := newMessenger(NewMemMailbox[string]())

	sent := ipcDispatch(t, messenger, "alice", "k-send", sys.SyscallSend,
		`{"to":"bob","payload":{"task":"review"},"capabilities":["mail.send"]}`)
	if sent.Status() != sys.StatusResult {
		t.Fatalf("send = %#v", sent)
	}

	received := ipcDispatch(t, messenger, "bob", "k-recv", sys.SyscallRecv, "")
	if received.Status() != sys.StatusResult {
		t.Fatalf("recv = %#v", received)
	}
	var message Message
	if err := json.Unmarshal(received.Result(), &message); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if message.From != "alice" || string(message.Payload) != `{"task":"review"}` ||
		len(message.Capabilities) != 1 || message.Capabilities[0] != "mail.send" {
		t.Fatalf("message = %+v", message)
	}
}

func TestIPCSendRefusesEscalation(t *testing.T) {
	mailbox := NewMemMailbox[string]()
	messenger := newMessenger(mailbox)

	result := ipcDispatch(t, messenger, "alice", "k-1", sys.SyscallSend,
		`{"to":"bob","capabilities":["k8s.delete"]}`)
	if result.Status() != sys.StatusFailed || result.Errno() != sys.ErrnoDenied {
		t.Fatalf("escalating send = %#v, want failed/denied", result)
	}
	if recv := ipcDispatch(t, messenger, "bob", "k-2", sys.SyscallRecv, ""); recv.Status() != sys.StatusYield {
		t.Fatalf("denied send still delivered: %#v", recv)
	}
}

func TestIPCEmptyMailboxYields(t *testing.T) {
	messenger := newMessenger(NewMemMailbox[string]())
	result := ipcDispatch(t, messenger, "bob", "k-1", sys.SyscallRecv, "")
	if result.Status() != sys.StatusYield {
		t.Fatalf("empty recv = %#v, want yield (parked until delivery)", result)
	}
}

func TestIPCDeliveryIsOrderedAndKeyed(t *testing.T) {
	messenger := newMessenger(NewMemMailbox[string]())

	ipcDispatch(t, messenger, "alice", "k-s1", sys.SyscallSend, `{"to":"bob","payload":1}`)
	ipcDispatch(t, messenger, "alice", "k-s1", sys.SyscallSend, `{"to":"bob","payload":1}`) // crash-retry: same key
	ipcDispatch(t, messenger, "alice", "k-s2", sys.SyscallSend, `{"to":"bob","payload":2}`)

	first := ipcDispatch(t, messenger, "bob", "k-r1", sys.SyscallRecv, "")
	// Crash-retry of the same receive re-reads the same message…
	retried := ipcDispatch(t, messenger, "bob", "k-r1", sys.SyscallRecv, "")
	if string(first.Result()) != string(retried.Result()) {
		t.Fatalf("retried recv changed message: %s vs %s", first.Result(), retried.Result())
	}
	// …and a new receive gets the next one, in send order.
	second := ipcDispatch(t, messenger, "bob", "k-r2", sys.SyscallRecv, "")

	var one, two Message
	if err := json.Unmarshal(first.Result(), &one); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	if err := json.Unmarshal(second.Result(), &two); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if string(one.Payload) != "1" || string(two.Payload) != "2" {
		t.Fatalf("delivery order = %s, %s; want 1, 2", one.Payload, two.Payload)
	}
	// The duplicated send delivered exactly once: nothing remains.
	if third := ipcDispatch(t, messenger, "bob", "k-r3", sys.SyscallRecv, ""); third.Status() != sys.StatusYield {
		t.Fatalf("retried send double-delivered: %#v", third)
	}
}

// Under the replay layer: a replayed sender never re-sends, a replayed
// receiver re-reads its journaled message without touching the mailbox.
func TestIPCUnderReplay(t *testing.T) {
	mailbox := NewMemMailbox[string]()
	messenger := newMessenger(mailbox)

	chain := func(t *testing.T, journal *memJournal, run string) sys.Dispatcher[testPID] {
		t.Helper()
		tape, err := journaled.NewTape(journal, journaled.Header{ABI: sys.ABIVersion, Program: "sha256:test", Run: run})
		if err != nil {
			t.Fatalf("new tape: %v", err)
		}
		return replay.NewDispatcher[testPID](tape, messenger)
	}
	send := sys.Syscall{Abi: sys.ABIVersion, Name: sys.SyscallSend, Args: json.RawMessage(`{"to":"bob","payload":"hi"}`)}
	recv := sys.Syscall{Abi: sys.ABIVersion, Name: sys.SyscallRecv}

	aliceJournal, bobJournal := &memJournal{}, &memJournal{}
	if _, err := chain(t, aliceJournal, "alice").Dispatch(context.Background(), testPID{id: "alice"}, send, sys.Authorization{}); err != nil {
		t.Fatalf("send: %v", err)
	}
	first, err := chain(t, bobJournal, "bob").Dispatch(context.Background(), testPID{id: "bob"}, recv, sys.Authorization{})
	if err != nil {
		t.Fatalf("recv: %v", err)
	}

	// Crash both hosts; replay both runs.
	if _, err := chain(t, aliceJournal, "alice").Dispatch(context.Background(), testPID{id: "alice"}, send, sys.Authorization{}); err != nil {
		t.Fatalf("replayed send: %v", err)
	}
	replayed, err := chain(t, bobJournal, "bob").Dispatch(context.Background(), testPID{id: "bob"}, recv, sys.Authorization{})
	if err != nil {
		t.Fatalf("replayed recv: %v", err)
	}
	if string(replayed.Result()) != string(first.Result()) {
		t.Fatalf("replayed recv diverged: %s vs %s", replayed.Result(), first.Result())
	}
	// The replayed send delivered nothing new.
	if extra := ipcDispatch(t, messenger, "bob", "k-extra", sys.SyscallRecv, ""); extra.Status() != sys.StatusYield {
		t.Fatalf("replayed send duplicated a delivery: %#v", extra)
	}
	for _, journal := range []*memJournal{aliceJournal, bobJournal} {
		if err := journaled.Verify(journal); err != nil {
			t.Fatalf("verify: %v", err)
		}
	}
}

func TestIPCRequiresIdempotencyKey(t *testing.T) {
	messenger := newMessenger(NewMemMailbox[string]())
	_, err := messenger.Dispatch(context.Background(), testPID{id: "alice"},
		sys.Syscall{Abi: sys.ABIVersion, Name: sys.SyscallSend, Args: json.RawMessage(`{"to":"bob"}`)}, sys.Authorization{})
	if err == nil || !strings.Contains(err.Error(), "idempotency key") {
		t.Fatalf("err = %v, want the below-replay requirement", err)
	}
}
