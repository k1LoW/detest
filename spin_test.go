package detest

import (
	"strings"
	"testing"
)

// spinModel has a waiter that busy-waits on a flag through a yield point and
// a setter that sets it (#45).
func spinModel(withSetter bool) func(t *testing.T, s *Sim) {
	return func(t *testing.T, s *Sim) {
		flag := false
		s.Seed(func() { flag = false })
		s.Manual("waiter", 1, func(p *Proc) error {
			for !flag {
				p.Step("wait")
			}
			return nil
		})
		if withSetter {
			s.Manual("setter", 1, func(p *Proc) error {
				p.Step("set")
				flag = true
				return nil
			})
		}
	}
}

// A waiter that keeps yielding without changing anything gives way to the
// setter, as a real scheduler would let it run, so every run ends.
func TestBusyWaitGivesWayToOthers(t *testing.T) {
	res, _ := exploreBubble(t, spinModel(true), nil, nil, 0)
	if res.Violated || !res.Complete {
		t.Fatalf("want a complete exploration without violation, got %s", res.report())
	}
}

// A process that does more work alone than MaxSpins allows passes once
// MaxSpins is raised above it.
func TestMaxSpinsLetsLongWorkAlone(t *testing.T) {
	model := func(t *testing.T, s *Sim) {
		s.Manual("worker", 1, func(p *Proc) error {
			for i := range 150 {
				p.Step("work %d", i)
			}
			return nil
		})
	}
	res, _ := exploreBubble(t, model, nil, nil, 0)
	if !res.Violated || res.Kind != "progress" {
		t.Fatalf("want 150 steps alone reported under the default, got %s", res.report())
	}
	res, _ = exploreBubble(t, model, []Option{MaxSpins(200)}, nil, 0)
	if res.Violated {
		t.Fatalf("want no violation with MaxSpins(200), got %s", res.report())
	}
}

// Giving way is the scheduler's fairness, not a preemption to explore, so
// the waiter gives way even with no preemption budget.
func TestBusyWaitGivesWayWithoutPreemptions(t *testing.T) {
	res, _ := exploreBubble(t, spinModel(true), []Option{MaxPreemptions(0)}, nil, 0)
	if res.Violated || !res.Complete {
		t.Fatalf("want a complete exploration without violation, got %s", res.report())
	}
}

// Two processes that each wait for a flag only the other would set take turns
// at spinning for ever, which is a progress violation naming them.
func TestBusyWaitTakingTurnsIsAViolation(t *testing.T) {
	model := func(t *testing.T, s *Sim) {
		a, b := false, false
		s.Seed(func() { a, b = false, false })
		s.Manual("x", 1, func(p *Proc) error {
			for !b {
				p.Step("x waits")
			}
			a = true
			return nil
		})
		s.Manual("y", 1, func(p *Proc) error {
			for !a {
				p.Step("y waits")
			}
			b = true
			return nil
		})
	}
	res, _ := exploreBubble(t, model, nil, nil, 0)
	if !res.Violated || res.Kind != "progress" || !strings.Contains(res.Err.Error(), "x#") || !strings.Contains(res.Err.Error(), "y#") {
		t.Fatalf("want a progress violation naming x and y, got %s", res.report())
	}
}

// A waiter gives way to a process doing work without committing as many
// times as that work takes steps, within MaxSpins.
func TestBusyWaitOnLongerWork(t *testing.T) {
	model := func(t *testing.T, s *Sim) {
		flag := false
		s.Seed(func() { flag = false })
		s.Manual("waiter", 1, func(p *Proc) error {
			for !flag {
				p.Step("wait")
			}
			return nil
		})
		s.Manual("worker", 1, func(p *Proc) error {
			for i := range 50 {
				p.Step("work %d", i)
			}
			flag = true
			return nil
		})
	}
	res, _ := exploreBubble(t, model, []Option{MaxPreemptions(1)}, nil, 0)
	if res.Violated {
		t.Fatalf("want no violation, got %s", res.report())
	}
}

// With nothing else able to run, the waiter would spin forever, which is
// reported as a progress violation naming it.
func TestBusyWaitAloneIsAViolation(t *testing.T) {
	res, _ := exploreBubble(t, spinModel(false), nil, nil, 0)
	if !res.Violated || res.Kind != "progress" || !strings.Contains(res.Err.Error(), "waiter") {
		t.Fatalf("want a progress violation naming the waiter, got %s", res.report())
	}
}
