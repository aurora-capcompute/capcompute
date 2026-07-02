package capcompute

import (
	"context"
	"sync"
	"time"

	"github.com/aurora-capcompute/capcompute/sys"
)

// Throttle is the syscalls-per-second half of aggregate resource control
// (CHALLENGE B, M2.2): a per-key token bucket in front of the dispatcher
// chain. It only ever *delays* — it never denies — because a wall-clock-
// dependent refusal would be guest-visible nondeterminism, while a delay is
// invisible to a guest that has no ambient clock. Backpressure, like the
// scheduler's quotas.
//
// KeyOf picks the accounting bucket: cred→PID rate-limits each run,
// cred→tenant rate-limits an owner's aggregate.
type Throttle[K any] struct {
	next  sys.Dispatcher[K]
	rate  float64 // tokens per second
	burst float64
	keyOf func(cred K) string

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

// NewThrottle wraps next so each key's syscall rate is bounded to rate/sec
// with the given burst (minimum 1). A rate <= 0 disables throttling.
func NewThrottle[K any](rate float64, burst float64, keyOf func(cred K) string, next sys.Dispatcher[K]) *Throttle[K] {
	if burst < 1 {
		burst = 1
	}
	return &Throttle[K]{
		next:    next,
		rate:    rate,
		burst:   burst,
		keyOf:   keyOf,
		now:     time.Now,
		sleep:   sleepContext,
		buckets: make(map[string]*bucket),
	}
}

func (t *Throttle[K]) Dispatch(ctx context.Context, cred K, syscall sys.Syscall, auth sys.Authorization) (sys.SyscallResult, error) {
	if t.rate > 0 {
		if wait := t.reserve(t.keyOf(cred)); wait > 0 {
			if err := t.sleep(ctx, wait); err != nil {
				return sys.SyscallResult{}, err
			}
		}
	}
	return t.next.Dispatch(ctx, cred, syscall, auth)
}

func (t *Throttle[K]) Capabilities() []sys.Capability {
	return t.next.Capabilities()
}

// Forget releases a key's bucket once its runs are gone.
func (t *Throttle[K]) Forget(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.buckets, key)
}

// reserve takes one token, going into debt if none is available, and returns
// how long the caller must wait to honor the debt. Reserving before waiting
// keeps arrival order: later calls queue behind earlier debt.
func (t *Throttle[K]) reserve(key string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	b, ok := t.buckets[key]
	if !ok {
		b = &bucket{tokens: t.burst, last: now}
		t.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * t.rate
	if b.tokens > t.burst {
		b.tokens = t.burst
	}
	b.last = now

	b.tokens--
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / t.rate * float64(time.Second))
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
