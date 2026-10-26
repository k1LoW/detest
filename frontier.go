package detest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// frontier holds the subtrees of the schedule tree no worker has explored yet,
// each as the choice prefix all of its runs share. A finished run reveals the
// unexplored siblings along its path, and every one of them is an independent
// subtree, so idle workers take them as they appear, the load balances itself
// and every run is executed once. Under IterativeBounding a pass makes again
// the runs of earlier passes that lead to its own (see IterativeBounding).
//
// Workers wait for subtrees on channels created outside every bubble. A
// sync.Cond would not do: synctest counts a goroutine in Cond.Wait as durably
// blocked and, with nothing else running in its bubble, reports a deadlock.
type frontier struct {
	lazy    string // why EagerStart was dropped, if it was (see Explore)
	mu      sync.Mutex
	wake    []chan struct{} // per worker, buffered so a wakeup is never lost
	waiting []bool
	stack   []subtree // LIFO keeps the order depth first and the stack small
	// iterative is IterativeBounding, under which the tree is explored in
	// one pass per level, each from the root and depth first, leaving out
	// the subtrees whose prefix costs more preemptions than level. higher
	// records that the pass left one out, so that the next pass has runs to
	// make, and again counts the runs made again on the way to the level's.
	iterative  bool
	level      int
	higher     bool
	again      int
	busy       int // workers running a prefix taken from the stack
	stopped    bool
	incomplete bool // MaxRuns, MaxDuration or a signal cut the exploration short
	// random hands out run indexes instead of subtrees, under the Random
	// strategy, with next the index of the next run and bestIndex the
	// index of the best violation's run. halted stops handing them out
	// once a run violated, before it is shrunk.
	random    bool
	next      int
	bestIndex int
	halted    bool
	runs      int
	maxRuns   int
	expired   atomic.Bool // MaxDuration has passed, or a signal came
	// best is the ranks of the choices (see choice.rank) of the earliest
	// violating run found so far, in the depth-first order one worker
	// explores in, with its result. Subtrees
	// after it are dropped and the ones before it still explored, so the
	// violation reported is the one a single worker would report.
	best       []int
	bestResult *result
	prior      int   // runs of the explorations a checkpoint resumes
	cuts       int   // runs cut at MaxIdleTicks, the resumed explorations' too
	fatal      error // a misuse that ends the exploration, such as a stale checkpoint
	progress   []workerProgress
	// sometimes holds the Sometimes conditions in the order declared, and
	// whether a run of any worker, or of an exploration a checkpoint resumes,
	// met each.
	sometimes []string
	reached   map[string]bool
	// refused is the statements detest refused as unsupported in any run,
	// which the application saw as an error there.
	refused map[string]bool
	// shared counts the statements goroutines ran at once on a transaction
	// they share with their process (see sqlConn.runShared).
	shared atomic.Int64
	// partial is PartialOrder, under which pushed is the subtrees pushed or
	// run so far, where the backtrack points of several runs name the same
	// subtree.
	partial bool
	pushed  map[string]*subtree
	// zIn is, by a state's prefix key, the sleep set that arrived there,
	// and explored the options run from it so far (see sleep.go).
	zIn      map[string][]sleeper
	explored map[string][]explored
	// exploreAll keeps the exploration going past violations (see
	// Sim.exploreAll).
	exploreAll bool
}

// workerProgress is what the stall watchdog reads of a worker: the steps its
// scheduler took, and whether it is idle, waiting for a subtree or done, so
// that its standing still is no stall.
type workerProgress struct {
	steps atomic.Int64
	idle  atomic.Bool
}

func newFrontier(workers, maxRuns int) *frontier {
	f := &frontier{stack: []subtree{{}}, maxRuns: maxRuns, wake: make([]chan struct{}, workers), waiting: make([]bool, workers), progress: make([]workerProgress, workers), pushed: map[string]*subtree{"": {}}, zIn: map[string][]sleeper{}, explored: map[string][]explored{}}
	for i := range f.wake {
		f.wake[i] = make(chan struct{}, 1)
	}
	return f
}

// wakeAll wakes the waiting workers. f.mu is held.
func (f *frontier) wakeAll() { f.wakeN(len(f.waiting)) }

// wakeN wakes up to n waiting workers. f.mu is held.
func (f *frontier) wakeN(n int) {
	for i, w := range f.waiting {
		if n == 0 {
			return
		}
		if w {
			f.waiting[i] = false
			n--
			select {
			case f.wake[i] <- struct{}{}:
			default:
			}
		}
	}
}

// settled wakes the workers a change of the stack concerns: one per subtree
// pushed, or all of them once nothing is left to take or wait for. f.mu is
// held.
func (f *frontier) settled(pushed int) {
	if f.busy == 0 && len(f.stack) == 0 {
		f.wakeAll()
		return
	}
	f.wakeN(pushed)
}

// take returns the next prefix to run, waiting while other workers may still
// add some, and under the Random strategy the index of the run instead. It
// reports false once the exploration is over.
func (f *frontier) take(worker int) (subtree, int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.random {
		return f.takeRandom(worker)
	}
	for {
		for len(f.stack) > 0 && f.after(f.stack[len(f.stack)-1].prefix) {
			f.stack = f.stack[:len(f.stack)-1] // after the best violation
		}
		if len(f.stack) == 0 && f.busy == 0 && !f.stopped && f.nextLevel() {
			continue
		}
		if len(f.stack) > 0 || f.busy == 0 || f.stopped {
			break
		}
		f.waiting[worker] = true
		f.progress[worker].idle.Store(true)
		f.mu.Unlock()
		<-f.wake[worker]
		f.mu.Lock()
		f.progress[worker].idle.Store(false)
	}
	if f.stopped || len(f.stack) == 0 {
		f.progress[worker].idle.Store(true)
		f.wakeAll() // the exploration is over: let the others see it
		return subtree{}, 0, false
	}
	if f.runs >= f.maxRuns || (f.runs+f.again > 0 && f.expired.Load()) {
		f.incomplete, f.stopped = true, true
		f.progress[worker].idle.Store(true)
		f.wakeAll()
		return subtree{}, 0, false
	}
	p := f.stack[len(f.stack)-1]
	f.stack = f.stack[:len(f.stack)-1]
	f.busy++
	// Under PartialOrder a run may spend preemptions past its prefix, so
	// passRun tells a run made again after it is made.
	if p.again = !f.partial && p.level < f.level; p.again {
		f.again++
	} else {
		f.runs++
	}
	return p, 0, true
}

// nextLevel starts the pass of the next level under IterativeBounding, from
// the root, once the pass before has finished and left out a subtree of a
// higher level. It reports whether it started one. f.mu is held.
func (f *frontier) nextLevel() bool {
	if !f.iterative || !f.higher || f.best != nil && !f.exploreAll {
		return false
	}
	f.level, f.higher = f.level+1, false
	f.stack = append(f.stack, subtree{})
	if f.partial {
		// The backtrack points and sleep sets of a pass hold for its bound
		// only, and the next pass explores under another. Carried over, a
		// prefix pushed under the lower bound would keep the higher one
		// from pushing it, and a sleep set from waking what the higher
		// bound makes a preemption cost.
		f.pushed, f.zIn, f.explored = map[string]*subtree{"": {}}, map[string][]sleeper{}, map[string][]explored{}
	}
	return true
}

// passRun records a run of a pass of IterativeBounding with PartialOrder,
// which spent preempts preemptions and left an option of a higher bound out
// when higher. It reports whether the run was one of an earlier pass, made
// again, which it then counts as one.
func (f *frontier) passRun(preempts int, higher bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if higher {
		f.higher = true
	}
	if preempts >= f.level {
		return false
	}
	f.runs--
	f.again++
	return true
}

// completeLevel returns, under IterativeBounding, the preemptions up to
// which every run was made, -1 when not every run without one was. f.mu
// need not be held, as every worker has returned.
func (f *frontier) completeLevel() int {
	if len(f.stack) > 0 {
		return f.level - 1
	}
	return f.level
}

// takeRandom is take under the Random strategy. Indexes are handed out in
// order, so once a run violates every run before it has been taken, and no
// other is needed to report the violation of the lowest index. f.mu
// is held.
func (f *frontier) takeRandom(worker int) (subtree, int, bool) {
	if f.stopped || f.halted {
		f.progress[worker].idle.Store(true)
		return subtree{}, 0, false
	}
	if f.runs >= f.maxRuns || (f.runs > 0 && f.expired.Load()) {
		f.incomplete, f.stopped = true, true
		f.progress[worker].idle.Store(true)
		return subtree{}, 0, false
	}
	i := f.next
	f.next++
	f.busy++
	f.runs++
	return subtree{}, i, true
}

// cut counts a run cut at MaxIdleTicks.
func (f *frontier) cut() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cuts++
}

func (f *frontier) cutRuns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cuts
}

// expire stops the exploration from starting another run.
func (f *frontier) expire() { f.expired.Store(true) }

// finish returns a taken prefix with the subtrees its run revealed.
func (f *frontier) finish(children []subtree) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pushed := f.push(children)
	f.busy--
	f.settled(pushed)
}

// push adds the subtrees not after the best violation and, under
// PartialOrder where several runs' backtrack points name one subtree, not
// pushed before. Under IterativeBounding it leaves the subtrees above the
// pass's level to a later pass. It returns how many it added. f.mu is held.
func (f *frontier) push(children []subtree) int {
	pushed := 0
	for i := range children {
		c := children[i]
		if f.partial {
			k := prefixKey(c.prefix)
			if prev := f.pushed[k]; prev != nil {
				if prev.conservative && !c.conservative {
					prev.conservative = false // the bound is not the only reason any more
				}
				continue
			}
			f.pushed[k] = &c
		}
		if f.iterative && c.level > f.level {
			f.higher = true // for a later pass
			continue
		}
		if !f.after(c.prefix) {
			f.stack = append(f.stack, c)
			pushed++
		}
	}
	return pushed
}

// walked marks the states a run went through as explored, under
// PartialOrder: the first run through a state takes its first option, so a
// backtrack point naming that option is explored already.
func (f *frontier) walked(choices []choice) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for j := range choices {
		k := prefixKey(choices[:j+1])
		if f.pushed[k] == nil {
			f.pushed[k] = &subtree{}
		}
	}
}

// declareSometimes records the conditions a worker declared. Every worker
// declares the same ones; the first to come records them.
func (f *frontier) declareSometimes(names []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sometimes == nil {
		f.sometimes = names
	}
}

func (f *frontier) reach(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reached == nil {
		f.reached = map[string]bool{}
	}
	f.reached[name] = true
}

func (f *frontier) refuse(msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refused == nil {
		f.refused = map[string]bool{}
	}
	f.refused[msg] = true
}

// refusals returns the statements refused as unsupported, sorted.
func (f *frontier) refusals() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.refused))
}

// unreached returns the declared conditions no run met.
func (f *frontier) unreached() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, n := range f.sometimes {
		if !f.reached[n] {
			out = append(out, n)
		}
	}
	return out
}

// fail ends the exploration for every worker on a misuse.
func (f *frontier) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fatal == nil {
		f.fatal = err
	}
	f.stopped = true
	f.busy--
	f.wakeAll()
}

// halt stops handing out random runs, as one violated. The runs handed out
// already, the earlier ones among them, go on.
func (f *frontier) halt() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.halted = true
}

// found records a violating run, keeping the earliest one: the first in
// depth-first order, or the one of the lowest index under the Random
// strategy.
func (f *frontier) found(choices []choice, index int, res *result, children []subtree) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.random {
		if f.bestResult == nil || index < f.bestIndex {
			f.bestIndex, f.bestResult = index, res
		}
		f.busy--
		f.settled(0)
		return
	}
	ranks := make([]int, len(choices))
	for i, c := range choices {
		ranks[i] = c.rank()
	}
	if f.best == nil || precedes(ranks, f.best) {
		f.best, f.bestResult = ranks, res
	}
	// Under PartialOrder the violating run's backtrack points go to its
	// ancestors, where the alternatives before its own are before it in
	// depth-first order and still to be explored.
	pushed := f.push(children)
	f.busy--
	f.settled(pushed)
}

// after reports whether every run under prefix comes after the best
// violation in depth-first order: at the first choice they differ, the
// prefix took a later alternative.
func (f *frontier) after(prefix []choice) bool {
	if f.best == nil || f.exploreAll {
		return false
	}
	for i, c := range prefix {
		if i >= len(f.best) {
			return false
		}
		if k := c.rank(); k != f.best[i] {
			return k > f.best[i]
		}
	}
	return false // the prefix leads to the best run, and may to earlier ones
}

func precedes(a, b []int) bool {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// children returns the subtrees below prefix that run r did not enter, which
// are the alternatives it did not pick at every choice it made past the
// prefix, with what their prefix costs under IterativeBounding. They are
// ordered so that the stack yields the deepest first, and at one choice the
// first option first. Past the prefix a run
// picks the first option, or under IterativeBounding the first that
// preempts nothing, so the alternatives before the pick are subtrees too.
func (s *Sim) children(r *run, prefix int) []subtree {
	c := r.choices
	var out []subtree
	for j := prefix; j < len(c); j++ {
		for v := c[j].n - 1; v >= 0; v-- {
			if v == c[j].picked {
				continue
			}
			p := make([]choice, j+1)
			copy(p, c[:j])
			p[j] = choice{label: c[j].label, n: c[j].n, picked: v, fp: c[j].fp, free: c[j].free}
			if s.shardTotal > 1 && len(p) >= s.shardDepth && !s.ownsPrefix(p) {
				continue // another machine's subtree
			}
			sub := subtree{prefix: p}
			if j < len(r.costs) {
				sub.level = r.costs[j].level(v)
			}
			out = append(out, sub)
		}
	}
	return out
}

// checkShared is check for one of several workers sharing f.
func (s *Sim) checkShared(f *frontier, worker int) *result {
	start := time.Now()
	names := make([]string, len(s.sometimes))
	for i, c := range s.sometimes {
		names[i] = c.name
	}
	f.declareSometimes(names)
	runs, maxDepth := 0, 0
	seen := seenChoices{}
	k := 0
	if s.strategy.prioritized {
		// The run's length is measured on a run every worker makes alike,
		// so that the runs drawn do not depend on Workers. It picks by
		// priority without switches or faults, as a fault such as a loss
		// listed first would end it early and put every run's faults at its
		// start. It is no run of the exploration, and what it finds is left
		// to the runs that are, but it goes through the nondeterminism check,
		// as state it leaves behind shows in the next run.
		r := s.newRunMeasuring(nil, true)
		r.rng, r.seen, r.path = s.measuringRng(), seen, fnvOffset
		r.prio = newPrioritized(r.rng, 1, 1, 0, 0, 0)
		r.execute()
		k = r.steps
	}
	for {
		taken, index, ok := f.take(worker)
		if !ok {
			return &result{Runs: runs, MaxDepth: maxDepth, Elapsed: time.Since(start)}
		}
		prefix := taken.prefix
		if !taken.again {
			runs++
		}
		if s.partialOrder && f.iterative {
			// The runs a violation is shrunk and retraced with apply it too.
			f.mu.Lock()
			s.passBound, s.passBounded = f.level, true
			f.mu.Unlock()
		}
		r := s.newRun(prefix)
		if s.partialOrder {
			f.mu.Lock()
			r.sleepPending = f.sleepFor(prefix)
			f.mu.Unlock()
		}
		if f.random {
			r.rng, r.seen, r.path = s.rngFor(index), seen, fnvOffset
			if s.strategy.prioritized {
				losses := 0
				for _, q := range s.queues {
					losses += q.lossBudget
				}
				r.prio = newPrioritized(r.rng, s.strategy.depth, k, s.maxCrashes, losses, s.maxStalls)
			}
		}
		r.tracing = s.verbose
		v := r.execute()
		if s.depObserve != nil && !r.covered {
			s.depObserve(r, v) // a run cut where its siblings cover the rest is no run of its own
		}
		if taken.again {
			// The run was made, and checked, in the pass of its own level.
			// Only its subtrees are wanted here.
			var children []subtree
			if v == nil || s.exploreAll {
				children = s.children(r, len(prefix))
			}
			if v != nil && v.kind == "fatal" {
				f.fail(v.err)
				return &result{Runs: runs, MaxDepth: maxDepth, Elapsed: time.Since(start)}
			}
			f.finish(children)
			continue
		}
		var children []subtree
		switch {
		case f.random:
		case s.partialOrder:
			f.record(r, len(prefix))
			children = s.reduce(r, len(prefix), v != nil || r.cut || r.pending != nil, f.sleepAt(r, len(prefix)))
			f.walked(r.choices)
		case v == nil || s.exploreAll:
			children = s.children(r, len(prefix))
		}
		if s.partialOrder && f.iterative && (v == nil || v.kind != "fatal") {
			// A pass's level is the bound, and a run of fewer preemptions
			// is a run of an earlier pass, which reported what it broke.
			// Only the bound the user set leaves no higher pass to make.
			higher := r.boundCut && (!s.boundPreemptions || s.passBound < s.maxPreemptions)
			if f.passRun(r.preempts, higher) {
				runs--
				f.finish(children)
				continue
			}
		}
		maxDepth = max(maxDepth, len(r.choices))
		if r.cut {
			f.cut()
		}
		if s.verbose {
			s.printRun(runs, r)
		}
		if v != nil && v.kind == "fatal" {
			f.fail(v.err)
			return &result{Runs: runs, MaxDepth: maxDepth, Elapsed: time.Since(start)}
		}
		if v != nil {
			if f.random {
				f.halt() // shrinking takes a run per choice
			}
			choices := r.choices
			if !s.exploreAll {
				r, v = s.shrink(r, v)
				if !r.tracing {
					r, v = s.retrace(r, v)
					if v.kind == "fatal" { // the rerun took another path
						f.fail(v.err)
						return &result{Runs: runs, MaxDepth: maxDepth, Elapsed: time.Since(start)}
					}
				}
			}
			// Keep exploring: a subtree before this run may hold a violation
			// a single worker would have found first.
			f.found(choices, index, s.makeResult(r, v, runs, maxDepth, false, start), children)
			continue
		}
		f.finish(children)
	}
}

// merge combines the workers' results: the best violation if one was found,
// else the run counts with whether the exploration finished.
func (f *frontier) merge(results []*result, workers int) *result {
	merged := &result{Complete: !f.incomplete, Workers: workers, Fatal: f.fatal, Unreached: f.unreached(), CutRuns: f.cutRuns(), Unsupported: f.refusals(), Shared: f.shared.Load()}
	for _, r := range results {
		merged.Runs += r.Runs
		merged.MaxDepth = max(merged.MaxDepth, r.MaxDepth)
		merged.Shard = r.Shard
		merged.strategy = r.strategy
		merged.eager, merged.lazy, merged.partial = r.eager, r.lazy, r.partial
		if r.Replay {
			// A replay does not go through the frontier, so its cut is its own.
			merged.Replay, merged.Schedule = true, r.Schedule
			merged.CutRuns += r.CutRuns
		}
	}
	best := f.bestResult
	for _, r := range results {
		if r.Fatal != nil && merged.Fatal == nil {
			merged.Fatal = r.Fatal
		}
		if best == nil && r.Violated {
			best = r // a replay, which does not go through the frontier
		}
	}
	if merged.Fatal != nil {
		return merged
	}
	merged.CompleteLevel, merged.Again = f.completeLevel(), f.again
	// A pass covers the tree up to its level only, so the share of the
	// stack says nothing of the runs left.
	if best == nil && f.incomplete && !f.random && !f.iterative && merged.Shard == "" {
		merged.Estimated = true
		merged.Explored, merged.Remaining = f.remaining()
	}
	if best != nil {
		v := *best
		v.Runs, v.CutRuns, v.MaxDepth, v.Workers, v.Unsupported, v.Shared, v.strategy, v.eager, v.lazy, v.partial = merged.Runs, merged.CutRuns, merged.MaxDepth, workers, merged.Unsupported, merged.Shared, merged.strategy, merged.eager, merged.lazy, merged.partial
		return &v
	}
	return merged
}

// remaining estimates the runs left in an exploration MaxRuns, MaxDuration
// or a signal cut short, with the weighted backtrack estimator of Kilby,
// Slaney, Thiébaux and Walsh ("Estimating Search Tree Size", AAAI 2006). A
// subtree's weight is the share of the tree it would hold if every node's
// subtrees were the same size, the product of 1/d over the nodes above it
// with d their number of subtrees, so the subtrees left on the stack hold
// a share w and the runs made 1-w. It returns that share and the runs left,
// which it gives only from minExplored on.
//
// The estimator sums the weights of the runs made, as they finish. Summing
// the stack's instead gives the same share, since the runs made and the
// subtrees left partition the tree, and needs nothing saved in a checkpoint
// to go on with a resumed exploration. A shard's or a violation's dropped
// subtrees break the partition, and so do the ones a pass of
// IterativeBounding leaves to later passes, so the caller asks for none of
// them. f.mu need
// not be held: every worker has returned.
//
// Under PartialOrder a node's subtrees are the ones pushed or run from it,
// not the options of its choice, which would count the subtrees the
// reduction leaves out. A later run may still add some, so the estimate may
// be off either way.
//
// Early in an exploration the share explored comes out several times too
// large, since the first runs follow the paths with the fewest choices,
// and the runs left as many times too few, which would read as nearly done.
// Below minExplored no count is given, which also keeps 1-w clear of the
// rounding of a w near 1 and the runs left within a few times the runs made.
func (f *frontier) remaining() (explored float64, left int) {
	var subtrees map[string]int
	if f.partial {
		subtrees = map[string]int{}
		for k := range f.pushed {
			if k != "" {
				parent := k[:strings.LastIndexByte(k[:len(k)-1], ',')+1]
				subtrees[parent]++
			}
		}
	}
	w := 0.0
	for _, sub := range f.stack {
		share := 1.0
		for i, c := range sub.prefix {
			d := c.n
			if f.partial {
				d = subtrees[prefixKey(sub.prefix[:i])]
			}
			share /= float64(max(d, 1))
		}
		w += share
	}
	explored = max(1-w, 0)
	if explored < minExplored {
		return explored, 0
	}
	return explored, roughly(float64(f.prior+f.runs) * w / explored)
}

// minExplored is the share of the tree explored from which the runs left
// are estimated. On small scenarios the runs left came out as low as a
// fifth of the true count up to about 20% explored, and within two times
// from there.
const minExplored = 0.2

// roughly rounds n to one significant figure, as the estimate is no more
// precise than that, and its exact value would differ between workers'
// orders of finishing.
func roughly(n float64) int {
	if n < 1 {
		return 1
	}
	e := math.Pow(10, math.Floor(math.Log10(n)))
	return int(math.Round(n/e) * e)
}

// checkpoint is the rest of an exploration MaxRuns, MaxDuration or a signal
// cut short, as the subtrees not explored yet, each as the choices that lead
// to it. Under IterativeBounding they are the rest of the current pass,
// whose subtrees of lower levels lead to runs of its level through runs made
// before, and the later passes start from the root again.
// checkpointVersion is the version of the checkpoint format. 2 is the tree
// of eager starts and of INSERT yielding first, whose prefixes a checkpoint
// of version 1 does not name. An exploration under IterativeBounding saves
// version iterativeCheckpointVersion, with its pass and each subtree's
// level, so that one of another strategy, or a detest that knows no levels,
// refuses it rather than resume it in another order.
const (
	checkpointVersion          = 2
	iterativeCheckpointVersion = 3
)

type checkpoint struct {
	Version  int             `json:"version"`
	Runs     int             `json:"runs"`           // explored before, in all the explorations so far
	Cuts     int             `json:"cuts,omitempty"` // of those runs, the ones cut at MaxIdleTicks
	Subtrees [][]savedChoice `json:"subtrees"`
	// Level, Higher and Levels are the pass of IterativeBounding, whether
	// it left out a subtree of a higher level, and each subtree's level.
	Level   int      `json:"level,omitempty"`
	Higher  bool     `json:"higher,omitempty"`
	Levels  []int    `json:"levels,omitempty"`
	Reached []string `json:"reached,omitempty"` // Sometimes conditions met so far
	Refused []string `json:"refused,omitempty"` // statements refused as unsupported so far
	// Lazy is why EagerStart was dropped, so that a resumed exploration
	// goes on in the tree its subtrees belong to.
	Lazy string `json:"lazy,omitempty"`
}

type savedChoice struct {
	Label  string `json:"label"`
	N      int    `json:"n"`
	Picked int    `json:"picked"`
	FP     uint64 `json:"fp,omitempty"`
	Free   int    `json:"free,omitempty"` // under IterativeBounding (see choice.free)
}

// load resumes from the checkpoint at path, if there is one.
func (f *frontier) load(path string) error {
	b, err := os.ReadFile(path) //nolint:gosec // the path the user gave in DETEST_CHECKPOINT
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var ck checkpoint
	if err := json.Unmarshal(b, &ck); err != nil {
		return fmt.Errorf("detest: %s is not a checkpoint of this detest: %w", path, err)
	}
	switch {
	case ck.Version == checkpointVersion && f.iterative:
		return fmt.Errorf("detest: %s is a checkpoint of an exploration without IterativeBounding, which it cannot resume in its order", path)
	case ck.Version == iterativeCheckpointVersion && !f.iterative:
		return fmt.Errorf("detest: %s is a checkpoint of an exploration under IterativeBounding, which only IterativeBounding resumes", path)
	case ck.Version != checkpointVersion && ck.Version != iterativeCheckpointVersion:
		return fmt.Errorf("detest: %s is not a checkpoint of this detest: version %d", path, ck.Version)
	case !validLevels(ck):
		// A level the runs do not spend would make them again unchecked,
		// or count them at another pass, where nothing in the runs tells.
		return fmt.Errorf("detest: %s is not a checkpoint of this detest: its levels do not fit its pass", path)
	}
	f.stack = f.stack[:0]
	for k, st := range ck.Subtrees {
		p := make([]choice, len(st))
		for i, c := range st {
			p[i] = choice{label: c.Label, n: c.N, picked: c.Picked, fp: c.FP, free: c.Free}
		}
		sub := subtree{prefix: p}
		if f.iterative {
			sub.level = ck.Levels[k]
		}
		f.stack = append(f.stack, sub)
	}
	f.level, f.higher = ck.Level, ck.Higher
	f.prior, f.cuts, f.lazy = ck.Runs, ck.Cuts, ck.Lazy
	for _, n := range ck.Reached {
		f.reach(n)
	}
	for _, m := range ck.Refused {
		f.refuse(m)
	}
	return nil
}

// save writes the subtrees left to path.
func (f *frontier) save(path string) error {
	ck := checkpoint{Version: checkpointVersion, Runs: f.prior + f.runs, Cuts: f.cutRuns(), Lazy: f.lazy}
	for n := range f.reached {
		ck.Reached = append(ck.Reached, n)
	}
	sort.Strings(ck.Reached)
	ck.Refused = slices.Sorted(maps.Keys(f.refused))
	for _, sub := range f.stack {
		st := make([]savedChoice, len(sub.prefix))
		for i, c := range sub.prefix {
			st[i] = savedChoice{Label: c.label, N: c.n, Picked: c.picked, FP: c.fp, Free: c.free}
		}
		ck.Subtrees = append(ck.Subtrees, st)
		if f.iterative {
			ck.Levels = append(ck.Levels, sub.level)
		}
	}
	if f.iterative {
		ck.Version, ck.Level, ck.Higher = iterativeCheckpointVersion, f.level, f.higher
	}
	b, err := json.Marshal(ck)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600) //nolint:gosec // the path the user gave in DETEST_CHECKPOINT
}

// validLevels reports whether the pass and the subtrees' levels of a
// checkpoint of version iterativeCheckpointVersion fit together, as a pass
// holds subtrees of its level or lower only.
func validLevels(ck checkpoint) bool {
	if ck.Version != iterativeCheckpointVersion {
		return true
	}
	if ck.Level < 0 || len(ck.Levels) != len(ck.Subtrees) {
		return false
	}
	for _, l := range ck.Levels {
		if l < 0 || l > ck.Level {
			return false
		}
	}
	return true
}
