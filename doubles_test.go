package capcompute

// In-memory test doubles for the kernel's store interfaces. The kernel ships
// interfaces only (Journal, Mailbox, ProcessTable); durable implementations
// live in consumer modules, and tests supply these.

import (
	"context"
	"fmt"
	"sync"

	"github.com/aurora-capcompute/capcompute/sys/replay/tape/journaled"
)

// memJournal is an in-memory journaled.Journal.
type memJournal struct {
	header    journaled.Header
	hasHeader bool
	records   []journaled.Record
}

func newMemJournal() *memJournal { return &memJournal{} }

func (j *memJournal) Header() (journaled.Header, bool, error) { return j.header, j.hasHeader, nil }

func (j *memJournal) SetHeader(header journaled.Header) error {
	j.header = header
	j.hasHeader = true
	return nil
}

func (j *memJournal) Load(idx int) (journaled.Record, error) {
	if idx < 0 || idx >= len(j.records) {
		return journaled.Record{}, fmt.Errorf("journal: no record at %d", idx)
	}
	return j.records[idx], nil
}

func (j *memJournal) Append(record journaled.Record) error {
	j.records = append(j.records, record)
	return nil
}

func (j *memJournal) Length() int { return len(j.records) }

// memMailbox is an in-memory Mailbox honoring the interface's key semantics:
// a retried send delivers once, a retried receive re-reads its message.
type memMailbox[ID comparable] struct {
	mu       sync.Mutex
	queues   map[ID][]Message
	appended map[string]struct{} // send keys already delivered
	consumed map[string]*Message // recv keys already served
}

func newMemMailbox[ID comparable]() *memMailbox[ID] {
	return &memMailbox[ID]{
		queues:   make(map[ID][]Message),
		appended: make(map[string]struct{}),
		consumed: make(map[string]*Message),
	}
}

func (m *memMailbox[ID]) Append(_ context.Context, to ID, key string, message Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, duplicate := m.appended[key]; duplicate {
		return nil // a retried send delivers once
	}
	m.appended[key] = struct{}{}
	m.queues[to] = append(m.queues[to], message)
	return nil
}

func (m *memMailbox[ID]) Receive(_ context.Context, pid ID, key string) (Message, bool, error) {
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
