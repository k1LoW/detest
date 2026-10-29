package detest

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/k1LoW/detest/internal/sqlir"
)

// progressLines runs watchProgress on f every 10ms and returns the lines it
// logs, with a stop that ends it.
func progressLines(f *frontier, estimate bool) (<-chan string, func()) {
	lines := make(chan string, 100)
	stop := watchProgress(func(args ...any) {
		select {
		case lines <- fmt.Sprint(args...):
		default:
		}
	}, f, 10*time.Millisecond, time.Now(), estimate)
	return lines, stop
}

func waitLine(t *testing.T, lines <-chan string, want string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case l := <-lines:
			if strings.Contains(l, want) {
				return l
			}
		case <-deadline:
			t.Fatalf("no progress line says %q", want)
		}
	}
}

// A progress line sums the workers' runs and gives the runs made since the
// line before, the deepest run of any worker, and the estimate the summary
// would give.
func TestProgressLine(t *testing.T) {
	f := newFrontier(2, 100)
	f.progress[0].runs.Store(3)
	f.progress[1].runs.Store(2)
	f.progress[0].maxDepth.Store(4)
	f.progress[1].maxDepth.Store(7)
	lines, stop := progressLines(f, true)
	defer stop()
	l := waitLine(t, lines, "explored 5 runs, 5 in the last 10ms (max depth 7)")
	if !strings.HasPrefix(l, "detest: still exploring at ") || !strings.Contains(l, "too early to estimate the runs left") {
		t.Errorf("line %q, want the time, the elapsed time and the estimate", l)
	}
	f.progress[0].runs.Store(10)
	waitLine(t, lines, "explored 12 runs, 7 in the last 10ms")
}

// Under IterativeBounding a progress line gives the preemptions up to which
// every run was made, the pass under way not counted, and no estimate; a
// resumed exploration's line counts the runs before the checkpoint too.
func TestProgressLineIterative(t *testing.T) {
	f := newFrontier(1, 100)
	f.iterative, f.level, f.prior = true, 2, 4
	f.progress[0].runs.Store(6)
	lines, stop := progressLines(f, false)
	defer stop()
	l := waitLine(t, lines, "explored 6 runs, 10 in all with the ones before the checkpoint")
	if !strings.HasSuffix(l, "; every run of up to 1 preemption explored") || strings.Contains(l, "estimate") {
		t.Errorf("line %q, want the level completed and no estimate", l)
	}
}

// An exploration that outlives the interval logs through the test's t while
// it runs, alone or with workers running as subtests, and stops logging
// before it returns, as a Log after the test ended would panic. Progress
// under a second is raised to one.
func TestExploreWithProgress(t *testing.T) {
	sqlir.Warmup() // so that MaxDuration is spent on runs
	model := func(t *testing.T, s *Sim) { counterModel(s, true) }
	for _, workers := range []int{1, 2} {
		t.Run(fmt.Sprintf("workers %d", workers), func(t *testing.T) {
			opts := []Option{IterativeBounding(), Workers(workers), MaxDuration(1500 * time.Millisecond), Progress(time.Nanosecond)}
			if res, _ := explore(t, model, opts); res.Runs == 0 {
				t.Fatal("no runs made")
			}
		})
	}
}
