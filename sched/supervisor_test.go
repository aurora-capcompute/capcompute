package sched_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aurora-capcompute/capcompute"
	"github.com/aurora-capcompute/capcompute/sched"
)

func failed() capcompute.ResumeResult[schedPID] {
	return capcompute.ResumeResult[schedPID]{Status: capcompute.ResumeFailed}
}

type exits struct {
	mu   sync.Mutex
	pids []string
}

func (e *exits) record(pid string, _ capcompute.ResumeResult[schedPID]) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pids = append(e.pids, pid)
}

func (e *exits) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.pids...)
}

func supervise(t *testing.T, h *harness, config sched.SupervisorConfig[string, schedPID], children ...sched.ChildSpec[string]) *sched.Supervisor[string, schedPID] {
	t.Helper()
	supervisor, err := sched.Supervise(context.Background(), config, children)
	if err != nil {
		t.Fatalf("supervise: %v", err)
	}
	return supervisor
}

func child(pid string) sched.ChildSpec[string] {
	return sched.ChildSpec[string]{PID: pid, Owner: "acme", Priority: sched.Normal}
}

func TestSupervisorOneForOneRestartsFailure(t *testing.T) {
	h := newHarness()
	scheduler, _ := sched.New(h.config())
	defer scheduler.Close()
	logged := &exits{}

	supervisor := supervise(t, h, sched.SupervisorConfig[string, schedPID]{
		Scheduler:   scheduler,
		Strategy:    sched.OneForOne,
		MaxRestarts: 3,
		Window:      time.Minute,
		OnExit:      logged.record,
	}, child("p1"))
	defer supervisor.Close()

	h.next(t).done <- failed()    // first quantum crashes…
	h.next(t).done <- completed() // …the restart completes.
	waitFor(t, func() bool { return len(logged.list()) == 1 })
	if h.activationsOf("p1") != 2 {
		t.Fatalf("activations = %d, want 2 (restart reconstructs by replay)", h.activationsOf("p1"))
	}
	if supervisor.Watching() != 0 {
		t.Fatalf("watching = %d, want 0", supervisor.Watching())
	}
}

func TestSupervisorIntensityGivesUp(t *testing.T) {
	h := newHarness()
	scheduler, _ := sched.New(h.config())
	defer scheduler.Close()
	logged := &exits{}

	supervisor := supervise(t, h, sched.SupervisorConfig[string, schedPID]{
		Scheduler:   scheduler,
		Strategy:    sched.OneForOne,
		MaxRestarts: 1,
		Window:      time.Minute,
		OnExit:      logged.record,
	}, child("p1"))
	defer supervisor.Close()

	h.next(t).done <- failed() // charges the single allowed restart
	h.next(t).done <- failed() // the restart crashes too: intensity exceeded
	waitFor(t, func() bool { return len(logged.list()) == 1 })

	select {
	case started := <-h.running:
		t.Fatalf("quantum %s started after the supervisor gave up", started.pid)
	case <-time.After(50 * time.Millisecond):
	}
	if h.activationsOf("p1") != 2 {
		t.Fatalf("activations = %d, want 2 (no third try)", h.activationsOf("p1"))
	}
}

func TestSupervisorNormalExitsAreNotRestarted(t *testing.T) {
	h := newHarness()
	scheduler, _ := sched.New(h.config())
	defer scheduler.Close()
	logged := &exits{}

	supervisor := supervise(t, h, sched.SupervisorConfig[string, schedPID]{
		Scheduler:   scheduler,
		Strategy:    sched.OneForOne,
		MaxRestarts: 3,
		Window:      time.Minute,
		OnExit:      logged.record,
	}, child("done"), child("parked"))
	defer supervisor.Close()

	first := h.next(t)
	first.done <- completed()
	second := h.next(t)
	second.done <- yielded()
	waitFor(t, func() bool { return len(logged.list()) == 2 })

	if h.activationsOf("done")+h.activationsOf("parked") != 2 {
		t.Fatal("a normal exit was restarted")
	}
}

func TestSupervisorOneForAllRestartsSiblings(t *testing.T) {
	h := newHarness()
	config := h.config()
	config.MaxConcurrent = 2
	scheduler, _ := sched.New(config)
	defer scheduler.Close()

	supervisor := supervise(t, h, sched.SupervisorConfig[string, schedPID]{
		Scheduler:   scheduler,
		Strategy:    sched.OneForAll,
		MaxRestarts: 2,
		Window:      time.Minute,
	}, child("a"), child("b"))
	defer supervisor.Close()

	// Both run; a crashes. b must be stopped (its quantum cancelled) and both
	// must come back.
	quanta := map[string]quantum{}
	for i := 0; i < 2; i++ {
		started := h.next(t)
		quanta[started.pid] = started
	}
	quanta["a"].done <- failed()

	restarted := map[string]int{}
	for i := 0; i < 2; i++ {
		started := h.next(t)
		restarted[started.pid]++
		started.done <- completed()
	}
	if restarted["a"] != 1 || restarted["b"] != 1 {
		t.Fatalf("restarts = %v, want one each for a and b", restarted)
	}
	if h.activationsOf("b") != 2 {
		t.Fatalf("b activations = %d, want 2 (stopped and reconstructed)", h.activationsOf("b"))
	}
}

func TestSupervisorRestForOneRestartsLaterSiblingsOnly(t *testing.T) {
	h := newHarness()
	config := h.config()
	config.MaxConcurrent = 3
	scheduler, _ := sched.New(config)
	defer scheduler.Close()

	supervisor := supervise(t, h, sched.SupervisorConfig[string, schedPID]{
		Scheduler:   scheduler,
		Strategy:    sched.RestForOne,
		MaxRestarts: 2,
		Window:      time.Minute,
	}, child("a"), child("b"), child("c"))
	defer supervisor.Close()

	quanta := map[string]quantum{}
	for i := 0; i < 3; i++ {
		started := h.next(t)
		quanta[started.pid] = started
	}
	// b crashes: c restarts with it; a keeps running untouched.
	quanta["b"].done <- failed()

	restarted := map[string]int{}
	for i := 0; i < 2; i++ {
		started := h.next(t)
		restarted[started.pid]++
		started.done <- completed()
	}
	if restarted["b"] != 1 || restarted["c"] != 1 || restarted["a"] != 0 {
		t.Fatalf("restarts = %v, want b and c only", restarted)
	}
	if h.activationsOf("a") != 1 {
		t.Fatalf("a activations = %d, want 1 (undisturbed)", h.activationsOf("a"))
	}
	quanta["a"].done <- completed()
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never held")
}
