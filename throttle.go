package capcompute

import (
	"context"
	"sync"
	"time"

	"github.com/aurora-capcompute/capcompute/sys"
)

// RateLimit is the cross-process token-bucket state behind syscall throttling —
// shared by all of a host's per-process chains (Stack holds one), while the
// Throttle decorator that consumes it is wired per process. It only ever *delays*
// — never denies — because a wall-clock-dependent refusal would be
// guest-visible nondeterminism, while a delay is invisible to a guest that
// has no ambient clock. Backpressure, like the scheduler's quotas.
type RateLimit struct {
	rate  float64 // tokens per second; <= 0 disables throttling
	burst float64

	// now/sleep are the clock seam, injectable in tests.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimit bounds each key to rate tokens/sec with the given burst
// (minimum 1). A rate <= 0 disables throttling.
func NewRateLimit(rate float64, burst float64) *RateLimit {
	if burst < 1 {
		burst = 1
	}
	return &RateLimit{
		rate:    rate,
		burst:   burst,
		now:     time.Now,
		sleep:   sleepContext,
		buckets: make(map[string]*bucket),
	}
}

// Forget releases a key's bucket once its processes are gone.
func (l *RateLimit) Forget(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

// reserve takes one token, going into debt if none is available, and returns
// how long the caller must wait to honor the debt. Reserving before waiting
// keeps arrival order: later calls queue behind earlier debt.
func (l *RateLimit) reserve(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now

	b.tokens--
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / l.rate * float64(time.Second))
}

// Throttle applies a shared RateLimit in front of one process's dispatcher chain
// (CHALLENGE B, M2.2 — the syscalls-per-second half of aggregate resource
// control). KeyOf picks the accounting bucket: cred→PID rate-limits each process,
// cred→tenant rate-limits an owner's aggregate.
type Throttle[K any] struct {
	limit *RateLimit
	keyOf func(cred K) string
	next  sys.Dispatcher[K]
}

func NewThrottle[K any](limit *RateLimit, keyOf func(cred K) string, next sys.Dispatcher[K]) *Throttle[K] {
	return &Throttle[K]{limit: limit, keyOf: keyOf, next: next}
}

func (t *Throttle[K]) Dispatch(ctx context.Context, cred K, syscall sys.Syscall, auth sys.Authorization) (sys.SyscallResult, error) {
	if t.limit != nil && t.limit.rate > 0 {
		if wait := t.limit.reserve(t.keyOf(cred)); wait > 0 {
			if err := t.limit.sleep(ctx, wait); err != nil {
				return sys.SyscallResult{}, err
			}
		}
	}
	return t.next.Dispatch(ctx, cred, syscall, auth)
}

func (t *Throttle[K]) Capabilities() []sys.Capability {
	return t.next.Capabilities()
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
