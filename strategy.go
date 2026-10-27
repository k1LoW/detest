package detest

import (
	"fmt"
	"math/rand/v2"
)

// strategy is how Explore picks the schedules it runs, set by DepthFirst,
// IterativeBounding, Random or Prioritized.
type strategy struct {
	random      bool // the runs are drawn from seed, under Random or Prioritized
	seed        uint64
	prioritized bool
	depth       int  // under Prioritized, the PCT depth
	iterative   bool // IterativeBounding
}

func (st strategy) String() string {
	if st.prioritized {
		return fmt.Sprintf("prioritized seed %d at depth %d", st.seed, st.depth)
	}
	return fmt.Sprintf("random seed %d", st.seed)
}

// DepthFirst makes Explore enumerate the schedules depth first, which is the
// default. An exploration that completes means no schedule within the bounds
// breaks an invariant. Of DepthFirst, IterativeBounding, Random and
// Prioritized, the one passed last applies.
func DepthFirst() Option { return func(s *Sim) { s.strategy = strategy{} } }

// IterativeBounding makes Explore enumerate the schedules in order of the
// preemptions they spend, as iterative context bounding (Musuvathi and
// Qadeer, "Iterative Context Bounding for Systematic Testing of
// Multithreaded Programs", PLDI 2007) does. It makes every run with no
// preemption, then every run with one, and so on up to MaxPreemptions, or
// without end when it is not set. Depth first, the last choices of a run are varied
// first, so a race between its first steps is tried only after the whole
// tree below the earlier order, which an exploration cut by MaxRuns or
// MaxDuration may never reach however few preemptions the race needs. Most
// bugs need few preemptions, and under IterativeBounding they are found
// early wherever in the run the preemptions fall.
//
// It explores the tree once per number of preemptions, each pass from the
// root and depth first, as CHESS does. A pass leaves out the subtrees that
// spend more preemptions than its number, and makes again the runs of
// fewer that lead to its own, reporting nothing they break a second time. Past
// the choices a subtree fixes, a run takes the first option that preempts
// nothing, rather than the first option, so that it spends what the
// subtree spends. Its memory and its checkpoint stay those of one
// depth-first pass, and the runs made again are a small share of the runs,
// since each number of preemptions has many more runs than every smaller
// one together. The summary line gives how many there were, and neither
// Runs nor MaxRuns counts them.
//
// Without PartialOrder, a complete exploration makes the same runs as
// DepthFirst, and one cut short is saved to a checkpoint that resumes it in
// the same pass. One cut short reports the number of preemptions up to
// which it made every run. The violation reported is the first one depth
// first among the runs of the fewest preemptions, which may be another
// than the one DepthFirst reports. Workers wait for a pass
// to finish before they start the next.
//
// With PartialOrder each pass is a reduced exploration under its number of
// preemptions as the bound, rather than a pass that leaves out the subtrees
// costing more, since the reduction's backtrack points and sleep sets hold
// for the bound the options were cut at. Nothing of the reduction is
// carried from one pass to the next. A run may then spend preemptions past
// the choices its subtree fixes, and the ones of fewer than the pass's are
// made again. The schedules it prints carry "bound:" and the pass's number
// after "reads:", and replay under that bound with or without either
// option. Of
// DepthFirst, IterativeBounding, Random and Prioritized, the one passed
// last applies.
func IterativeBounding() Option { return func(s *Sim) { s.strategy = strategy{iterative: true} } }

// Random makes Explore run schedules whose every choice is drawn uniformly
// from a generator seeded with seed, until MaxRuns or MaxDuration stops it.
// It scales to schedule trees too large for a depth-first search to get past
// the first choices, at the cost of saying nothing about the schedules it
// did not draw, so the exploration is never complete. A violation is
// reported with its schedule and replays with DETEST_REPLAY as under
// DepthFirst. Each run draws from its own generator, derived from seed and
// the run's index, so the schedule of a run does not depend on Workers, nor
// does the violation reported when the exploration stops at it or at
// MaxRuns. MaxDuration ends it after however many runs the workers made by
// then, which differs from one execution to the next. Shard splits the runs
// rather than the tree: shard index of total makes the runs whose index
// leaves index when divided by total, and depth is not used. Of DepthFirst,
// IterativeBounding, Random and Prioritized, the one passed last applies.
// DETEST_SEED overrides seed, and leaves a test under DepthFirst or
// IterativeBounding as it is.
func Random(seed uint64) Option {
	return func(s *Sim) { s.strategy = strategy{random: true, seed: seed} }
}

// Prioritized makes Explore run schedules drawn from seed as Random does,
// but picks them the way PCT (probabilistic concurrency testing, Burckhardt
// et al., "A Randomized Scheduler with Probabilistic Guarantees of Finding
// Bugs", ASPLOS 2010) does. Each process gets a priority drawn at random,
// the highest runs, and at depth-1 steps drawn at random the process that
// ran last drops below the others. A run therefore switches between
// processes at depth-1 places and runs each process on otherwise, where
// Random switches at almost every step and rarely lets a process go on for
// long. A bug that needs depth orderings between operations, such as a read
// by one process between another's read and write (depth 2), is found by
// each run with a probability of at least 1/(n*k^(depth-1)) for n processes
// and k steps, a bound Random does not have for long runs. Most concurrency
// bugs need a depth of 1 to 3.
//
// It differs from PCT where detest differs from its model. A process
// created by starting a process type or delivering a message takes the
// priority its option drew when it appeared, and the option draws again for
// the next process it makes, such as a redelivery. k is the length of a run
// that picks by priority with no switch and no fault, drawn from seed, which
// each worker makes alike once before it starts and which reports nothing of
// what it finds. Crashes and losses happen at steps drawn among the first k
// too, and the other choices, such as an external call's outcome, are drawn
// as under Random. The bound holds for the order of processes only, and for
// runs no longer than k. A switch is a preemption, so a MaxPreemptions below
// depth-1 leaves switches unused.
//
// Everything else is as under Random: the exploration is never complete,
// violations replay with DETEST_REPLAY, Workers and Shard split the runs,
// and DETEST_SEED overrides seed. depth is at least 1, which switches
// nowhere. Of DepthFirst, IterativeBounding, Random and Prioritized, the
// one passed last applies.
func Prioritized(seed uint64, depth int) Option {
	return func(s *Sim) { s.strategy = strategy{random: true, seed: seed, prioritized: true, depth: depth} }
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

// measuringRng returns the generator of the run Prioritized measures k on,
// the same in every worker and shard, and apart from every run's.
func (s *Sim) measuringRng() *rand.Rand {
	return rand.New(rand.NewPCG(s.strategy.seed, ^uint64(0))) //nolint:gosec // schedules, not secrets
}

// A random run replays no prefix, so the fingerprint check a replayed prefix
// gives depth-first exploration is made against the worker's earlier runs:
// two runs that made the same picks up to a choice must arrive at it with the
// same operations. Only the shallow choices are kept, which runs keep
// revisiting, and only so many, so that the table stays small however long
// the exploration runs. Under ExploreAfter the choices before its state and
// as many after it are kept, as the runs branch only from there.
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
	if r.pos-r.leadEnd >= seenDepth {
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
