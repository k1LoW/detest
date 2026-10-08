package detest

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/k1LoW/detest/db/postgres"
)

// A run cut short by a violation can leave a process asleep on a timer. Its
// goroutine must be gone before the next run starts, or it would wake in that
// run and act on its simulated resources.
func TestCutRunLeavesNoProcessForTheNext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSim(t, EagerStart(false))
		db, store := s.DB("app", postgres.New())
		defer s.closeSQL()
		mustExec(t, db, `CREATE TABLE marks (id text PRIMARY KEY)`)
		s.Manual("sleeper", 1, func(p *Proc) error {
			time.Sleep(time.Second)
			_, err := db.Exec(`INSERT INTO marks (id) VALUES ('late')`)
			return err
		})
		s.Manual("fast", 1, func(p *Proc) error {
			_, err := db.Exec(`INSERT INTO marks (id) VALUES ('fast')`)
			return err
		})
		s.Always(func(st *State) error {
			if _, ok := st.Row(store, "marks", "fast"); ok {
				return errors.New("cut")
			}
			return nil
		})
		s.frozen = true

		// sleeper starts and sleeps, fast inserts, the invariant cuts the run.
		r := s.newRun([]choice{{picked: 0, replay: true}})
		if v := r.execute(); v == nil || v.err.Error() != "cut" {
			t.Fatalf("got %v, want the cut", v)
		}
		for _, p := range r.procs {
			select {
			case <-p.exited:
			default:
				t.Errorf("%s is still running after its run ended", p.name)
			}
		}
	})
}
