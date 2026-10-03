package detest

import (
	"testing"
	"testing/synctest"
)

func departures(choices []choice) int {
	n := 0
	for _, c := range choices {
		if c.picked != 0 {
			n++
		}
	}
	return n
}

// Of all the schedules that lose an update, the one departing most from the
// default shrinks to one that departs less and breaks the same way.
func TestShrinkSimplifiesTheSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSim(t)
		defer s.closeSQL()
		counterModel(s, false)
		s.Manual("noise", 1, func(p *Proc) error {
			p.Step("unrelated work")
			p.Step("more unrelated work")
			return nil
		})
		s.frozen = true

		var worst *run
		var worstV *violation
		stack := [][]choice{nil}
		for len(stack) > 0 {
			prefix := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			r := s.newRun(prefix)
			if v := r.execute(); v != nil {
				if worst == nil || departures(r.choices) > departures(worst.choices) {
					worst, worstV = r, v
				}
				continue
			}
			stack = append(stack, s.children(r.choices, len(prefix))...)
		}
		if worst == nil {
			t.Fatal("no violating schedule")
		}
		r, v := s.shrink(worst, worstV)
		if departures(r.choices) >= departures(worst.choices) {
			t.Fatalf("shrinking kept %d departures from the default", departures(r.choices))
		}
		if v.err.Error() != worstV.err.Error() {
			t.Fatalf("shrunk to another violation: %v, was %v", v.err, worstV.err)
		}
		t.Logf("%d departures shrunk to %d", departures(worst.choices), departures(r.choices))
	})
}
