package detest

import (
	"fmt"
	"strings"
	"testing"
)

// An exploration cut short past a quarter of its runs estimates the runs
// left within a small factor of the runs one exploration to the end makes
// after the cut, with and without PartialOrder.
func TestRemainingEstimate(t *testing.T) {
	model := func(t *testing.T, s *Sim) { counterModel(s, true) }
	for _, name := range []string{"full tree", "partial order"} {
		t.Run(name, func(t *testing.T) {
			opts := []Option{MaxPreemptions(2)}
			if name == "partial order" {
				opts = []Option{PartialOrder()}
			}
			full, _ := explore(t, model, opts)
			if !full.Complete || full.Estimated {
				t.Fatalf("complete=%v estimated=%v, want a complete exploration without an estimate", full.Complete, full.Estimated)
			}
			for _, frac := range []int{4, 2} {
				cut := full.Runs / frac
				res, _ := explore(t, model, append(opts, MaxRuns(cut)))
				left := full.Runs - cut
				t.Logf("%d of %d runs: estimated %d left, %d in fact", cut, full.Runs, res.Remaining, left)
				if res.Complete || !res.Estimated || res.Remaining*3 < left || res.Remaining > left*3 {
					t.Errorf("%d of %d runs: estimated %d left, %d in fact", cut, full.Runs, res.Remaining, left)
				}
				if want := fmt.Sprintf("roughly %d runs left", res.Remaining); !strings.Contains(res.report(), want) {
					t.Errorf("report %q does not say %q", res.report(), want)
				}
			}
		})
	}
}

// An exploration cut after its first run is too early to estimate, as the
// first run alone would set the estimate.
func TestRemainingEstimateTooEarly(t *testing.T) {
	res, _ := explore(t, func(t *testing.T, s *Sim) { counterModel(s, true) }, []Option{MaxPreemptions(3), MaxRuns(1)})
	if !res.Estimated || res.Explored >= minExplored || res.Remaining != 0 {
		t.Fatalf("estimated=%v explored=%v remaining=%d, want too early", res.Estimated, res.Explored, res.Remaining)
	}
	if !strings.Contains(res.report(), "too early to estimate the runs left") {
		t.Errorf("report %q does not say it is too early", res.report())
	}
}

// A deep tree cut early leaves subtrees whose shares sum to 1 in floating
// point. That is too early to estimate, not an exploration with nothing left.
func TestRemainingEstimateDeepTree(t *testing.T) {
	f := newFrontier(1, 1)
	f.runs = 1
	f.stack = nil
	for depth := 1; depth <= 64; depth++ {
		p := make([]choice, depth)
		for i := range p {
			p[i] = choice{n: 2}
		}
		p[depth-1].picked = 1
		f.stack = append(f.stack, subtree{prefix: p})
	}
	if explored, left := f.remaining(); explored >= minExplored || left != 0 {
		t.Fatalf("explored=%v left=%d, want too early", explored, left)
	}
}

// A sampled exploration has no tree to estimate the rest of.
func TestRemainingEstimateNotSampled(t *testing.T) {
	res, _ := explore(t, func(t *testing.T, s *Sim) { counterModel(s, true) }, []Option{Random(1), MaxRuns(5)})
	if res.Complete || res.Estimated {
		t.Fatalf("complete=%v estimated=%v, want no estimate", res.Complete, res.Estimated)
	}
}
