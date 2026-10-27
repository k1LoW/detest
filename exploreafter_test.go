package detest

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// leadModel has prep take the counter from 0 to 3 in three statements
// before two processes race to increment it, with or without compare-and-set.
// With after, the exploration starts once the counter is 3, and leads
// records the choices every run made before that.
func leadModel(cas, after bool, leads *[][]int) func(t *testing.T, s *Sim) {
	return func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE counters (id text PRIMARY KEY, n bigint NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		s.Seed(func() {
			_, _ = db.Exec(`INSERT INTO counters (id, n) VALUES ('c', 0)`)
		})
		s.Manual("prep", 1, func(p *Proc) error {
			for range 3 {
				if _, err := db.Exec(`UPDATE counters SET n = n + 1 WHERE id = 'c'`); err != nil {
					return err
				}
			}
			return nil
		})
		for i := range 2 {
			s.Manual(fmt.Sprintf("inc_%d", i), 1, func(p *Proc) error {
				var n int64
				if err := db.QueryRow(`SELECT n FROM counters WHERE id = 'c'`).Scan(&n); err != nil {
					return err
				}
				q := `UPDATE counters SET n = $1 WHERE id = 'c'`
				args := []any{n + 1}
				if cas {
					q += ` AND n = $2`
					args = append(args, n)
				}
				_, err := db.Exec(q, args...)
				return err
			})
		}
		if !cas {
			s.AtQuiescence(func(st *State) error {
				row, _ := st.Row(store, "counters", "c")
				if n := row.Int64("n"); n != 5 {
					return fmt.Errorf("lost update: n = %d", n)
				}
				return nil
			})
		}
		if after {
			s.ExploreAfter(func(st *State) bool {
				row, _ := st.Row(store, "counters", "c")
				return row.Int64("n") >= 3
			})
		}
		if leads != nil {
			s.depObserve = func(r *run, v *violation) {
				var picks []int
				for _, c := range r.choices[:r.leadEnd] {
					picks = append(picks, c.picked)
				}
				*leads = append(*leads, picks)
			}
		}
	}
}

// sameLeads fails unless every run made the same choices before the state,
// at least one.
func sameLeads(t *testing.T, leads [][]int) {
	t.Helper()
	if len(leads) == 0 || len(leads[0]) == 0 {
		t.Fatalf("want runs that made choices before the state, got %v", leads)
	}
	for _, l := range leads[1:] {
		if !slices.Equal(l, leads[0]) {
			t.Fatalf("runs made different choices before the state: %v and %v", leads[0], l)
		}
	}
}

// The runs branch only once the state holds, so the exploration is complete
// in fewer runs, all of them through one fixed schedule up to the state.
func TestExploreAfterBranchesOnlyFromTheState(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"depth first", nil},
		{"PartialOrder", []Option{PartialOrder()}},
		{"IterativeBounding", []Option{IterativeBounding()}},
		{"IterativeBounding with PartialOrder", []Option{IterativeBounding(), PartialOrder()}},
		{"MaxPreemptions", []Option{MaxPreemptions(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			whole, _ := exploreBubble(t, leadModel(true, false, nil), tc.opts, nil, 0)
			var leads [][]int
			after, _ := exploreBubble(t, leadModel(true, true, &leads), tc.opts, nil, 0)
			if whole.Violated || after.Violated || !whole.Complete || !after.Complete {
				t.Fatalf("want complete explorations without violation, got %s\nand %s", whole.report(), after.report())
			}
			if after.Runs >= whole.Runs {
				t.Fatalf("want fewer runs from the state than %d, got %d", whole.Runs, after.Runs)
			}
			sameLeads(t, leads)
		})
	}
}

// The race past the state is found, and its schedule, the choices before
// the state included, replays it.
func TestExploreAfterFindsAndReplaysTheRace(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"depth first", nil},
		{"PartialOrder", []Option{PartialOrder()}},
		{"Workers", []Option{Workers(4)}},
		{"Random", []Option{Random(1), MaxRuns(200)}},
		{"Prioritized", []Option{Prioritized(1, 2), MaxRuns(200)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var res *result
			if n := workerCount(tc.opts); n > 1 {
				res, _ = exploreWorkers(t, leadModel(false, true, nil), tc.opts, newFrontier(n, 1000000), n)
			} else {
				res, _ = exploreBubble(t, leadModel(false, true, nil), tc.opts, nil, 0)
			}
			if !res.Violated || !strings.Contains(res.Err.Error(), "lost update") {
				t.Fatalf("want the lost update found, got %s", res.report())
			}
			if !strings.Contains(res.report(), afterNote) {
				t.Fatalf("want the report to say what was not explored, got %s", res.report())
			}
			Explore(t, func(t *testing.T, s *Sim) {
				leadModel(false, true, nil)(t, s)
				s.ExpectViolation("lost update")
			}, append(slices.Clip(tc.opts), Replay(res.Schedule))...)
		})
	}
}

// Random and Prioritized draw nothing before the state, so every run gets
// there by the same choices.
func TestExploreAfterDrawsAfterTheState(t *testing.T) {
	for _, opt := range []Option{Random(1), Prioritized(1, 3)} {
		var leads [][]int
		res, _ := exploreBubble(t, leadModel(true, true, &leads), []Option{opt, MaxRuns(50)}, nil, 0)
		if res.Violated {
			t.Fatalf("want no violation, got %s", res.report())
		}
		sameLeads(t, leads)
	}
}

// A run that ends before the state holds explored nothing, and fails rather
// than passes.
func TestExploreAfterFailsWhenTheStateNeverHolds(t *testing.T) {
	model := func(t *testing.T, s *Sim) {
		leadModel(true, false, nil)(t, s)
		s.ExploreAfter(func(st *State) bool { return false })
	}
	res, _ := exploreBubble(t, model, nil, nil, 0)
	if res.Fatal == nil || !strings.Contains(res.Fatal.Error(), "never held") {
		t.Fatalf("want the exploration to fail, got %v: %s", res.Fatal, res.report())
	}
}

// Before the state no fault is taken, though a loss is listed before the
// delivery that leads to it.
func TestExploreAfterTakesNoFaultBeforeTheState(t *testing.T) {
	model := func(t *testing.T, s *Sim) {
		delivered, raced := false, 0
		q := s.Queue("orders", Losses(1))
		s.Seed(func() {
			delivered, raced = false, 0
			q.SeedMsg(Msg{"id": "o1"})
		})
		s.OnMessage("complete", q, func(p *Proc, msg Msg) error {
			p.Step("complete %s", msg.Str("id"))
			delivered = true
			return nil
		})
		for i := range 2 {
			s.Manual(fmt.Sprintf("racer_%d", i), 1, func(p *Proc) error {
				p.Step("racer %d", i)
				raced++
				return nil
			}, After(func(st *State) bool { return delivered }))
		}
		s.ExploreAfter(func(st *State) bool { return delivered })
		s.Sometimes("raced", func(st *State) bool { return raced == 2 })
	}
	res, _ := exploreBubble(t, model, []Option{MaxCrashes(1)}, nil, 0)
	if res.Fatal != nil || res.Violated || len(res.Unreached) != 0 {
		t.Fatalf("want the state reached and the race run, got %v: %s", res.Fatal, res.report())
	}
}

// Shrinking keeps the choices before the state, though the first option of
// one is a loss the run would break the same way after.
func TestExploreAfterShrinksNothingBeforeTheState(t *testing.T) {
	model := func(t *testing.T, s *Sim) {
		q := s.Queue("orders", Losses(1))
		s.Seed(func() { q.SeedMsg(Msg{"id": "o1"}) })
		s.OnMessage("complete", q, func(p *Proc, msg Msg) error {
			p.Step("complete %s", msg.Str("id"))
			return nil
		})
		s.Manual("racer", 1, func(p *Proc) error {
			p.Step("racer")
			return nil
		})
		s.ExploreAfter(func(st *State) bool { return len(st.Queue(q)) == 0 })
		s.AtQuiescence(func(st *State) error { return fmt.Errorf("always broken") })
	}
	res, _ := exploreBubble(t, model, nil, nil, 0)
	if !res.Violated || !strings.Contains(res.Trace, "receives") {
		t.Fatalf("want the violation reported with the message delivered, got %s", res.report())
	}
}

// The schedule before the state is the same whatever the preemption bound,
// so that the passes of IterativeBounding explore from one state.
func TestExploreAfterLeadsTheSameWayUnderAnyBound(t *testing.T) {
	var free, bounded [][]int
	exploreBubble(t, leadModel(true, true, &free), nil, nil, 0)
	exploreBubble(t, leadModel(true, true, &bounded), []Option{MaxPreemptions(0)}, nil, 0)
	sameLeads(t, append(free, bounded...))
}

// Random checks its runs for nondeterminism against earlier runs at the
// shallow choices, which under ExploreAfter are counted from its state, so a
// long schedule before it leaves the check in place.
func TestExploreAfterKeepsRandomsNondeterminismCheck(t *testing.T) {
	model := func(t *testing.T, s *Sim) {
		prepared, runs := 0, 0 // runs is left unreset by the seed on purpose
		s.Seed(func() { prepared = 0 })
		s.Manual("prep", 1, func(p *Proc) error {
			for i := range 15 {
				p.Step("prep %d", i)
				prepared++
			}
			return nil
		})
		// Ready alongside prep, so that every step of prep is a choice.
		s.Manual("idle", 1, func(p *Proc) error {
			p.Step("idle")
			return nil
		})
		s.Manual("a", 1, func(p *Proc) error {
			runs++
			for i := range 1 + runs%3 {
				p.Step("a %d", i)
			}
			return nil
		}, After(func(st *State) bool { return prepared == 15 }))
		s.Manual("b", 1, func(p *Proc) error {
			p.Step("b")
			return nil
		}, After(func(st *State) bool { return prepared == 15 }))
		s.ExploreAfter(func(st *State) bool { return prepared == 15 })
	}
	res, _ := exploreBubble(t, model, []Option{Random(1), MaxRuns(50)}, nil, 0)
	if res.Fatal == nil || !strings.Contains(res.Fatal.Error(), "nondeterministic") {
		t.Fatalf("want the nondeterminism found, got %v: %s", res.Fatal, res.report())
	}
}

// A run that breaks before the state branches nowhere, so that an
// exploration going on past it makes no run that leaves the fixed schedule.
func TestExploreAfterBranchesNowhereInARunBrokenBeforeTheState(t *testing.T) {
	model := func(t *testing.T, s *Sim) {
		leadModel(true, true, nil)(t, s)
		store := s.dbs[0]
		s.Always(func(st *State) error {
			row, _ := st.Row(store, "counters", "c")
			if row.Int64("n") == 2 {
				return fmt.Errorf("halfway")
			}
			return nil
		})
	}
	for _, opts := range [][]Option{{exploreAll()}, {exploreAll(), PartialOrder()}} {
		res, _ := exploreBubble(t, model, opts, nil, 0)
		if res.Fatal != nil || !res.Violated || !strings.Contains(res.Err.Error(), "halfway") {
			t.Fatalf("want the violation before the state alone, got %v: %s", res.Fatal, res.report())
		}
	}
}

// A checkpoint resumes only under the ExploreAfter it was saved with, as the
// schedule before the state takes no fault and spends no preemption.
func TestExploreAfterCheckpointIsRefusedTheOtherWay(t *testing.T) {
	for _, saved := range []bool{true, false} {
		path := filepath.Join(t.TempDir(), "ckpt.json")
		f := newFrontier(1, 2)
		exploreBubble(t, leadModel(true, saved, nil), nil, f, 0)
		if err := f.save(path); err != nil {
			t.Fatal(err)
		}
		g := newFrontier(1, 1000000)
		if err := g.load(path); err != nil {
			t.Fatal(err)
		}
		res, _ := exploreBubble(t, leadModel(true, !saved, nil), nil, g, 0)
		if res.Fatal == nil || !strings.Contains(res.Fatal.Error(), "ExploreAfter") {
			t.Fatalf("want the checkpoint saved with ExploreAfter=%v refused, got %v", saved, res.Fatal)
		}
	}
}
