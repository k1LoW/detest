package detest

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/k1LoW/detest/postgres"
)

// counterModel declares two handlers that read-check-write a counter, with the
// update guarded by CAS or not. Every declaration runs once per worker, so it
// keeps no state outside m.
func counterModel(sim *Sim, cas bool) {
	db, store := sim.DB("app", postgres.New())
	sim.Seed(func() {
		_, _ = db.Exec(`INSERT INTO "counters" ("id","n") VALUES ($1,$2)`, "c", int64(0))
	})
	handler := func(p *Proc) error {
		for {
			var n int64
			if err := db.QueryRow(`SELECT "n" FROM "counters" WHERE "id" = $1`, "c").Scan(&n); err != nil {
				return err
			}
			if !cas {
				_, err := db.Exec(`UPDATE "counters" SET "n"=$1 WHERE "id" = $2`, n+1, "c")
				return err
			}
			res, err := db.Exec(`UPDATE "counters" SET "n"=$1 WHERE "id" = $2 AND "n" = $3`, n+1, "c", n)
			if err != nil {
				return err
			}
			if k, _ := res.RowsAffected(); k == 1 {
				return nil
			}
		}
	}
	for i := range 3 {
		sim.Manual(fmt.Sprintf("inc_%d", i), 1, handler)
	}
	sim.AtQuiescence(func(s *State) error {
		row, _ := s.Row(store, "counters", "c")
		if row.Int64("n") != 3 {
			return fmt.Errorf("lost update: n = %d", row.Int64("n"))
		}
		return nil
	})
}

func TestWorkersFindViolation(t *testing.T) {
	Explore(t, func(t *testing.T, sim *Sim) {
		counterModel(sim, false)
		sim.ExpectViolation("lost update")
	}, Workers(4))
}

func TestWorkersExploreEverySchedule(t *testing.T) {
	Explore(t, func(t *testing.T, sim *Sim) { counterModel(sim, true) }, Workers(4), MaxPreemptions(2))
}

// With several violating schedules, parallel workers report the same one as
// a single worker: the first in depth-first order.
func TestWorkersReportTheFirstViolation(t *testing.T) {
	model := func(t *testing.T, sim *Sim) { counterModel(sim, false) }
	var progress atomic.Int64
	one, _ := exploreBubble(t, model, nil, &progress, nil, 0)
	if !one.Violated {
		t.Fatal("expected a violation")
	}
	for i := range 20 {
		got, _ := exploreWorkers(t, model, nil, &progress, newFrontier(4, 200000), 4)
		if got.Schedule != one.Schedule || got.Err.Error() != one.Err.Error() {
			t.Fatalf("attempt %d: 4 workers reported %s (%v), 1 worker %s (%v)", i, got.Schedule, got.Err, one.Schedule, one.Err)
		}
	}
}
