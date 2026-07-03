package sched

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/aurora-capcompute/capcompute"
)

// Strategy is the OTP restart strategy, adapted to durable cooperative processes:
// "restarting" a sibling means stopping its quantum (the kernel kills the
// guest; the journal survives) and resubmitting it — replay reconstructs it
// exactly, so a restart loses no committed work.
type Strategy int

const (
	// OneForOne restarts only the failed child.
	OneForOne Strategy = iota
	// OneForAll stops and restarts every supervised sibling too.
	OneForAll
	// RestForOne stops and restarts the siblings declared after the failed
	// child, preserving declaration order semantics.
	RestForOne
)

// ChildSpec declares one supervised child. Order matters for RestForOne.
type ChildSpec[ID comparable] struct {
	PID      ID
	Owner    string
	Priority Priority
}

// SupervisorConfig wires a Supervisor over a Scheduler.
type SupervisorConfig[ID comparable, K capcompute.PID[ID]] struct {
	Scheduler *Scheduler[ID, K]
	Strategy  Strategy
	// MaxRestarts within Window is the restart intensity, counted across the
	// whole supervisor (the OTP rule). Exceeding it gives up: the supervisor
	// stops restarting and escalates through OnExit. MaxRestarts 0 means
	// give up on the first failure.
	MaxRestarts int
	Window      time.Duration
	// OnExit reports a child leaving supervision: completed or yielded
	// (normal — a yielded process parks until the app wakes it), stopped from
	// outside, or failed after the supervisor gave up. Optional.
	OnExit func(pid ID, result capcompute.ResumeResult[K])
}

// Supervisor watches a set of children on a scheduler and restarts failures
// per its strategy, within its restart intensity. Failures burn intensity;
// sibling restarts triggered by a strategy do not.
type Supervisor[ID comparable, K capcompute.PID[ID]] struct {
	config SupervisorConfig[ID, K]
	now    func() time.Time

	mu       sync.Mutex
	order    []ChildSpec[ID] // declaration order, for RestForOne
	watching map[ID]ChildSpec[ID]
	stopping map[ID]bool // siblings we stopped on purpose: resubmit on arrival
	restarts []time.Time
	gaveUp   bool
	closed   bool
	watchers sync.WaitGroup
}

// Supervise submits every child and starts watching. Children must not be
// already scheduled.
func Supervise[ID comparable, K capcompute.PID[ID]](ctx context.Context, config SupervisorConfig[ID, K], children []ChildSpec[ID]) (*Supervisor[ID, K], error) {
	if config.Scheduler == nil {
		return nil, errors.New("sched: supervisor requires a scheduler")
	}
	if config.MaxRestarts > 0 && config.Window <= 0 {
		return nil, errors.New("sched: restart intensity requires a window")
	}
	supervisor := &Supervisor[ID, K]{
		config:   config,
		now:      time.Now,
		order:    append([]ChildSpec[ID](nil), children...),
		watching: make(map[ID]ChildSpec[ID], len(children)),
		stopping: make(map[ID]bool),
	}
	for _, child := range children {
		supervisor.mu.Lock()
		err := supervisor.start(ctx, child)
		supervisor.mu.Unlock()
		if err != nil {
			supervisor.Close()
			return nil, err
		}
	}
	return supervisor, nil
}

// Close stops supervision and every still-supervised child, then waits for
// the watchers to drain.
func (s *Supervisor[ID, K]) Close() {
	s.mu.Lock()
	s.closed = true
	pids := make([]ID, 0, len(s.watching))
	for pid := range s.watching {
		pids = append(pids, pid)
	}
	s.mu.Unlock()
	for _, pid := range pids {
		s.config.Scheduler.Stop(pid)
	}
	s.watchers.Wait()
}

// Watching reports how many children are currently supervised.
func (s *Supervisor[ID, K]) Watching() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.watching)
}

// start submits one child and spawns its watcher. Caller holds the lock.
func (s *Supervisor[ID, K]) start(ctx context.Context, child ChildSpec[ID]) error {
	results, err := s.config.Scheduler.Submit(ctx, child.PID, child.Owner, child.Priority)
	if err != nil {
		return err
	}
	s.watching[child.PID] = child
	s.watchers.Add(1)
	go func() {
		defer s.watchers.Done()
		s.handle(ctx, child, <-results)
	}()
	return nil
}

func (s *Supervisor[ID, K]) handle(ctx context.Context, child ChildSpec[ID], result capcompute.ResumeResult[K]) {
	s.mu.Lock()

	if s.stopping[child.PID] {
		// A sibling we stopped as part of a strategy restart: resubmit it
		// without charging intensity — the triggering failure already paid.
		delete(s.stopping, child.PID)
		delete(s.watching, child.PID)
		if !s.closed && !s.gaveUp {
			if err := s.start(ctx, child); err == nil {
				s.mu.Unlock()
				return
			}
		}
		s.mu.Unlock()
		s.exit(child.PID, result)
		return
	}

	delete(s.watching, child.PID)

	if result.Status != capcompute.ResumeFailed || s.closed || s.gaveUp {
		// Completed and yielded processes exit normally (a yielded process parks until
		// the app wakes it — crashes are what supervision is for); external
		// stops and post-give-up failures exit too.
		s.mu.Unlock()
		s.exit(child.PID, result)
		return
	}

	if !s.chargeRestart() {
		s.gaveUp = true
		s.mu.Unlock()
		s.exit(child.PID, result)
		return
	}

	// Strategy: stop the siblings the strategy names; they resubmit
	// themselves when their stopped results arrive.
	var stop []ID
	switch s.config.Strategy {
	case OneForAll:
		for pid := range s.watching {
			stop = append(stop, pid)
		}
	case RestForOne:
		after := false
		for _, sibling := range s.order {
			if sibling.PID == child.PID {
				after = true
				continue
			}
			if after {
				if _, watched := s.watching[sibling.PID]; watched {
					stop = append(stop, sibling.PID)
				}
			}
		}
	}
	for _, pid := range stop {
		s.stopping[pid] = true
	}
	restartErr := s.start(ctx, child)
	s.mu.Unlock()

	for _, pid := range stop {
		s.config.Scheduler.Stop(pid)
	}
	if restartErr != nil {
		s.exit(child.PID, result)
	}
}

// chargeRestart applies the intensity rule. Caller holds the lock.
func (s *Supervisor[ID, K]) chargeRestart() bool {
	now := s.now()
	recent := s.restarts[:0]
	for _, at := range s.restarts {
		if now.Sub(at) <= s.config.Window {
			recent = append(recent, at)
		}
	}
	s.restarts = recent
	if len(s.restarts) >= s.config.MaxRestarts {
		return false
	}
	s.restarts = append(s.restarts, now)
	return true
}

func (s *Supervisor[ID, K]) exit(pid ID, result capcompute.ResumeResult[K]) {
	if s.config.OnExit != nil {
		s.config.OnExit(pid, result)
	}
}
