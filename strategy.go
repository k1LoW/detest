package detest

import (
	"fmt"
	"math/rand/v2"
)

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
// DepthFirst. Each run draws from its own generator, derived from seed and
// the run's index, so the runs drawn and the violation reported do not
// depend on Workers. Shard splits the runs rather than the tree: shard index
// of total makes the runs whose index leaves index when divided by total,
// and depth is not used. Of DepthFirst and Random, the one passed last
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

// rngFor returns the generator of the shard's random run with the given
// index. A shard takes every total-th run of the sequence one machine would
// make, so the shards together make those runs, none twice.
func (s *Sim) rngFor(index int) *rand.Rand {
	if s.shardTotal > 1 {
		index = index*s.shardTotal + s.shardIndex
	}
	return rand.New(rand.NewPCG(s.strategy.seed, uint64(index))) //nolint:gosec // schedules, not secrets
}

// A random run replays no prefix, so the fingerprint check a replayed prefix
// gives depth-first exploration is made against the worker's earlier runs:
// two runs that made the same picks up to a choice must arrive at it with the
// same operations. Only the shallow choices are kept, which runs keep
// revisiting, and only so many, so that the table stays small however long
// the exploration runs.
const (
	seenDepth = 12
	seenMax   = 1 << 16
)

type seenChoice struct {
	n  int
	fp uint64
}

// seenChoices maps the hash of the picks that lead to a choice to the choice.
type seenChoices map[uint64]seenChoice

func (r *run) checkSeen(label string, n int, fp uint64) {
	if r.pos >= seenDepth {
		return
	}
	c, ok := r.seen[r.path]
	if !ok {
		if len(r.seen) < seenMax {
			r.seen[r.path] = seenChoice{n: n, fp: fp}
		}
		return
	}
	if c.n != n || c.fp != fp {
		r.pending = &violation{kind: "fatal", err: fmt.Errorf("detest: the simulation is nondeterministic: the operations before choice %d (%s) differ from an earlier run's that made the same picks up to there. Look for what varies between runs of the code under test: state a seed does not reset, map iteration order, the wall clock, randomness, or goroutines detest does not schedule", r.pos, label)}
	}
}
