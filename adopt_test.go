package detest

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k1LoW/detest/postgres"
)

// incr adds one to the counter with a read and a write, which lose an update
// when two run interleaved, or with a compare-and-set that retries.
func incr(db *sql.DB, cas bool) error {
	for {
		var n int64
		if err := db.QueryRow(`SELECT "n" FROM "counters" WHERE "id" = 'c'`).Scan(&n); err != nil {
			return err
		}
		if !cas {
			_, err := db.Exec(`UPDATE "counters" SET "n" = $1 WHERE "id" = 'c'`, n+1)
			return err
		}
		res, err := db.Exec(`UPDATE "counters" SET "n" = $1 WHERE "id" = 'c' AND "n" = $2`, n+1, n)
		if err != nil {
			return err
		}
		if k, _ := res.RowsAffected(); k == 1 {
			return nil
		}
	}
}

// fanOut runs fn in n goroutines and waits for them, as errgroup does.
func fanOut(n int, fn func() error) error {
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() { errs[i] = fn() })
	}
	wg.Wait()
	return errors.Join(errs...)
}

func counterSim(s *Sim, goroutines int, cas bool) {
	db, store := s.DB("app", postgres.New())
	s.Seed(func() {
		_, _ = db.Exec(`INSERT INTO "counters" ("id","n") VALUES ('c', 0)`)
	})
	s.Manual("pod", 1, func(p *Proc) error {
		return fanOut(goroutines, func() error { return incr(db, cas) })
	})
	s.AtQuiescence(func(st *State) error {
		row, _ := st.Row(store, "counters", "c")
		if n := row.Int64("n"); n != int64(goroutines) {
			return fmt.Errorf("lost update: n = %d", n)
		}
		return nil
	})
}

func TestAdoptedGoroutinesInterleave(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		counterSim(s, 2, false)
		s.ExpectViolation("lost update")
	})
}

func TestAdoptedGoroutinesNamedAfterParent(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) { counterSim(s, 2, false) }, nil, nil, 0)
	if !res.Violated {
		t.Fatal("expected a violation")
	}
	for _, name := range []string{"pod#1.1 ", "pod#1.2 ", "starts goroutine pod#1.1"} {
		if !strings.Contains(res.Trace, name) {
			t.Errorf("the trace does not name %q:\n%s", name, res.Trace)
		}
	}
}

// Goroutines arrive in detest in whatever order the runtime runs them. A
// complete exploration over parallel workers checks that their order as
// processes does not depend on it, as detest fails a replay that takes another
// path. Each goroutine counts its own row, so that one taken for its sibling
// writes another row than the schedule recorded.
func TestAdoptedGoroutinesDeterministic(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		s.Seed(func() {
			for i := range 3 {
				_, _ = db.Exec(`INSERT INTO "counters" ("id","n") VALUES ($1, 0)`, fmt.Sprint(i))
			}
		})
		s.Manual("pod", 1, func(p *Proc) error {
			var wg sync.WaitGroup
			for i := range 3 {
				wg.Go(func() {
					for range 2 {
						_, _ = db.Exec(`UPDATE "counters" SET "n" = "n" + 1 WHERE "id" = $1`, fmt.Sprint(i))
					}
				})
			}
			wg.Wait()
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			for _, row := range st.Rows(store, "counters") {
				if row.Int64("n") != 2 {
					return fmt.Errorf("counter %s = %d", row.Str("id"), row.Int64("n"))
				}
			}
			return nil
		})
	}, Workers(8), MaxPreemptions(2))
}

// A goroutine its process does not wait for goes on after the process is done,
// as one a worker loop hands a claimed job to.
func TestAdoptedGoroutineOutlivesItsProcess(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		s.Seed(func() {
			_, _ = db.Exec(`INSERT INTO "jobs" ("id","state") VALUES ('j', 'pending')`)
		})
		var wg sync.WaitGroup
		s.Loop("worker", 1, func(p *Proc) error {
			res, err := db.Exec(`UPDATE "jobs" SET "state" = 'claimed' WHERE "id" = 'j' AND "state" = 'pending'`)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return ErrIdle
			}
			wg.Go(func() {
				_, _ = db.Exec(`UPDATE "jobs" SET "state" = 'done' WHERE "id" = 'j'`)
			})
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			row, _ := st.Row(store, "jobs", "j")
			if row.Str("state") != "done" {
				return fmt.Errorf("job left %s", row.Str("state"))
			}
			return nil
		})
		s.Sometimes("the worker is done before its goroutine", func(st *State) bool {
			row, _ := st.Row(store, "jobs", "j")
			return row.Str("state") == "claimed"
		})
	})
}

// A goroutine that sleeps between its calls, as one polling another service
// does, lets the clock advance and is taken back when it wakes.
func TestAdoptedGoroutineSleeps(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		s.Seed(func() {
			_, _ = db.Exec(`INSERT INTO "counters" ("id","n") VALUES ('c', 0)`)
		})
		s.Manual("pod", 1, func(p *Proc) error {
			return fanOut(2, func() error {
				for range 2 {
					if _, err := db.Exec(`UPDATE "counters" SET "n" = "n" + 1 WHERE "id" = 'c'`); err != nil {
						return err
					}
					time.Sleep(3 * time.Second)
				}
				return nil
			})
		})
		s.AtQuiescence(func(st *State) error {
			row, _ := st.Row(store, "counters", "c")
			if n := row.Int64("n"); n != 4 {
				return fmt.Errorf("n = %d", n)
			}
			return nil
		})
	})
}

// A run cut short by a violation while goroutines are parked in detest's
// calls ends them with an error instead of a panic nothing recovers.
func TestAdoptedGoroutinesAbortedInCalls(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		ext := s.External("svc", ReadOnly())
		rt := ext.Transport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		mu := s.Mutex("mu")
		s.Manual("pod", 1, func(p *Proc) error {
			calls := []func() error{
				func() error {
					_, err := db.Exec(`INSERT INTO "flags" ("id") VALUES ('f')`)
					return err
				},
				func() error {
					return ext.Do(s.Current(), "do", func() error { return nil })
				},
				func() error {
					req, _ := http.NewRequest(http.MethodGet, "http://svc/", nil)
					resp, err := (&http.Client{Transport: rt}).Do(req)
					if err == nil {
						_ = resp.Body.Close()
					}
					return err
				},
				func() error {
					mu.Lock()
					defer mu.Unlock()
					return nil
				},
				func() error {
					return store.Tx(s.Current(), func(tx *Tx) error {
						return tx.Insert("flags", Row{"id": "g"})
					})
				},
				func() error {
					store.Get(s.Current(), "flags", "f")
					return nil
				},
				func() error {
					store.Select(s.Current(), "flags", func(Row) bool { return true })
					return nil
				},
			}
			var wg sync.WaitGroup
			for _, c := range calls {
				wg.Go(func() { _ = c() })
			}
			wg.Wait()
			return nil
		})
		s.Always(func(st *State) error {
			if len(st.Rows(store, "flags")) > 0 {
				return errors.New("flag set")
			}
			return nil
		})
		s.ExpectViolation("flag set")
	})
}

// A goroutine still asleep when its run ends wakes in a later run, here the
// rerun of the violating schedule, and must unwind there as a process of its
// own run rather than be adopted by the later one.
func TestAdoptedGoroutineFromEndedRunNotAdopted(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		s.Manual("pod", 1, func(p *Proc) error {
			go func() {
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('early')`)
				// Asleep past the violation below, it wakes while the rerun
				// sleeps.
				time.Sleep(3 * time.Second)
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('late')`)
			}()
			time.Sleep(2 * time.Second)
			_, err := db.Exec(`INSERT INTO "marks" ("id") VALUES ('pod')`)
			return err
		})
		s.Always(func(st *State) error {
			if _, ok := st.Row(store, "marks", "late"); ok {
				return errors.New("late mark")
			}
			if _, ok := st.Row(store, "marks", "pod"); ok {
				return errors.New("pod mark")
			}
			return nil
		})
		s.ExpectViolation("pod mark")
	})
}

// A crash takes the pod with its goroutines, and may strike while the pod
// waits for them.
func TestCrashTakesAdoptedGoroutines(t *testing.T) {
	var mu sync.Mutex
	halfDone := 0
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		s.Manual("pod", 1, func(p *Proc) error {
			if err := fanOut(1, func() error {
				for _, id := range []string{"a", "b"} {
					if _, err := db.Exec(`INSERT INTO "marks" ("id") VALUES ($1)`, id); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
			_, err := db.Exec(`INSERT INTO "marks" ("id") VALUES ('pod')`)
			return err
		})
		s.AtQuiescence(func(st *State) error {
			_, a := st.Row(store, "marks", "a")
			_, b := st.Row(store, "marks", "b")
			_, pod := st.Row(store, "marks", "pod")
			if pod && !b {
				return errors.New("the pod went on without its goroutine's work")
			}
			if a && !b {
				mu.Lock()
				halfDone++
				mu.Unlock()
			}
			return nil
		})
	}, MaxCrashes(1))
	if halfDone == 0 {
		t.Error("no run crashed the pod between its goroutine's two inserts")
	}
}

// A goroutine the end of its run cut at a yield point unwinds there, and when
// it calls again during a later run, its statement and its call to another
// service are turned away before they touch that run's state.
func TestAdoptedGoroutineCutAtYieldNotRunLater(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		hits := 0
		s.Seed(func() { hits = 0 })
		ext := s.External("svc", ReadOnly())
		client := &http.Client{Transport: ext.Transport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))}
		s.Manual("pod", 1, func(p *Proc) error {
			// The rerun sleeps here while the goroutine cut in the first run
			// wakes.
			time.Sleep(2 * time.Second)
			go func() {
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('a')`)
			}()
			go func() {
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('b')`) // cut here
				time.Sleep(time.Second)
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('late')`)
				req, _ := http.NewRequest(http.MethodGet, "http://svc/", nil)
				if resp, err := client.Do(req); err == nil {
					_ = resp.Body.Close()
				}
			}()
			return nil
		})
		s.Always(func(st *State) error {
			if _, ok := st.Row(store, "marks", "late"); ok {
				return errors.New("late mark")
			}
			if hits > 0 {
				return errors.New("a call of an ended run reached the service")
			}
			if _, ok := st.Row(store, "marks", "a"); ok {
				return errors.New("a mark")
			}
			return nil
		})
		s.ExpectViolation("a mark")
	})
}

// A worker tick that hands its job to a goroutine and returns leaves the
// goroutine running, and a crash of the pod still takes it.
func TestCrashTakesGoroutineOfFinishedTick(t *testing.T) {
	var mu sync.Mutex
	halfDone := 0
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		s.Loop("worker", 1, func(p *Proc) error {
			go func() {
				for _, id := range []string{"a", "b"} {
					if _, err := db.Exec(`INSERT INTO "marks" ("id") VALUES ($1)`, id); err != nil {
						return
					}
				}
			}()
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			_, a := st.Row(store, "marks", "a")
			_, b := st.Row(store, "marks", "b")
			if a && !b {
				mu.Lock()
				halfDone++
				mu.Unlock()
			}
			return nil
		})
	}, MaxCrashes(1))
	if halfDone == 0 {
		t.Error("no run crashed the pod between its goroutine's two inserts")
	}
}

// A process that sleeps for minutes, as one waiting out a lease or a backoff
// does, is waited for rather than reported as blocked for good.
func TestProcessSleepsForMinutes(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		s.Manual("pod", 1, func(p *Proc) error {
			time.Sleep(6 * time.Minute)
			_, err := db.Exec(`INSERT INTO "marks" ("id") VALUES ('woke')`)
			return err
		})
		s.AtQuiescence(func(st *State) error {
			if _, ok := st.Row(store, "marks", "woke"); !ok {
				return errors.New("the pod never woke")
			}
			return nil
		})
	})
}

// A goroutine that crashed with its pod at a yield point gets errRunOver once
// the run ends and goes on, and must be turned away when it calls again during
// a later run.
func TestCrashedGoroutineNotAdoptedLater(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		s.Manual("pod", 1, func(p *Proc) error {
			// A later run sleeps here while the goroutine of a crashed run
			// wakes.
			time.Sleep(5 * time.Second)
			go func() {
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('a')`)
				time.Sleep(3 * time.Second)
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('late')`)
			}()
			return nil
		})
		s.Always(func(st *State) error {
			_, a := st.Row(store, "marks", "a")
			_, late := st.Row(store, "marks", "late")
			if late && !a {
				return errors.New("a goroutine of an ended run wrote in this one")
			}
			return nil
		})
	}, MaxCrashes(1))
}

// A goroutine that starts one of its own before its first call into detest
// reaches detest in the same step as its child, and both are taken in, the
// child named after its parent.
func TestAdoptedGoroutineStartsOneBeforeItsFirstCall(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		s.Manual("pod", 1, func(p *Proc) error {
			var wg sync.WaitGroup
			wg.Go(func() {
				wg.Go(func() {
					_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('child')`)
				})
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('parent')`)
			})
			wg.Wait()
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			return errors.New("done")
		})
	}, nil, nil, 0)
	if !res.Violated {
		t.Fatal("expected the run to reach quiescence")
	}
	for _, name := range []string{"pod#1.1 ", "pod#1.1.1 "} {
		if !strings.Contains(res.Trace, name) {
			t.Errorf("the trace does not name %q:\n%s", name, res.Trace)
		}
	}
}

// A process blocked for good on a channel is reported, and its goroutine
// left blocked when the bubble ends does not discard the report.
func TestProcessBlockedForeverReported(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		s.Manual("pod", 1, func(p *Proc) error {
			<-make(chan struct{})
			return nil
		})
		s.ExpectViolation("pod#1 is blocked on a channel")
	})
}

// The same holds for a goroutine the code under test started.
func TestAdoptedGoroutineBlockedForeverReported(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		s.Manual("pod", 1, func(p *Proc) error {
			go func() {
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('a')`)
				<-make(chan struct{})
			}()
			return nil
		})
		s.ExpectViolation("pod#1.1 is blocked on a channel")
	})
}

// A goroutine may return holding a mutex its pod unlocks later, as Go allows.
// A crash of the pod after the goroutine returned frees that mutex too.
func TestCrashFreesMutexOfReturnedGoroutine(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mu := s.Mutex("mu")
		s.Manual("pod", 1, func(p *Proc) error {
			var wg sync.WaitGroup
			wg.Go(func() { mu.Lock() })
			wg.Go(func() {
				time.Sleep(time.Second) // after its sibling returned
				for _, id := range []string{"a", "b"} {
					_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ($1)`, id)
				}
			})
			wg.Wait()
			mu.Unlock()
			return nil
		})
		s.Manual("other", 1, func(p *Proc) error {
			mu.Lock()
			defer mu.Unlock()
			return nil
		}, After(func(st *State) bool {
			_, ok := st.Row(store, "marks", "a")
			return ok
		}))
	}, MaxCrashes(1))
}
