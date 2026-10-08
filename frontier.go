package detest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// frontier holds the subtrees of the schedule tree no worker has explored yet,
// each as the choice prefix all of its runs share. A finished run reveals the
// unexplored siblings along its path, and every one of them is an independent
// subtree, so idle workers take them as they appear: the load balances itself
// and every run is executed once.
//
// Workers wait for subtrees on channels created outside every bubble. A
// sync.Cond would not do: synctest counts a goroutine in Cond.Wait as durably
// blocked and, with nothing else running in its bubble, reports a deadlock.
type frontier struct {
	lazy       string // why EagerStart was dropped, if it was (see Explore)
	mu         sync.Mutex
	wake       []chan struct{} // per worker, buffered so a wakeup is never lost
	waiting    []bool
	stack      [][]choice // LIFO keeps the order depth first and the stack small
	busy       int        // workers running a prefix taken from the stack
	stopped    bool
	incomplete bool // MaxRuns or MaxDuration cut the exploration short
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
	expired   atomic.Bool // MaxDuration has passed
	// best is the choices of the earliest violating run found so far, in the
	// depth-first order one worker explores in, with its result. Subtrees
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
}

// workerProgress is what the stall watchdog reads of a worker: the steps its
// scheduler took, and whether it is idle, waiting for a subtree or done, so
// that its standing still is no stall.
type workerProgress struct {
	steps atomic.Int64
	idle  atomic.Bool
}

func newFrontier(workers, maxRuns int) *frontier {
	f := &frontier{stack: [][]choice{nil}, maxRuns: maxRuns, wake: make([]chan struct{}, workers), waiting: make([]bool, workers), progress: make([]workerProgress, workers)}
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
func (f *frontier) take(worker int) ([]choice, int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.random {
		return f.takeRandom(worker)
	}
	for {
		for len(f.stack) > 0 && f.after(f.stack[len(f.stack)-1]) {
			f.stack = f.stack[:len(f.stack)-1] // after the best violation
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
		return nil, 0, false
	}
	if f.runs >= f.maxRuns || (f.runs > 0 && f.expired.Load()) {
		f.incomplete, f.stopped = true, true
		f.progress[worker].idle.Store(true)
		f.wakeAll()
		return nil, 0, false
	}
	p := f.stack[len(f.stack)-1]
	f.stack = f.stack[:len(f.stack)-1]
	f.busy++
	f.runs++
	return p, 0, true
}

// takeRandom is take under the Random strategy. Indexes are handed out in
// order, so once a run violates every run before it has been taken, and no
// other is needed to report the violation of the lowest index. f.mu
// is held.
func (f *frontier) takeRandom(worker int) ([]choice, int, bool) {
	if f.stopped || f.halted {
		f.progress[worker].idle.Store(true)
		return nil, 0, false
	}
	if f.runs >= f.maxRuns || (f.runs > 0 && f.expired.Load()) {
		f.incomplete, f.stopped = true, true
		f.progress[worker].idle.Store(true)
		return nil, 0, false
	}
	i := f.next
	f.next++
	f.busy++
	f.runs++
	return nil, i, true
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
func (f *frontier) finish(children [][]choice) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pushed := 0
	for _, c := range children {
		if !f.after(c) {
			f.stack = append(f.stack, c)
			pushed++
		}
	}
	f.busy--
	f.settled(pushed)
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
func (f *frontier) found(choices []choice, index int, res *result) {
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
	picks := make([]int, len(choices))
	for i, c := range choices {
		picks[i] = c.picked
	}
	if f.best == nil || precedes(picks, f.best) {
		f.best, f.bestResult = picks, res
	}
	f.busy--
	f.settled(0)
}

// after reports whether every run under prefix comes after the best
// violation in depth-first order: at the first choice they differ, the
// prefix took a later alternative.
func (f *frontier) after(prefix []choice) bool {
	if f.best == nil {
		return false
	}
	for i, c := range prefix {
		if i >= len(f.best) {
			return false
		}
		if c.picked != f.best[i] {
			return c.picked > f.best[i]
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

// children returns the subtrees below prefix that the run with choices c did
// not enter: at every choice the run made past the prefix, the alternatives it
// did not pick. They are ordered so that the stack yields the deepest first.
func (s *Sim) children(c []choice, prefix int) [][]choice {
	var out [][]choice
	for j := prefix; j < len(c); j++ {
		for v := c[j].n - 1; v > c[j].picked; v-- {
			p := make([]choice, j+1)
			copy(p, c[:j])
			p[j] = choice{label: c[j].label, n: c[j].n, picked: v, fp: c[j].fp}
			if s.shardTotal > 1 && len(p) >= s.shardDepth && !s.ownsPrefix(p) {
				continue // another machine's subtree
			}
			out = append(out, p)
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
		prefix, index, ok := f.take(worker)
		if !ok {
			return &result{Runs: runs, MaxDepth: maxDepth, Elapsed: time.Since(start)}
		}
		runs++
		r := s.newRun(prefix)
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
			r, v = s.shrink(r, v)
			if !r.tracing {
				r, v = s.retrace(r, v)
				if v.kind == "fatal" { // the rerun took another path
					f.fail(v.err)
					return &result{Runs: runs, MaxDepth: maxDepth, Elapsed: time.Since(start)}
				}
			}
			// Keep exploring: a subtree before this run may hold a violation
			// a single worker would have found first.
			f.found(choices, index, s.makeResult(r, v, runs, maxDepth, false, start))
			continue
		}
		if f.random {
			f.finish(nil)
			continue
		}
		f.finish(s.children(r.choices, len(prefix)))
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
		merged.eager, merged.lazy = r.eager, r.lazy
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
	if best != nil {
		v := *best
		v.Runs, v.CutRuns, v.MaxDepth, v.Workers, v.Unsupported, v.Shared, v.strategy, v.eager, v.lazy = merged.Runs, merged.CutRuns, merged.MaxDepth, workers, merged.Unsupported, merged.Shared, merged.strategy, merged.eager, merged.lazy
		return &v
	}
	return merged
}

// checkpoint is the rest of an exploration MaxRuns or MaxDuration cut short: the subtrees
// not explored yet, each as the choices that lead to it.
type checkpoint struct {
	Version  int             `json:"version"`
	Runs     int             `json:"runs"`           // explored before, in all the explorations so far
	Cuts     int             `json:"cuts,omitempty"` // of those runs, the ones cut at MaxIdleTicks
	Subtrees [][]savedChoice `json:"subtrees"`
	Reached  []string        `json:"reached,omitempty"` // Sometimes conditions met so far
	Refused  []string        `json:"refused,omitempty"` // statements refused as unsupported so far
	// Lazy is why EagerStart was dropped, so that a resumed exploration
	// goes on in the tree its subtrees belong to.
	Lazy string `json:"lazy,omitempty"`
}

type savedChoice struct {
	Label  string `json:"label"`
	N      int    `json:"n"`
	Picked int    `json:"picked"`
	FP     uint64 `json:"fp,omitempty"`
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
	if ck.Version != 1 {
		return fmt.Errorf("detest: %s is not a checkpoint of this detest: version %d", path, ck.Version)
	}
	f.stack = f.stack[:0]
	for _, st := range ck.Subtrees {
		p := make([]choice, len(st))
		for i, c := range st {
			p[i] = choice{label: c.Label, n: c.N, picked: c.Picked, fp: c.FP}
		}
		f.stack = append(f.stack, p)
	}
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
	ck := checkpoint{Version: 1, Runs: f.prior + f.runs, Cuts: f.cutRuns(), Lazy: f.lazy}
	for n := range f.reached {
		ck.Reached = append(ck.Reached, n)
	}
	sort.Strings(ck.Reached)
	ck.Refused = slices.Sorted(maps.Keys(f.refused))
	for _, p := range f.stack {
		st := make([]savedChoice, len(p))
		for i, c := range p {
			st[i] = savedChoice{Label: c.label, N: c.n, Picked: c.picked, FP: c.fp}
		}
		ck.Subtrees = append(ck.Subtrees, st)
	}
	b, err := json.Marshal(ck)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600) //nolint:gosec // the path the user gave in DETEST_CHECKPOINT
}
