package detest

import (
	"fmt"
	"strings"
	"testing"
)

// An exploration cut short estimates the runs left within a small factor of
// the runs one exploration to the end makes after the cut, with and without
// PartialOrder.
func TestRemainingEstimate(t *testing.T) {
	model := func(t *testing.T, s *Sim) { counterModel(s, true) }
	for _, name := range []string{"full tree", "partial order"} {
		t.Run(name, func(t *testing.T) {
			opts := []Option{MaxPreemptions(2)}
			if name == "partial order" {
				opts = []Option{PartialOrder()}
			}
			full, _ := explore(t, model, opts)
			if !full.Complete || full.Remaining != 0 {
				t.Fatalf("complete=%v remaining=%d, want a complete exploration without an estimate", full.Complete, full.Remaining)
			}
			for _, frac := range []int{4, 2} {
				cut := full.Runs / frac
				res, _ := explore(t, model, append(opts, MaxRuns(cut)))
				left := full.Runs - cut
				t.Logf("%d of %d runs: estimated %d left, %d in fact", cut, full.Runs, res.Remaining, left)
				if res.Complete || res.Remaining*3 < left || res.Remaining > left*3 {
					t.Errorf("%d of %d runs: estimated %d left, %d in fact", cut, full.Runs, res.Remaining, left)
				}
				if want := fmt.Sprintf("roughly %d runs left", res.Remaining); !strings.Contains(res.report(), want) {
					t.Errorf("report %q does not say %q", res.report(), want)
				}
			}
		})
	}
}

// A sampled exploration has no tree to estimate the rest of.
func TestRemainingEstimateNotSampled(t *testing.T) {
	res, _ := explore(t, func(t *testing.T, s *Sim) { counterModel(s, true) }, []Option{Random(1), MaxRuns(5)})
	if res.Complete || res.Remaining != 0 {
		t.Fatalf("complete=%v remaining=%d, want no estimate", res.Complete, res.Remaining)
	}
}
