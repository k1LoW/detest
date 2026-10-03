package detest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
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
	mu         sync.Mutex
	wake       []chan struct{} // per worker, buffered so a wakeup is never lost
	waiting    []bool
	stack      [][]choice // LIFO keeps the order depth first and the stack small
	busy       int        // workers running a prefix taken from the stack
	stopped    bool
	incomplete bool // MaxRuns cut the exploration short
	runs       int
	maxRuns    int
	// best is the choices of the earliest violating run found so far, in the
	// depth-first order one worker explores in, with its result. Subtrees
	// after it are dropped and the ones before it still explored, so the
	// violation reported is the one a single worker would report.
	best       []int
	bestResult *result
	prior      int   // runs of the explorations a checkpoint resumes
	fatal      error // a misuse that ends the exploration, such as a stale checkpoint
}

func newFrontier(workers, maxRuns int) *frontier {
	f := &frontier{stack: [][]choice{nil}, maxRuns: maxRuns, wake: make([]chan struct{}, workers), waiting: make([]bool, workers)}
	for i := range f.wake {
		f.wake[i] = make(chan struct{}, 1)
	}
	return f
}

// wakeAll wakes the waiting workers. f.mu is held.
func (f *frontier) wakeAll() {
	for i, w := range f.waiting {
		if w {
			f.waiting[i] = false
			select {
			case f.wake[i] <- struct{}{}:
			default:
			}
		}
	}
}

// take returns the next prefix to run, waiting while other workers may still
// add some. It reports false once the exploration is over.
func (f *frontier) take(worker int) ([]choice, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		for len(f.stack) > 0 && f.after(f.stack[len(f.stack)-1]) {
			f.stack = f.stack[:len(f.stack)-1] // after the best violation
		}
		if len(f.stack) > 0 || f.busy == 0 || f.stopped {
			break
		}
		f.waiting[worker] = true
		f.mu.Unlock()
		<-f.wake[worker]
		f.mu.Lock()
	}
	if f.stopped || len(f.stack) == 0 {
		f.wakeAll() // the exploration is over: let the others see it
		return nil, false
	}
	if f.runs >= f.maxRuns {
		f.incomplete, f.stopped = true, true
		f.wakeAll()
		return nil, false
	}
	p := f.stack[len(f.stack)-1]
	f.stack = f.stack[:len(f.stack)-1]
	f.busy++
	f.runs++
	return p, true
}

// finish returns a taken prefix with the subtrees its run revealed.
func (f *frontier) finish(children [][]choice) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range children {
		if !f.after(c) {
			f.stack = append(f.stack, c)
		}
	}
	f.busy--
	f.wakeAll()
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

// found records a violating run, keeping the earliest one.
func (f *frontier) found(choices []choice, res *result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	picks := make([]int, len(choices))
	for i, c := range choices {
		picks[i] = c.picked
	}
	if f.best == nil || precedes(picks, f.best) {
		f.best, f.bestResult = picks, res
	}
	f.busy--
	f.wakeAll()
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
func (m *Model) children(c []choice, prefix int) [][]choice {
	var out [][]choice
	for j := prefix; j < len(c); j++ {
		for v := c[j].n - 1; v > c[j].picked; v-- {
			p := make([]choice, j+1)
			copy(p, c[:j])
			p[j] = choice{label: c[j].label, n: c[j].n, picked: v}
			if m.shardTotal > 1 && len(p) >= m.shardDepth && !m.ownsPrefix(p) {
				continue // another machine's subtree
			}
			out = append(out, p)
		}
	}
	return out
}

// checkShared is check for one of several workers sharing f.
func (m *Model) checkShared(f *frontier, worker int) *result {
	start := time.Now()
	runs, maxDepth := 0, 0
	for {
		prefix, ok := f.take(worker)
		if !ok {
			return &result{Runs: runs, MaxDepth: maxDepth, Elapsed: time.Since(start)}
		}
		runs++
		r := m.newRun(prefix)
		r.tracing = m.verbose
		v := r.execute()
		maxDepth = max(maxDepth, len(r.choices))
		if m.verbose {
			m.printRun(runs, r)
		}
		if v != nil && v.kind == "fatal" {
			f.fail(v.err)
			return &result{Runs: runs, MaxDepth: maxDepth, Elapsed: time.Since(start)}
		}
		if v != nil {
			choices := r.choices
			if !r.tracing {
				r, v = m.retrace(r, v)
			}
			// Keep exploring: a subtree before this run may hold a violation
			// a single worker would have found first.
			f.found(choices, m.makeResult(r, v, runs, maxDepth, false, start))
			continue
		}
		f.finish(m.children(r.choices, len(prefix)))
	}
}

// merge combines the workers' results: the best violation if one was found,
// else the run counts with whether the exploration finished.
func (f *frontier) merge(results []*result, workers int) *result {
	merged := &result{Complete: !f.incomplete, Workers: workers, Fatal: f.fatal}
	for _, r := range results {
		merged.Runs += r.Runs
		merged.MaxDepth = max(merged.MaxDepth, r.MaxDepth)
		merged.Shard = r.Shard
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
		v.Runs, v.MaxDepth, v.Workers = merged.Runs, merged.MaxDepth, workers
		return &v
	}
	return merged
}

// checkpoint is the rest of an exploration MaxRuns cut short: the subtrees
// not explored yet, each as the choices that lead to it.
type checkpoint struct {
	Version  int             `json:"version"`
	Runs     int             `json:"runs"` // explored before, in all the explorations so far
	Subtrees [][]savedChoice `json:"subtrees"`
}

type savedChoice struct {
	Label  string `json:"label"`
	N      int    `json:"n"`
	Picked int    `json:"picked"`
}

// load resumes from the checkpoint at path, if there is one.
func (f *frontier) load(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var ck checkpoint
	if err := json.Unmarshal(b, &ck); err != nil || ck.Version != 1 {
		return fmt.Errorf("detest: %s is not a checkpoint of this detest: %v", path, err)
	}
	f.stack = f.stack[:0]
	for _, st := range ck.Subtrees {
		p := make([]choice, len(st))
		for i, c := range st {
			p[i] = choice{label: c.Label, n: c.N, picked: c.Picked}
		}
		f.stack = append(f.stack, p)
	}
	f.prior = ck.Runs
	return nil
}

// save writes the subtrees left to path.
func (f *frontier) save(path string) error {
	ck := checkpoint{Version: 1, Runs: f.prior + f.runs}
	for _, p := range f.stack {
		st := make([]savedChoice, len(p))
		for i, c := range p {
			st[i] = savedChoice{Label: c.label, N: c.n, Picked: c.picked}
		}
		ck.Subtrees = append(ck.Subtrees, st)
	}
	b, err := json.Marshal(ck)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
