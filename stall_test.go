package detest

import (
	"fmt"
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

// Stalling a process other than the current one leaves the current one
// current, so the stall gives no preemption away. Under MaxPreemptions(0)
// another process then steps in between two steps of a process only when
// that process stalled itself, which shows as time passing between them.
// a sleeps holding a row b then waits for, and its commit makes b runnable
// beside a, which is where a stall of the other one comes up. c can start
// only from then on, so only a free switch would start it before a's next
// step.
func TestStallKeepsThePreemptionBound(t *testing.T) {
	type entry struct {
		name string
		at   time.Time
	}
	var log []entry
	var committed bool
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE rows (id text PRIMARY KEY, n int NOT NULL)`)
		mustExec(t, db, `CREATE TABLE marks (id text PRIMARY KEY)`)
		s.Seed(func() {
			log, committed = log[:0], false
			mustExec(t, db, `INSERT INTO rows VALUES ('r', 0)`)
		})
		for _, name := range []string{"a", "b", "c"} {
			s.Manual(name, 1, func(p *Proc) error {
				ctx := p.Context()
				if name != "c" {
					tx, err := db.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, `UPDATE rows SET n = n + 1 WHERE id = 'r'`); err != nil {
						_ = tx.Rollback()
						return err
					}
					if name == "a" {
						// Lets b start and wait for the row a holds.
						time.Sleep(time.Second)
					}
					if err := tx.Commit(); err != nil {
						return err
					}
					committed = committed || name == "a"
				}
				for _, i := range []string{"1", "2"} {
					if _, err := db.ExecContext(ctx, `INSERT INTO marks VALUES ($1)`, name+i); err != nil {
						return err
					}
					log = append(log, entry{name, time.Now()})
				}
				return nil
			}, When(func() bool { return name != "c" || committed }))
		}
		s.AtQuiescence(func(*State) error {
			for i, first := range log {
				for j := i + 1; j < len(log); j++ {
					if log[j].name != first.name {
						continue
					}
					if j > i+1 && log[j].at.Equal(first.at) {
						return fmt.Errorf("%s stepped in between the steps of %s, which did not stall", log[i+1].name, first.name)
					}
					break
				}
			}
			return nil
		})
	}, MaxPreemptions(0), MaxStalls(1, time.Minute))
}
