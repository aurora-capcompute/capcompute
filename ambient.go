package capcompute

import (
	"github.com/tetratelabs/wazero"
	wzsys "github.com/tetratelabs/wazero/sys"
)

// The processor owns every ambient source a guest can observe (law #1:
// no ambient authority; law #2: all nondeterminism flows through syscalls).
// WASI preview1 exposes clock_time_get and random_get to every guest, so the
// processor pins them to deterministic sources that restart identically on each
// fresh instance — a crash-replay therefore observes the same sequence.
// Real time or randomness, if a guest ever needs it, must be a journaled
// capability, never an ambient read.

// guestEpochSec is the fixed wall-clock origin guests observe
// (2022-01-01T00:00:00Z), advancing one millisecond per read.
const guestEpochSec int64 = 1640995200

const guestClockStepNanos int64 = 1_000_000 // 1ms per read

// randSeed is the fixed xorshift64* seed every instance's RNG starts from.
const randSeed uint64 = 0x9E3779B97F4A7C15

// ambientSources are the pinned clock and RNG backing one instance's WASI
// clock_time_get / random_get. The processor holds them so a resume can reset them
// (see reset) before re-executing on a warm instance.
type ambientSources struct {
	clock *deterministicClock
	rand  *deterministicRand
}

// reset returns the pinned sources to their instance-creation state. A yielded
// process resumes by re-executing its entrypoint from the top, and the
// scheduler keeps its wazero instance warm — so the clock/RNG counters have
// already advanced from the earlier quantum. Left as-is, the re-executed prefix
// would observe a different ambient sequence than it did the first time and
// diverge from the journal (law #2). Resetting makes a warm resume
// observe exactly the sequence a cold replay on a fresh instance would.
func (a *ambientSources) reset() {
	if a == nil {
		return
	}
	a.clock.wallReads = 0
	a.clock.nanoReads = 0
	a.rand.state = randSeed
}

// guestModuleConfig builds the module configuration for one process instance.
// It is constructed fresh per instance and never caller-supplied: no
// environment, no args, pinned deterministic clock and RNG. It returns the
// pinned sources alongside so the processor can reset them on each resume.
func guestModuleConfig() (wazero.ModuleConfig, *ambientSources) {
	clock := &deterministicClock{}
	rand := &deterministicRand{state: randSeed}

	config := wazero.NewModuleConfig().
		WithWalltime(clock.walltime, wzsys.ClockResolution(guestClockStepNanos)).
		WithNanotime(clock.nanotime, wzsys.ClockResolution(guestClockStepNanos)).
		WithRandSource(rand)
	return config, &ambientSources{clock: clock, rand: rand}
}

// deterministicClock backs the WASI clock a guest can reach. Both readings
// advance one guestClockStepNanos per read from a fixed origin, so a fresh
// instance (including a crash-replay) observes the identical sequence.
type deterministicClock struct {
	wallReads, nanoReads int64
}

func (c *deterministicClock) walltime() (int64, int32) {
	nanos := c.wallReads * guestClockStepNanos
	c.wallReads++
	return guestEpochSec + nanos/1_000_000_000, int32(nanos % 1_000_000_000)
}

func (c *deterministicClock) nanotime() int64 {
	nanos := c.nanoReads * guestClockStepNanos
	c.nanoReads++
	return nanos
}

// deterministicRand is a fixed-seed xorshift64* stream backing WASI
// random_get. Each instance starts from the same seed, so replayed guests
// read identical bytes.
type deterministicRand struct {
	state uint64
}

func (r *deterministicRand) Read(p []byte) (int, error) {
	for i := range p {
		r.state ^= r.state >> 12
		r.state ^= r.state << 25
		r.state ^= r.state >> 27
		p[i] = byte((r.state * 0x2545F4914F6CDD1D) >> 56)
	}
	return len(p), nil
}
