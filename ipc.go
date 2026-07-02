package capcompute

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/aurora-capcompute/capcompute/sys"
)

// Message is one inter-process message. Capabilities carries the names the
// sender delegates with it — attenuated at send (a sender cannot pass what it
// does not hold); how the receiver's grant table absorbs them is app policy.
type Message struct {
	From         string          `json:"from"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	Capabilities []string        `json:"capabilities,omitempty"`
}

// Mailbox is the app-supplied durable queue between processes. Both methods
// are keyed by the calling syscall's idempotency key, which is what makes
// at-least-once dispatch safe: a retried send must not duplicate a delivery,
// and a retried receive must return the same message it consumed the first
// time. MemMailbox is the reference implementation.
type Mailbox[ID comparable] interface {
	Append(ctx context.Context, to ID, key string, message Message) error
	Receive(ctx context.Context, pid ID, key string) (Message, bool, error)
}

// SendRequest is the args payload of sys.send.
type SendRequest struct {
	To           string          `json:"to"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	Capabilities []string        `json:"capabilities,omitempty"`
}

// SendResponse is the journaled outcome of a delivery.
type SendResponse struct {
	To string `json:"to"`
}

var sendInputSchema = json.RawMessage(`{
	"type": "object",
	"required": ["to"],
	"properties": {
		"to":           {"type": "string", "minLength": 1},
		"payload":      {},
		"capabilities": {"type": "array", "items": {"type": "string"}}
	},
	"additionalProperties": false
}`)

var recvInputSchema = json.RawMessage(`{"type": "object", "additionalProperties": false}`)

// IPCConfig wires the Messenger decorator.
type IPCConfig[ID comparable, K PID[ID]] struct {
	// Grants reports the sender's capability set — the ceiling for names a
	// message may delegate.
	Grants GrantSource[K]
	// Mailbox is the durable queue.
	Mailbox Mailbox[ID]
	// ParsePID maps the wire "to" address to a PID; the app owns PID syntax.
	ParsePID func(to string) (ID, error)
	// FormatPID renders a PID as the wire "from" address. Nil = fmt.Sprint.
	FormatPID func(pid ID) string
}

// Messenger serves the reserved sys.send and sys.recv syscalls — message-
// passing IPC. It must sit *below* the replay layer: a send is then an
// effect journaled in the sender's journal (replay never re-sends), a receive
// is an input event journaled in the receiver's (replay re-reads the same
// message at the same position — delivery order is journal order, never wall
// clock), and both carry idempotency keys so crash-retries neither duplicate
// a delivery nor skip a message. A receive on an empty mailbox yields: the
// run parks until the app wakes it on delivery, the same protocol as any
// pending external task.
type Messenger[ID comparable, K PID[ID]] struct {
	config IPCConfig[ID, K]
	next   sys.Dispatcher[K]
}

func NewMessenger[ID comparable, K PID[ID]](config IPCConfig[ID, K], next sys.Dispatcher[K]) *Messenger[ID, K] {
	if config.FormatPID == nil {
		config.FormatPID = func(pid ID) string { return fmt.Sprint(pid) }
	}
	return &Messenger[ID, K]{config: config, next: next}
}

func (m *Messenger[ID, K]) Dispatch(ctx context.Context, cred K, syscall sys.Syscall, auth sys.Authorization) (sys.SyscallResult, error) {
	switch syscall.Name {
	case sys.SyscallSend:
		return m.send(ctx, cred, syscall)
	case sys.SyscallRecv:
		return m.receive(ctx, cred)
	default:
		return m.next.Dispatch(ctx, cred, syscall, auth)
	}
}

func (m *Messenger[ID, K]) send(ctx context.Context, cred K, syscall sys.Syscall) (sys.SyscallResult, error) {
	var request SendRequest
	if err := json.Unmarshal(syscall.Args, &request); err != nil {
		return sys.FailCode(sys.ErrnoInvalidArgs, fmt.Sprintf("decode send args: %v", err)), nil
	}
	if request.To == "" {
		return sys.FailCode(sys.ErrnoInvalidArgs, "send: to is required"), nil
	}
	to, err := m.config.ParsePID(request.To)
	if err != nil {
		return sys.FailCode(sys.ErrnoInvalidArgs, fmt.Sprintf("send: bad address %q: %v", request.To, err)), nil
	}

	// Delegation law: a message may only carry capabilities the sender holds.
	senderGrants := m.config.Grants(cred)
	requested := make([]sys.Capability, 0, len(request.Capabilities))
	for _, name := range request.Capabilities {
		granted, ok := findCapability(senderGrants, name)
		if !ok {
			return sys.FailCode(sys.ErrnoDenied, fmt.Sprintf("send: sender does not hold capability %q", name)), nil
		}
		requested = append(requested, granted)
	}
	if _, err := sys.Attenuate(senderGrants, requested); err != nil {
		return sys.FailCode(sys.ErrnoDenied, fmt.Sprintf("send: %v", err)), nil
	}

	key, ok := sys.IdempotencyKey(ctx)
	if !ok {
		return sys.SyscallResult{}, fmt.Errorf("send: no idempotency key in context; Messenger must run below the replay layer")
	}
	if err := m.config.Mailbox.Append(ctx, to, key, Message{
		From:         m.config.FormatPID(cred.PID()),
		Payload:      request.Payload,
		Capabilities: request.Capabilities,
	}); err != nil {
		return sys.SyscallResult{}, fmt.Errorf("send: append to mailbox: %w", err)
	}

	result, err := json.Marshal(SendResponse{To: request.To})
	if err != nil {
		return sys.SyscallResult{}, err
	}
	return sys.Result(result), nil
}

func (m *Messenger[ID, K]) receive(ctx context.Context, cred K) (sys.SyscallResult, error) {
	key, ok := sys.IdempotencyKey(ctx)
	if !ok {
		return sys.SyscallResult{}, fmt.Errorf("recv: no idempotency key in context; Messenger must run below the replay layer")
	}
	message, delivered, err := m.config.Mailbox.Receive(ctx, cred.PID(), key)
	if err != nil {
		return sys.SyscallResult{}, fmt.Errorf("recv: %w", err)
	}
	if !delivered {
		return sys.Yield("mailbox empty: waiting for a message"), nil
	}
	result, err := json.Marshal(message)
	if err != nil {
		return sys.SyscallResult{}, err
	}
	return sys.Result(result), nil
}

// Capabilities publishes sys.send and sys.recv (schema'd); whether a run may
// call them is the manifest's decision, enforced by the Validator like any
// capability.
func (m *Messenger[ID, K]) Capabilities() []sys.Capability {
	return append(m.next.Capabilities(),
		sys.Capability{
			Name:        sys.SyscallSend,
			Description: "send a message (and optionally delegate held capabilities) to another process; delivered in journal order",
			InputSchema: sendInputSchema,
		},
		sys.Capability{
			Name:        sys.SyscallRecv,
			Description: "receive the next message for this process; blocks (yields) while the mailbox is empty",
			InputSchema: recvInputSchema,
		},
	)
}

// MemMailbox is the in-memory reference Mailbox — for tests and prototyping.
// Production supplies a durable implementation with the same key semantics.
type MemMailbox[ID comparable] struct {
	mu       sync.Mutex
	queues   map[ID][]Message
	appended map[string]struct{} // send keys already delivered
	consumed map[string]*Message // recv keys already served
}

func NewMemMailbox[ID comparable]() *MemMailbox[ID] {
	return &MemMailbox[ID]{
		queues:   make(map[ID][]Message),
		appended: make(map[string]struct{}),
		consumed: make(map[string]*Message),
	}
}

func (m *MemMailbox[ID]) Append(_ context.Context, to ID, key string, message Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, duplicate := m.appended[key]; duplicate {
		return nil // a retried send delivers once
	}
	m.appended[key] = struct{}{}
	m.queues[to] = append(m.queues[to], message)
	return nil
}

func (m *MemMailbox[ID]) Receive(_ context.Context, pid ID, key string) (Message, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if served, retried := m.consumed[key]; retried {
		return *served, true, nil // a retried receive re-reads its message
	}
	queue := m.queues[pid]
	if len(queue) == 0 {
		return Message{}, false, nil
	}
	next := queue[0]
	m.queues[pid] = queue[1:]
	m.consumed[key] = &next
	return next, true, nil
}
