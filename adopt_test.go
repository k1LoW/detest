package detest

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
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

// A seed that resets the previous run's channel wakes a goroutine of that
// run before any process of the new one ran. Its statement is turned away,
// not run as part of the seed.
func TestAdoptedGoroutineWokenBySeedNotRun(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		var ch chan struct{}
		s.Seed(func() {
			if ch != nil {
				close(ch)
				synctest.Wait() // the woken goroutine gets as far as it can
			}
			ch = make(chan struct{})
		})
		s.Manual("pod", 1, func(p *Proc) error {
			wake := ch
			go func() {
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('g')`)
				<-wake
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('late')`)
			}()
			return nil
		})
		s.Always(func(st *State) error {
			if _, ok := st.Row(store, "marks", "late"); ok {
				return errors.New("late mark")
			}
			if _, ok := st.Row(store, "marks", "g"); ok {
				return errors.New("g mark")
			}
			return nil
		})
		s.ExpectViolation("g mark")
	})
}

// A goroutine that runs a statement or commits on a transaction its parent
// began is refused, rather than scheduled as the parent.
func TestAdoptedGoroutineOnParentTxRefused(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit=%v", commit), func(t *testing.T) {
			var mu sync.Mutex
			var got error
			Explore(t, func(t *testing.T, s *Sim) {
				db, store := s.DB("app", postgres.New())
				s.Manual("pod", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					var gerr error
					var wg sync.WaitGroup
					wg.Go(func() {
						if commit {
							gerr = tx.Commit()
						} else {
							_, gerr = tx.Exec(`INSERT INTO "marks" ("id") VALUES ('g')`)
						}
					})
					wg.Wait()
					mu.Lock()
					got = gerr
					mu.Unlock()
					// A refused commit leaves no transaction on the pooled
					// connection for this statement to run in.
					_, err = db.Exec(`INSERT INTO "marks" ("id") VALUES ('after')`)
					return err
				})
				s.AtQuiescence(func(st *State) error {
					if _, ok := st.Row(store, "marks", "after"); !ok {
						return errors.New("a statement after the refused commit was not committed")
					}
					return nil
				})
			})
			if _, ok := errors.AsType[*ErrUnsupportedSQL](got); !ok {
				t.Errorf("the goroutine got %v, want an unsupported error", got)
			}
		})
	}
}

// A goroutine cut at a yield point that begins a transaction when it wakes
// during a later run is turned away before it touches the connection.
func TestAdoptedGoroutineBeginsTxInLaterRun(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
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
				tx, err := db.Begin()
				if err != nil {
					return
				}
				_, _ = tx.Exec(`INSERT INTO "marks" ("id") VALUES ('late')`)
				_ = tx.Commit()
			}()
			return nil
		})
		s.Always(func(st *State) error {
			if _, ok := st.Row(store, "marks", "late"); ok {
				return errors.New("late mark")
			}
			if _, ok := st.Row(store, "marks", "a"); ok {
				return errors.New("a mark")
			}
			return nil
		})
		s.ExpectViolation("a mark")
	})
}

// A goroutine of an ended run that takes a mutex in a later run ends there,
// rather than run its critical section without the mutex.
func TestStaleGoroutineEndsAtLock(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mu := s.Mutex("mu")
		entered := 0
		s.Seed(func() { entered = 0 })
		s.Manual("pod", 1, func(p *Proc) error {
			time.Sleep(2 * time.Second)
			go func() {
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('a')`)
			}()
			go func() {
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('b')`) // cut here
				time.Sleep(time.Second)
				mu.Lock()
				entered++
				mu.Unlock()
			}()
			return nil
		})
		s.Always(func(st *State) error {
			if entered > 0 {
				return errors.New("a goroutine of an ended run entered the critical section")
			}
			if _, ok := st.Row(store, "marks", "a"); ok {
				return errors.New("a mark")
			}
			return nil
		})
		s.ExpectViolation("a mark")
	})
}

// A fake's hand-written transaction handed to a goroutine its process
// started runs as that goroutine, which waits for row locks itself.
func handWrittenTxByGoroutine(s *Sim) *DB {
	_, store := s.DB("app", postgres.New())
	s.Seed(func() { store.SeedRow("counters", Row{"id": "c", "n": int64(0)}) })
	for _, name := range []string{"x", "y"} {
		s.Manual(name, 1, func(p *Proc) error {
			return store.Tx(p, func(tx *Tx) error {
				var err error
				var wg sync.WaitGroup
				wg.Go(func() {
					row, _, gerr := tx.GetForUpdate("counters", "c")
					if gerr != nil {
						err = gerr
						return
					}
					_, err = tx.Update("counters", "c", Row{"n": row.Int64("n") + 1})
				})
				wg.Wait()
				return err
			})
		})
	}
	return store
}

func TestHandWrittenTxRunByGoroutine(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		store := handWrittenTxByGoroutine(s)
		s.AtQuiescence(func(st *State) error {
			row, _ := st.Row(store, "counters", "c")
			if n := row.Int64("n"); n != 2 {
				return fmt.Errorf("lost update: n = %d", n)
			}
			return nil
		})
		s.Sometimes("a goroutine waits for the other transaction's lock", func(st *State) bool {
			row, _ := st.Row(store, "counters", "c")
			return row.Int64("n") == 1
		})
	})
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		handWrittenTxByGoroutine(s)
		s.AtQuiescence(func(st *State) error { return errors.New("done") })
	}, nil, nil, 0)
	if !res.Violated || !strings.Contains(res.Trace, "x#1.1  app: select public.counters id=c for update") {
		t.Errorf("the goroutine does not run the transaction's operations:\n%s", res.Trace)
	}
}

// A process that wakes from a sleep and calls an external service with its
// own *Proc is taken back by the scheduler before the call picks an outcome.
func TestExternalCallAfterSleepDeterministic(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		ext := s.External("svc")
		for _, name := range []string{"x", "y"} {
			s.Manual(name, 1, func(p *Proc) error {
				time.Sleep(time.Second)
				return ext.Do(p, "call", func() error { return nil })
			})
		}
	}, Workers(4))
}

// A goroutine whose first call into detest is an enqueue on a transaction
// handed to it is taken in as a process before the enqueue.
func TestHandWrittenTxEnqueueByGoroutine(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		_, store := s.DB("app", postgres.New())
		q := s.Queue("outbox")
		s.Manual("pod", 1, func(p *Proc) error {
			return store.Tx(p, func(tx *Tx) error {
				var wg sync.WaitGroup
				wg.Go(func() {
					tx.Enqueue(q, Msg{"id": "m"})
				})
				wg.Wait()
				return nil
			})
		})
		s.AtQuiescence(func(st *State) error {
			if len(st.Queue(q)) != 1 {
				return fmt.Errorf("%d messages", len(st.Queue(q)))
			}
			return errors.New("done")
		})
	}, nil, nil, 0)
	if !res.Violated || !strings.Contains(fmt.Sprint(res.Err), "done") {
		t.Fatalf("expected the message published, got %v", res.Err)
	}
	if !strings.Contains(res.Trace, "starts goroutine pod#1.1") {
		t.Errorf("the goroutine was not taken in:\n%s", res.Trace)
	}
}

// A goroutine started in the step that ends the run is still waiting to be
// taken in, and is retired with the others.
func TestPendingGoroutineRetired(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		s.Manual("pod", 1, func(p *Proc) error {
			// The rerun sleeps here while the goroutine of the first run wakes.
			time.Sleep(2 * time.Second)
			if _, err := db.Exec(`INSERT INTO "marks" ("id") VALUES ('x')`); err != nil {
				return err
			}
			go func() {
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('g')`)
				time.Sleep(time.Second)
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('late')`)
			}()
			return nil
		})
		s.Always(func(st *State) error {
			if _, ok := st.Row(store, "marks", "late"); ok {
				return errors.New("late mark")
			}
			if _, ok := st.Row(store, "marks", "x"); ok {
				return errors.New("x mark")
			}
			return nil
		})
		s.ExpectViolation("x mark")
	})
}

// Two goroutines a process started may use its hand-written transaction at
// once, each waiting for locks as itself.
func TestHandWrittenTxRunByTwoGoroutinesAtOnce(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		_, store := s.DB("app", postgres.New())
		s.Seed(func() {
			store.SeedRow("counters", Row{"id": "a", "n": int64(0)})
			store.SeedRow("counters", Row{"id": "b", "n": int64(0)})
		})
		s.Manual("x", 1, func(p *Proc) error {
			return store.Tx(p, func(tx *Tx) error {
				var wg sync.WaitGroup
				errs := make([]error, 2)
				for i, id := range []string{"a", "b"} {
					wg.Go(func() { _, _, errs[i] = tx.GetForUpdate("counters", id) })
				}
				wg.Wait()
				return errors.Join(errs...)
			})
		})
		s.Manual("y", 1, func(p *Proc) error {
			return store.Tx(p, func(tx *Tx) error {
				_, _, err := tx.GetForUpdate("counters", "b")
				return err
			})
		})
	})
}

// Goroutines running two processes' hand-written transactions that take
// row locks in opposite orders deadlock, which the database detects.
func TestHandWrittenTxGoroutinesDeadlock(t *testing.T) {
	var mu sync.Mutex
	deadlocks := 0
	Explore(t, func(t *testing.T, s *Sim) {
		_, store := s.DB("app", postgres.New())
		s.Seed(func() {
			store.SeedRow("counters", Row{"id": "a", "n": int64(0)})
			store.SeedRow("counters", Row{"id": "b", "n": int64(0)})
		})
		for _, w := range []struct {
			name  string
			order []string
		}{{"x", []string{"a", "b"}}, {"y", []string{"b", "a"}}} {
			order := w.order
			s.Manual(w.name, 1, func(p *Proc) error {
				err := store.Tx(p, func(tx *Tx) error {
					var err error
					var wg sync.WaitGroup
					wg.Go(func() {
						for _, id := range order {
							if _, _, err = tx.GetForUpdate("counters", id); err != nil {
								return
							}
						}
					})
					wg.Wait()
					return err
				})
				if errors.Is(err, ErrDeadlock) {
					mu.Lock()
					deadlocks++
					mu.Unlock()
				}
				return nil
			})
		}
	})
	if deadlocks == 0 {
		t.Error("no run detected the deadlock between the goroutines")
	}
}

// Two goroutines may wait on behalf of one hand-written transaction at
// once, and a deadlock through either one's wait is detected. The goroutine
// waiting for "c" sorts first and is on no cycle, while the one waiting for
// "d" closes the cycle with y.
func TestHandWrittenTxDeadlockThroughSecondWaiter(t *testing.T) {
	var mu sync.Mutex
	deadlocks := 0
	Explore(t, func(t *testing.T, s *Sim) {
		_, store := s.DB("app", postgres.New())
		s.Seed(func() {
			for _, id := range []string{"a", "c", "d"} {
				store.SeedRow("counters", Row{"id": id, "n": int64(0)})
			}
		})
		count := func(err error) error {
			if errors.Is(err, ErrDeadlock) {
				mu.Lock()
				deadlocks++
				mu.Unlock()
				return nil
			}
			return err
		}
		s.Manual("x", 1, func(p *Proc) error {
			return count(store.Tx(p, func(tx *Tx) error {
				if _, _, err := tx.GetForUpdate("counters", "a"); err != nil {
					return err
				}
				var wg sync.WaitGroup
				errs := make([]error, 2)
				for i, id := range []string{"c", "d"} {
					wg.Go(func() { _, _, errs[i] = tx.GetForUpdate("counters", id) })
				}
				wg.Wait()
				return errors.Join(errs...)
			}))
		})
		s.Manual("y", 1, func(p *Proc) error {
			return count(store.Tx(p, func(tx *Tx) error {
				if _, _, err := tx.GetForUpdate("counters", "d"); err != nil {
					return err
				}
				_, _, err := tx.GetForUpdate("counters", "a")
				return err
			}))
		})
		s.Manual("z", 1, func(p *Proc) error {
			return store.Tx(p, func(tx *Tx) error {
				_, _, err := tx.GetForUpdate("counters", "c")
				return err
			})
		})
	}, MaxPreemptions(2))
	if deadlocks == 0 {
		t.Error("no run detected the deadlock")
	}
}

// A goroutine that inserts through its parent's hand-written transaction
// takes no value of a sequence before its turn: neither on its first call,
// before it is taken in, nor as a goroutine of an ended run during a later
// run, where it is turned away first.
func TestStaleGoroutineTxInsertTakesNoSequence(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE marks (id serial PRIMARY KEY, name text NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		s.Manual("pod", 1, func(p *Proc) error {
			// The rerun sleeps here while the goroutine of the first run wakes.
			time.Sleep(2 * time.Second)
			return store.Tx(p, func(tx *Tx) error {
				go func() {
					_, _ = db.Exec(`INSERT INTO marks (name) VALUES ('a')`)
				}()
				go func() {
					_ = tx.Insert("marks", Row{"name": "g"}) // cut here
					time.Sleep(time.Second)
					_ = tx.Insert("marks", Row{"name": "late"})
				}()
				time.Sleep(5 * time.Second)
				return nil
			})
		})
		s.Always(func(st *State) error {
			for _, r := range st.Rows(store, "marks") {
				if r.Str("name") == "a" {
					return fmt.Errorf("a mark with id %d", r.Int64("id"))
				}
			}
			return nil
		})
		s.ExpectViolation("a mark with id 1")
	})
}

// Every goroutine waiting on behalf of a transaction picked as a deadlock
// victim sees the abort, rather than only the first one resumed.
func TestDeadlockVictimSeenByEveryWaiter(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		_, store := s.DB("app", postgres.New())
		s.Seed(func() {
			for _, id := range []string{"a", "b", "c"} {
				store.SeedRow("counters", Row{"id": id, "n": int64(0)})
			}
		})
		var mixed []string
		s.Seed(func() { mixed = nil })
		s.Manual("x", 1, func(p *Proc) error {
			_ = store.Tx(p, func(tx *Tx) error {
				if _, _, err := tx.GetForUpdate("counters", "a"); err != nil {
					return err
				}
				var wg sync.WaitGroup
				var done []error // in the order the goroutines finished
				for _, id := range []string{"b", "c"} {
					wg.Go(func() {
						_, _, err := tx.GetForUpdate("counters", id)
						done = append(done, err)
					})
				}
				wg.Wait()
				if len(done) == 2 && errors.Is(done[0], ErrDeadlock) && done[1] == nil {
					mixed = append(mixed, fmt.Sprintf("%v, then %v", done[0], done[1]))
				}
				return errors.Join(done...)
			})
			return nil
		})
		s.Manual("y", 1, func(p *Proc) error {
			_ = store.Tx(p, func(tx *Tx) error {
				for _, id := range []string{"b", "c", "a"} {
					if _, _, err := tx.GetForUpdate("counters", id); err != nil {
						return err
					}
				}
				return nil
			})
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			if len(mixed) > 0 {
				return fmt.Errorf("a waiter went on after the transaction was aborted: %s", mixed[0])
			}
			return nil
		})
	}, MaxPreemptions(2))
}

// An adopted goroutine whose stack is too deep to classify is still ended
// at a hand-written transaction's operation the end of the run cuts short.
func TestDeepAdoptedGoroutineAbortedInTx(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		s.Manual("pod", 1, func(p *Proc) error {
			return store.Tx(p, func(tx *Tx) error {
				var wg sync.WaitGroup
				var deep func(n int)
				deep = func(n int) {
					if n == 0 {
						_ = tx.Insert("marks", Row{"id": "deep"}) // cut here
						return
					}
					deep(n - 1)
				}
				wg.Go(func() { deep(600) })
				wg.Go(func() { _, _ = db.Exec(`INSERT INTO "flags" ("id") VALUES ('f')`) })
				wg.Wait()
				return nil
			})
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

// A goroutine releasing a read lock its parent took releases the parent's,
// not that of another process holding the same lock. Were it the other
// process's, that process's crash would free nothing, and a writer would wait
// for good on the read lock the parent's pod still counted.
func TestRUnlockByGoroutineReleasesItsFamilysLock(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		rw := s.RWMutex("rw")
		s.Manual("other", 1, func(p *Proc) error {
			rw.RLock()
			p.Step("hold the read lock")
			rw.RUnlock()
			return nil
		})
		s.Manual("pod", 1, func(p *Proc) error {
			rw.RLock()
			var wg sync.WaitGroup
			wg.Go(func() { rw.RUnlock() })
			wg.Wait()
			return nil
		})
		s.Manual("writer", 1, func(p *Proc) error {
			rw.Lock()
			defer rw.Unlock()
			p.Step("write")
			return nil
		})
	}, MaxCrashes(1), MaxPreemptions(2))
}

// A read of a hand-written transaction that another goroutine had aborted
// while the read stood at its yield point returns nothing, as one made after
// the abort does.
func TestHandWrittenTxReadAfterSiblingAbort(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		_, store := s.DB("app", postgres.New())
		s.Seed(func() {
			for _, id := range []string{"a", "b"} {
				store.SeedRow("counters", Row{"id": id, "n": int64(0)})
			}
		})
		var stale []string
		s.Seed(func() { stale = nil })
		s.Manual("x", 1, func(p *Proc) error {
			_ = store.Tx(p, func(tx *Tx) error {
				if _, _, err := tx.GetForUpdate("counters", "a"); err != nil {
					return err
				}
				var wg sync.WaitGroup
				var lockErr error
				aborted := false // the lock's deadlock error came back first
				wg.Go(func() {
					_, _, lockErr = tx.GetForUpdate("counters", "b")
					aborted = errors.Is(lockErr, ErrDeadlock)
				})
				wg.Go(func() {
					if read, _ := tx.Get("counters", "a"); read != nil && aborted {
						stale = append(stale, "a read returned a row after its transaction was aborted")
					}
				})
				wg.Wait()
				return lockErr
			})
			return nil
		})
		s.Manual("y", 1, func(p *Proc) error {
			_ = store.Tx(p, func(tx *Tx) error {
				for _, id := range []string{"b", "a"} {
					if _, _, err := tx.GetForUpdate("counters", id); err != nil {
						return err
					}
				}
				return nil
			})
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			if len(stale) > 0 {
				return errors.New(stale[0])
			}
			return nil
		})
	}, MaxPreemptions(2))
}

// A goroutine that first calls into detest after its pod crashed, such as
// one that slept first, is dead with the pod and writes nothing.
func TestGoroutineOfCrashedPodNeverRuns(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		s.Manual("pod", 1, func(p *Proc) error {
			go func() {
				time.Sleep(time.Second)
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('late')`)
			}()
			_, err := db.Exec(`INSERT INTO "marks" ("id") VALUES ('pod')`)
			return err
		})
		// Keeps the run going until the goroutine wakes.
		s.Manual("keeper", 1, func(p *Proc) error {
			time.Sleep(2 * time.Second)
			p.Step("wake")
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			_, pod := st.Row(store, "marks", "pod")
			_, late := st.Row(store, "marks", "late")
			if late && !pod {
				return errors.New("a goroutine of a crashed pod wrote")
			}
			return nil
		})
	}, MaxCrashes(1))
}

// A goroutine started by a helper that never calls into detest belongs to
// the process that started the helper, not to whichever process the
// scheduler ran last, as long as the helper is alive to tell.
func TestGoroutineOfHelperBelongsToHelpersProcess(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		s.Manual("a", 1, func(p *Proc) error {
			go func() { // the helper, waiting for what it starts
				time.Sleep(time.Second)
				var wg sync.WaitGroup
				wg.Go(func() {
					_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('c')`)
				})
				wg.Wait()
			}()
			return nil
		})
		s.Manual("b", 1, func(p *Proc) error {
			for range 3 {
				time.Sleep(400 * time.Millisecond)
				p.Step("tick")
			}
			return nil
		})
		s.AtQuiescence(func(st *State) error { return errors.New("done") })
	}, nil, nil, 0)
	if !res.Violated {
		t.Fatal("expected the run to reach quiescence")
	}
	if !strings.Contains(res.Trace, "a#1 ") || !strings.Contains(res.Trace, "starts goroutine a#1.1") {
		t.Errorf("the goroutine is not a's:\n%s", res.Trace)
	}
}

// A goroutine that sleeps after its last call into detest and then returns
// is not taken for one blocked for good.
func TestAdoptedGoroutineReturnsAfterTrailingSleep(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		s.Manual("pod", 1, func(p *Proc) error {
			go func() {
				_, _ = db.Exec(`INSERT INTO "marks" ("id") VALUES ('g')`)
				time.Sleep(time.Hour)
			}()
			return nil
		})
	})
}
