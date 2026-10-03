package detest

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Explore declares a simulation with fn and explores every schedule of it,
// failing the test on a violation. fn registers simulated resources,
// processes, seeds and invariants on s and returns; the exploration starts
// after it returns.
//
// Both run inside a testing/synctest bubble, so the code under test sleeps on
// a fake clock and detest can tell when a process is blocked. Build everything
// the code under test uses inside fn: channels and timers created outside the
// bubble do not count as blocking inside it.
//
// The exploration is bounded (preemptions, failures, MaxRuns), and the log
// line Explore writes states how far it got. A violating schedule is run once
// more to record its trace, so seeds and invariants see that run twice.
func Explore(t *testing.T, fn func(t *testing.T, s *Sim), opts ...Option) {
	t.Helper()
	start := time.Now()
	n := workerCount(opts)
	f := newFrontier(n, maxRunsOf(opts))
	stop := watchStall(f)
	defer stop()
	ckpt := os.Getenv("DETEST_CHECKPOINT")
	if ckpt != "" {
		if err := f.load(ckpt); err != nil {
			t.Fatal(err)
		}
	}
	var res *result
	var expect *string
	if n == 1 {
		var r *result
		if r, expect = exploreBubble(t, fn, opts, f, 0); r != nil {
			res = f.merge([]*result{r}, 1)
		}
	} else {
		res, expect = exploreWorkers(t, fn, opts, f, n)
	}
	if res == nil {
		return // fn stopped the test
	}
	if res.Fatal != nil {
		t.Fatal(res.Fatal)
	}
	res.Elapsed = time.Since(start) // the bubble's clock is fake
	res.PriorRuns = f.prior
	if ckpt != "" {
		if res.Violated || res.Complete {
			_ = os.Remove(ckpt) //nolint:gosec // the path the user gave in DETEST_CHECKPOINT
		} else if err := f.save(ckpt); err != nil {
			t.Error(err)
		} else {
			res.Checkpoint = ckpt
		}
	}
	switch {
	case expect == nil && res.Violated:
		t.Error(res.report())
	case expect == nil:
		t.Log(res.report())
	case !res.Violated && res.Checkpoint != "":
		t.Log(res.report()) // not found yet; the rest of the exploration is saved
	case !res.Violated:
		t.Errorf("detest: expected a violation containing %q, found none\n%s", *expect, res.report())
	case !strings.Contains(res.Err.Error(), *expect):
		t.Errorf("detest: expected a violation containing %q, found another\n%s", *expect, res.report())
	default:
		t.Log(res.report())
	}
}

func workerCount(opts []Option) int {
	probe := &Sim{}
	for _, o := range opts {
		o(probe)
	}
	n := probe.workers
	if s := os.Getenv("DETEST_WORKERS"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			n = v
		}
	}
	if os.Getenv("DETEST_SCHEDULE") != "" || n < 1 {
		return 1 // a replay is one run
	}
	return n
}

// exploreWorkers runs n workers as subtests, each in its own bubble:
// synctest.Wait waits for every goroutine of a bubble, so workers sharing one
// would wait for each other. The workers take subtrees from one frontier.
func exploreWorkers(t *testing.T, fn func(t *testing.T, s *Sim), opts []Option, f *frontier, n int) (*result, *string) {
	results := make([]*result, n)
	expects := make([]*string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			t.Run(fmt.Sprintf("worker%d", i), func(t *testing.T) {
				results[i], expects[i] = exploreBubble(t, fn, opts, f, i)
			})
		})
	}
	wg.Wait()
	for _, r := range results {
		if r == nil {
			return nil, nil
		}
	}
	return f.merge(results, n), expects[0]
}

func maxRunsOf(opts []Option) int {
	probe := newSimDefaults()
	for _, o := range opts {
		o(probe)
	}
	return probe.maxRuns
}

// exploreBubble declares and explores the simulation, alone or as a worker
// taking subtrees from f.
func exploreBubble(t *testing.T, fn func(t *testing.T, s *Sim), opts []Option, f *frontier, worker int) (*result, *string) {
	var res *result
	var expect *string
	synctest.Test(t, func(t *testing.T) {
		s := newSim(t, opts...)
		if f != nil {
			s.progress = &f.progress[worker]
		}
		s.frontier, s.worker = f, worker
		defer s.closeSQL() // the bubble cannot end while the pools' goroutines run
		fn(t, s)
		s.frozen = true
		res = s.check()
		if s.shardTotal > 1 {
			res.Shard = fmt.Sprintf("%d/%d", s.shardIndex, s.shardTotal)
		}
		expect = s.expect
	})
	return res, expect
}

func (s *Sim) closeSQL() {
	for _, db := range s.sqlDBs {
		_ = db.Close()
	}
}

// stallTimeout is how long the scheduler may make no progress before the
// watchdog reports a hang. DETEST_STALL overrides it; 0 disables the watchdog,
// for stepping through a run in a debugger.
func stallTimeout() time.Duration {
	if s := os.Getenv("DETEST_STALL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return 30 * time.Second
}

// watchStall reports a process blocked on something synctest does not treat
// as durably blocking, typically a sync.Mutex held by a process parked at a
// yield point. The scheduler then waits in synctest.Wait forever and no
// timeout inside the bubble can fire, so the watchdog runs outside it, on real
// time, and crashes the test binary with the blocked goroutines.
//
// Each worker is watched on its own: one worker stuck while the others keep
// exploring is a stall all the same. A worker waiting for a subtree, or done,
// is idle and never stalls.
func watchStall(f *frontier) (stop func()) {
	done := make(chan struct{})
	limit := stallTimeout()
	if limit <= 0 {
		return func() {}
	}
	go func() {
		tick := time.NewTicker(limit / 10)
		defer tick.Stop()
		last := make([]int64, len(f.progress))
		since := make([]time.Time, len(f.progress))
		for i := range f.progress {
			last[i], since[i] = f.progress[i].steps.Load(), time.Now()
		}
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			stalled := false
			for i := range f.progress {
				w := &f.progress[i]
				if n := w.steps.Load(); n != last[i] || w.idle.Load() {
					last[i], since[i] = n, time.Now()
					continue
				}
				if time.Since(since[i]) >= limit {
					stalled = true
				}
			}
			if !stalled {
				continue
			}
			fmt.Fprintf(os.Stderr, "detest: no scheduling progress for %s. A process is blocked on something detest does not model, such as a sync.Mutex held across a yield point or real I/O; inject a detest.Mutex or detest.RWMutex, or keep the I/O out of the code under test. Goroutines blocked in the bubble:\n\n%s", limit, nonDurableStacks())
			panic("detest: stalled")
		}
	}()
	return func() { close(done) }
}

// nonDurableStacks returns the stacks of bubble goroutines that are blocked
// but not durably, the ones synctest.Wait keeps waiting for.
func nonDurableStacks() string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, len(buf)*2)
	}
	var b strings.Builder
	for g := range strings.SplitSeq(string(buf), "\n\n") {
		header, _, _ := strings.Cut(g, "\n")
		if !strings.Contains(header, "synctest bubble") || strings.Contains(header, "durable") {
			continue
		}
		if strings.Contains(header, "[running") || strings.Contains(header, "[runnable") {
			continue
		}
		b.WriteString(g)
		b.WriteString("\n\n")
	}
	return b.String()
}
