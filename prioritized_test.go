package detest

import (
	"fmt"
	"strings"
	"testing"
)

// windowModel breaks only when b runs right after a's 20th step and before
// its 21st: a's 20 steps must go uninterrupted and then be preempted once.
func windowModel(t *testing.T, s *Sim) {
	steps, seen := 0, -1
	s.Seed(func() { steps, seen = 0, -1 })
	s.Manual("a", 1, func(p *Proc) error {
		for i := range 21 {
			p.Step("a %d", i)
			steps++
		}
		return nil
	})
	s.Manual("b", 1, func(p *Proc) error {
		p.Step("b")
		seen = steps
		return nil
	})
	s.AtQuiescence(func(st *State) error {
		if seen == 20 {
			return fmt.Errorf("b ran in the window")
		}
		return nil
	})
}

// Random rarely lets a run go on for 20 steps, while Prioritized runs each
// process on and switches at depth-1 places.
func TestPrioritizedReachesALateWindow(t *testing.T) {
	random, _ := exploreBubble(t, windowModel, []Option{Random(1), MaxRuns(2000)}, nil, 0)
	if random.Violated {
		t.Fatalf("Random found the window, which this test expects it to miss: %s", random.report())
	}
	prio, _ := exploreBubble(t, windowModel, []Option{Prioritized(1, 2), MaxRuns(2000)}, nil, 0)
	if !prio.Violated || !strings.Contains(prio.Err.Error(), "b ran in the window") {
		t.Fatalf("want the window found, got %s", prio.report())
	}
}

// The second switch drops b below a, which the first switch stopped, so a
// runs again while b can still run: a, then b, then a again.
func TestPrioritizedSwitchesBack(t *testing.T) {
	model := func(t *testing.T, s *Sim) {
		aSteps, bSteps, bSawA, aSawB := 0, 0, -1, -1
		s.Seed(func() { aSteps, bSteps, bSawA, aSawB = 0, 0, -1, -1 })
		s.Manual("a", 1, func(p *Proc) error {
			for i := range 10 {
				p.Step("a %d", i)
				aSteps++
				if aSteps == 6 {
					aSawB = bSteps
				}
			}
			return nil
		})
		s.Manual("b", 1, func(p *Proc) error {
			for i := range 10 {
				p.Step("b %d", i)
				bSteps++
				if bSteps == 1 {
					bSawA = aSteps
				}
			}
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			if bSawA == 5 && aSawB == 5 {
				return fmt.Errorf("a ran 5 steps, b 5 steps, then a again")
			}
			return nil
		})
	}
	res, _ := exploreBubble(t, model, []Option{Prioritized(1, 3), MaxRuns(5000)}, nil, 0)
	if !res.Violated {
		t.Fatalf("want a, b, a found, got %s", res.report())
	}
}

// A step with a single option takes no choice, so the choices a lone
// process makes are drawn as under Random.
func TestPrioritizedDrawsALoneProcessChoices(t *testing.T) {
	model := func(t *testing.T, s *Sim) {
		picked := 0
		s.Seed(func() { picked = 0 })
		s.Manual("a", 1, func(p *Proc) error {
			p.Step("a")
			picked = p.Choose("x", 2)
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			if picked == 1 {
				return fmt.Errorf("picked the second option")
			}
			return nil
		})
	}
	res, _ := exploreBubble(t, model, []Option{Prioritized(1, 2), MaxRuns(50)}, nil, 0)
	if !res.Violated {
		t.Fatalf("want a run picking the second option, got %s", res.report())
	}
}

// The run k is measured on is no run of the exploration, so a condition
// only it met is not reported as reached.
func TestPrioritizedMeasuringRunReachesNothing(t *testing.T) {
	model := func(t *testing.T, s *Sim) {
		s.Manual("a", 1, func(p *Proc) error {
			p.Step("a")
			return nil
		})
		s.Sometimes("always", func(st *State) bool { return true })
	}
	res, _ := exploreBubble(t, model, []Option{Prioritized(1, 2), MaxRuns(0)}, nil, 0)
	if res.Runs != 0 || len(res.Unreached) != 1 {
		t.Fatalf("want no run and the condition unreached, got %s", res.report())
	}
}

// The run k is measured on takes no fault, so a loss listed before the
// delivery does not end it early and put every run's loss before it.
func TestPrioritizedDeliversOnALossyQueue(t *testing.T) {
	model := func(t *testing.T, s *Sim) {
		delivered := false
		s.Seed(func() { delivered = false })
		q := s.Queue("orders", Losses(1))
		s.Manual("checkout", 1, func(p *Proc) error {
			q.Enqueue(p, Msg{"id": "o1"})
			return nil
		})
		s.OnMessage("complete", q, func(p *Proc, msg Msg) error {
			p.Step("complete %s", msg.Str("id"))
			delivered = true
			return nil
		})
		s.Sometimes("delivered", func(st *State) bool { return delivered })
	}
	res, _ := exploreBubble(t, model, []Option{Prioritized(1, 2), MaxRuns(50)}, nil, 0)
	if res.Violated || len(res.Unreached) != 0 {
		t.Fatalf("want a run delivering the message, got %s", res.report())
	}
}

func TestPrioritizedFindsViolation(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		counterModel(s, false)
		s.ExpectViolation("lost update")
	}, Prioritized(1, 2))
}

// Each run draws from the seed and its index, and the run that measures k is
// made alike by every worker, so workers report what one worker reports.
func TestPrioritizedWorkersReportTheFirstViolation(t *testing.T) {
	t.Setenv("DETEST_SHARD", "")
	opts := []Option{Prioritized(3, 2), MaxRuns(2000)}
	one, _ := exploreBubble(t, windowModel, opts, nil, 0)
	if !one.Violated || one.Runs <= 4 {
		t.Fatalf("want a violation after more runs than workers, got %s", one.report())
	}
	for i := range 10 {
		f := newFrontier(4, 200000)
		f.random = true
		got, _ := exploreWorkers(t, windowModel, opts, f, 4)
		if got.Schedule != one.Schedule || got.Err.Error() != one.Err.Error() {
			t.Fatalf("attempt %d: 4 workers reported %s (%v), 1 worker %s (%v)", i, got.Schedule, got.Err, one.Schedule, one.Err)
		}
	}
}

func TestPrioritizedViolationReplays(t *testing.T) {
	found, _ := exploreBubble(t, windowModel, []Option{Prioritized(1, 2), MaxRuns(2000)}, nil, 0)
	if !found.Violated || !strings.Contains(found.report(), "of prioritized seed 1 at depth 2,") {
		t.Fatalf("want a violation naming the seed and depth, got %s", found.report())
	}
	replayed, _ := exploreBubble(t, windowModel, []Option{Replay(found.Schedule)}, nil, 0)
	if !replayed.Violated || replayed.Err.Error() != found.Err.Error() {
		t.Fatalf("replaying %s: got %s, want %v", found.Schedule, replayed.report(), found.Err)
	}
}

func TestPrioritizedDetectsNondeterminism(t *testing.T) {
	res, _ := exploreBubble(t, ticketModel(false), []Option{Prioritized(1, 2)}, nil, 0)
	if res.Fatal == nil || !strings.Contains(res.Fatal.Error(), "the operations before choice") {
		t.Fatalf("got %v, want a nondeterminism error", res.Fatal)
	}
}

// Crashes happen at steps drawn among the first k, not at every step, so a
// crash lands inside a long process as well as at its start.
func TestPrioritizedCrashesAtDrawnSteps(t *testing.T) {
	model := func(t *testing.T, s *Sim) {
		steps := 0
		s.Seed(func() { steps = 0 })
		s.Manual("a", 1, func(p *Proc) error {
			for i := range 10 {
				p.Step("a %d", i)
				steps++
			}
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			if steps >= 5 && steps < 10 {
				return fmt.Errorf("a crashed after %d steps", steps)
			}
			return nil
		})
	}
	res, _ := exploreBubble(t, model, []Option{Prioritized(1, 1), MaxCrashes(1), MaxRuns(200)}, nil, 0)
	if !res.Violated || !strings.Contains(res.Err.Error(), "a crashed after") {
		t.Fatalf("want a crash in a's second half, got %s", res.report())
	}
}

// Without switches a run goes to the highest priority at every step, so each
// process runs to its end before another starts.
func TestPrioritizedDepthOneDoesNotSwitch(t *testing.T) {
	res, _ := exploreBubble(t, windowModel, []Option{Prioritized(1, 1), MaxRuns(500)}, nil, 0)
	if res.Violated {
		t.Fatalf("want no run switching inside a, got %s", res.report())
	}
}
