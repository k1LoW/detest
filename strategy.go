package detest

import "math/rand/v2"

// strategy is how Explore picks the schedules it runs, set by DepthFirst or
// Random.
type strategy struct {
	random bool
	seed   uint64
}

// DepthFirst makes Explore enumerate the schedules depth first, which is the
// default. An exploration that completes means no schedule within the bounds
// breaks an invariant. Of DepthFirst and Random, the one passed last applies.
func DepthFirst() Option { return func(s *Sim) { s.strategy = strategy{} } }

// Random makes Explore run schedules whose every choice is drawn uniformly
// from a generator seeded with seed, until MaxRuns or MaxDuration stops it.
// It scales to schedule trees too large for a depth-first search to get past
// the first choices, at the cost of saying nothing about the schedules it
// did not draw, so the exploration is never complete. A violation is
// reported with its schedule and replays with DETEST_REPLAY as under
// DepthFirst. Each run draws from its own generator, derived from seed, the
// shard and the run's index, so the runs drawn and the violation reported do
// not depend on Workers. Of DepthFirst and Random, the one passed last
// applies.
func Random(seed uint64) Option {
	return func(s *Sim) { s.strategy = strategy{random: true, seed: seed} }
}

func strategyOf(opts []Option) strategy {
	probe := &Sim{}
	for _, o := range opts {
		o(probe)
	}
	return probe.strategy
}

// rngFor returns the generator of the random run with the given index.
func (s *Sim) rngFor(index int) *rand.Rand {
	return rand.New(rand.NewPCG(s.strategy.seed, uint64(s.shardIndex)<<32^uint64(index))) //nolint:gosec // schedules, not secrets
}
