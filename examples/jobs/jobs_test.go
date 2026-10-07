package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/k1LoW/detest"
	"github.com/k1LoW/detest/db/postgres"
)

// simulate declares a worker that runs twice, which may crash at any step
// once, and the invariant that the job ends done.
func simulate(run func(context.Context, *sql.DB, func(string) error) error) func(t *testing.T, s *detest.Sim) {
	return func(t *testing.T, s *detest.Sim) {
		db, store := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE jobs (id text PRIMARY KEY, status text NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		s.Seed(func() {
			if _, err := db.Exec(`INSERT INTO jobs VALUES ('j1', 'pending')`); err != nil {
				t.Fatal(err)
			}
		})
		s.Manual("worker", 2, func(p *detest.Proc) error {
			err := run(p.Context(), db, func(id string) error {
				p.Step("does job %s", id)
				return nil
			})
			if errors.Is(err, ErrNoJob) {
				return nil
			}
			return err
		})
		s.AtQuiescence(func(st *detest.State) error {
			if row, _ := st.Row(store, "jobs", "j1"); row.Str("status") != "done" {
				return fmt.Errorf("job j1 left %s", row.Str("status"))
			}
			return nil
		})
	}
}

func TestRunTwoStepStrandsTheJob(t *testing.T) {
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		simulate(RunTwoStep)(t, s)
		s.ExpectViolation("job j1 left running")
	}, detest.MaxCrashes(1))
}

func TestRun(t *testing.T) {
	detest.Explore(t, simulate(Run), detest.MaxCrashes(1))
}
