package detest

import (
	"sync"
	"testing"
	"time"

	"github.com/k1LoW/detest/db/postgres"
)

// stallModel declares a process that inserts a row and records how much of
// the fake clock passed across the insert.
func stallModel(record func(time.Duration)) func(t *testing.T, s *Sim) {
	return func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE marks (id text PRIMARY KEY)`)
		s.Manual("writer", 1, func(p *Proc) error {
			start := time.Now()
			if _, err := db.ExecContext(p.Context(), `INSERT INTO marks VALUES ('m')`); err != nil {
				return err
			}
			record(time.Since(start))
			return nil
		})
	}
}

// A stall sleeps on the fake clock before the step it stands at, and the
// explorer also runs the step without one.
func TestStallAdvancesTheClock(t *testing.T) {
	seen := map[time.Duration]bool{}
	Explore(t, stallModel(func(d time.Duration) { seen[d] = true }), MaxStalls(1, time.Minute))
	if len(seen) != 2 || !seen[0] || !seen[time.Minute] {
		t.Fatalf("elapsed across the insert: %v, want 0 and 1m", seen)
	}
}

// Without MaxStalls nothing stalls.
func TestNoStallByDefault(t *testing.T) {
	seen := map[time.Duration]bool{}
	Explore(t, stallModel(func(d time.Duration) { seen[d] = true }))
	if len(seen) != 1 || !seen[0] {
		t.Fatalf("elapsed across the insert: %v, want only 0", seen)
	}
}

// Prioritized draws stalls at random steps as it draws crashes.
func TestPrioritizedStalls(t *testing.T) {
	seen := map[time.Duration]bool{}
	Explore(t, stallModel(func(d time.Duration) { seen[d] = true }), Prioritized(1, 2), MaxStalls(1, time.Minute), MaxRuns(50))
	if !seen[time.Minute] {
		t.Fatalf("elapsed across the insert: %v, want a run with 1m", seen)
	}
}

// Processes whose stalls end at the same instant wake in the runtime's order,
// and their steps still follow the schedule, so the exploration replays
// without nondeterminism and reaches both orders.
func TestStallsEndingTogetherKeepTheSchedule(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE log (id text PRIMARY KEY, v text NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO log VALUES ('l', '')`) })
		for _, name := range []string{"a", "b"} {
			s.Manual(name, 1, func(p *Proc) error {
				if _, err := db.ExecContext(p.Context(), `SELECT v FROM log WHERE id = 'l'`); err != nil {
					return err
				}
				_, err := db.ExecContext(p.Context(), `UPDATE log SET v = v || $1 WHERE id = 'l'`, name)
				return err
			})
		}
		s.AtQuiescence(func(st *State) error {
			row, _ := st.Row(store, "log", "l")
			mu.Lock()
			seen[row.Str("v")] = true
			mu.Unlock()
			return nil
		})
	}, MaxStalls(2, time.Minute), Workers(2))
	if !seen["ab"] || !seen["ba"] {
		t.Fatalf("final values: %v, want both orders", seen)
	}
}

func TestMaxStallsRejectsDurations(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second, 25 * time.Hour} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("MaxStalls(1, %s) did not panic", d)
				}
			}()
			MaxStalls(1, d)
		}()
	}
}
