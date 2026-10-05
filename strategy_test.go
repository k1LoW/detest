package detest

import (
	"strings"
	"testing"
)

func TestRandomFindsViolation(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		counterModel(s, false)
		s.ExpectViolation("lost update")
	}, Random(1))
}

// Without a violation, a random exploration runs until MaxRuns and is never
// complete.
func TestRandomRunsUntilMaxRuns(t *testing.T) {
	model := func(t *testing.T, s *Sim) { counterModel(s, true) }
	res, _ := exploreBubble(t, model, []Option{Random(1), MaxRuns(50)}, nil, 0)
	if res.Violated || res.Complete || res.Runs != 50 {
		t.Fatalf("want 50 runs, incomplete and without violation, got %s", res.report())
	}
}

// Parallel workers report the violation a single worker reports: the one of
// the lowest run index.
func TestRandomWorkersReportTheFirstViolation(t *testing.T) {
	model := func(t *testing.T, s *Sim) { counterModel(s, false) }
	// Under seed 17 the first runs pass, so the 4 workers find violations
	// in other runs before the first one.
	opts := []Option{Random(17)}
	one, _ := exploreBubble(t, model, opts, nil, 0)
	if !one.Violated || one.Runs <= 4 {
		t.Fatalf("want a violation after more runs than workers, got %s", one.report())
	}
	for i := range 20 {
		f := newFrontier(4, 200000)
		f.random = true
		got, _ := exploreWorkers(t, model, opts, f, 4)
		if got.Schedule != one.Schedule || got.Err.Error() != one.Err.Error() {
			t.Fatalf("attempt %d: 4 workers reported %s (%v), 1 worker %s (%v)", i, got.Schedule, got.Err, one.Schedule, one.Err)
		}
	}
}

func TestRandomViolationReplays(t *testing.T) {
	model := func(t *testing.T, s *Sim) { counterModel(s, false) }
	found, _ := exploreBubble(t, model, []Option{Random(3)}, nil, 0)
	if !found.Violated {
		t.Fatal("expected a violation")
	}
	replayed, _ := exploreBubble(t, model, []Option{Replay(found.Schedule)}, nil, 0)
	if !replayed.Violated || replayed.Err.Error() != found.Err.Error() {
		t.Fatalf("replaying %s: got %s, want %v", found.Schedule, replayed.report(), found.Err)
	}
}

// Of DepthFirst and Random, the one passed last applies.
func TestStrategyPassedLastApplies(t *testing.T) {
	model := func(t *testing.T, s *Sim) { counterModel(s, true) }
	res, _ := exploreBubble(t, model, []Option{Random(1), DepthFirst(), MaxPreemptions(1)}, nil, 0)
	if !res.Complete {
		t.Fatalf("want a complete depth-first exploration, got %s", res.report())
	}
	res, _ = exploreBubble(t, model, []Option{DepthFirst(), Random(1), MaxRuns(10)}, nil, 0)
	if res.Complete || res.Runs != 10 {
		t.Fatalf("want 10 random runs, got %s", res.report())
	}
}

// A random run replays no prefix, so state a seed does not reset is caught
// by comparing it with the previous run.
func TestRandomDetectsNondeterminism(t *testing.T) {
	res, _ := exploreBubble(t, ticketModel(false), []Option{Random(1)}, nil, 0)
	if res.Fatal == nil || !strings.Contains(res.Fatal.Error(), "the operations before choice") {
		t.Fatalf("got %v, want a nondeterminism error", res.Fatal)
	}
}

func TestRandomAcceptsDeterministicRuns(t *testing.T) {
	res, _ := exploreBubble(t, ticketModel(true), []Option{Random(1), MaxRuns(50)}, nil, 0)
	if res.Fatal != nil {
		t.Fatal(res.Fatal)
	}
	if res.Runs != 50 {
		t.Fatalf("explored %d runs, want 50", res.Runs)
	}
}
