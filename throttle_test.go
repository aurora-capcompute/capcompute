package capcompute

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aurora-capcompute/capcompute/sys"
)

// throttleClock is a manual clock: time advances only when the throttle
// "sleeps", so the tests are exact and instant.
type throttleClock struct {
	now    time.Time
	sleeps []time.Duration
}

func newThrottled(rate, burst float64) (*Throttle[testPID], *throttleClock, *recordingDispatcher) {
	next := &recordingDispatcher{}
	throttle := NewThrottle(rate, burst, func(cred testPID) string { return cred.id }, next)
	clock := &throttleClock{now: time.Unix(1_700_000_000, 0)}
	throttle.now = func() time.Time { return clock.now }
	throttle.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		clock.sleeps = append(clock.sleeps, d)
		clock.now = clock.now.Add(d)
		return nil
	}
	return throttle, clock, next
}

func throttledCall(t *testing.T, throttle *Throttle[testPID], ctx context.Context, pid string) error {
	t.Helper()
	_, err := throttle.Dispatch(ctx, testPID{id: pid}, sys.Syscall{Abi: sys.ABIVersion, Name: "mail.send"}, sys.Authorization{})
	return err
}

func TestThrottleBurstThenSteadyRate(t *testing.T) {
	throttle, clock, next := newThrottled(2, 3) // 2/sec, burst 3
	ctx := context.Background()

	// The burst passes without any delay…
	for i := 0; i < 3; i++ {
		if err := throttledCall(t, throttle, ctx, "p1"); err != nil {
			t.Fatalf("burst call %d: %v", i, err)
		}
	}
	if len(clock.sleeps) != 0 {
		t.Fatalf("burst slept: %v", clock.sleeps)
	}

	// …then each call pays the steady-state price: 1/rate = 500ms.
	for i := 0; i < 2; i++ {
		if err := throttledCall(t, throttle, ctx, "p1"); err != nil {
			t.Fatalf("steady call %d: %v", i, err)
		}
	}
	if len(clock.sleeps) != 2 || clock.sleeps[0] != 500*time.Millisecond || clock.sleeps[1] != 500*time.Millisecond {
		t.Fatalf("sleeps = %v, want [500ms 500ms]", clock.sleeps)
	}
	if len(next.calls) != 5 {
		t.Fatalf("delegated %d calls, want 5 (throttling delays, never denies)", len(next.calls))
	}
}

func TestThrottleKeysAreIndependent(t *testing.T) {
	throttle, clock, _ := newThrottled(1, 1)
	ctx := context.Background()

	if err := throttledCall(t, throttle, ctx, "p1"); err != nil {
		t.Fatalf("p1: %v", err)
	}
	// p1 exhausted its bucket; p2 is unaffected.
	if err := throttledCall(t, throttle, ctx, "p2"); err != nil {
		t.Fatalf("p2: %v", err)
	}
	if len(clock.sleeps) != 0 {
		t.Fatalf("independent keys slept: %v", clock.sleeps)
	}
}

func TestThrottleRefillsWithTime(t *testing.T) {
	throttle, clock, _ := newThrottled(2, 1)
	ctx := context.Background()

	if err := throttledCall(t, throttle, ctx, "p1"); err != nil {
		t.Fatalf("first: %v", err)
	}
	clock.now = clock.now.Add(time.Second) // 2 tokens accrue, capped at burst 1
	if err := throttledCall(t, throttle, ctx, "p1"); err != nil {
		t.Fatalf("after refill: %v", err)
	}
	if len(clock.sleeps) != 0 {
		t.Fatalf("refilled call slept: %v", clock.sleeps)
	}
}

func TestThrottleCancelledWhileWaiting(t *testing.T) {
	throttle, _, next := newThrottled(1, 1)
	ctx, cancel := context.WithCancel(context.Background())

	if err := throttledCall(t, throttle, ctx, "p1"); err != nil {
		t.Fatalf("first: %v", err)
	}
	cancel()
	if err := throttledCall(t, throttle, ctx, "p1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait err = %v, want context.Canceled", err)
	}
	if len(next.calls) != 1 {
		t.Fatalf("cancelled call reached the dispatcher")
	}
}

func TestThrottleDisabledPassesThrough(t *testing.T) {
	throttle, clock, next := newThrottled(0, 1)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := throttledCall(t, throttle, ctx, "p1"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if len(clock.sleeps) != 0 || len(next.calls) != 10 {
		t.Fatalf("disabled throttle interfered: sleeps=%v calls=%d", clock.sleeps, len(next.calls))
	}
}
