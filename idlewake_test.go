package detest

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/k1LoW/detest/db/mysql"
	"github.com/k1LoW/detest/db/postgres"
)

// idleModel declares two processes committing to accounts, which a sweeper
// whose tick runs sweep never reads, and fails a run in which the sweeper
// ticks more than once.
func idleModel(server func() Server, sweep func(p *Proc, db *sql.DB) error, schema ...string) func(t *testing.T, s *Sim) {
	return func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", server())
		for _, q := range schema {
			mustExec(t, db, q)
		}
		s.Seed(func() { mustExec(t, db, `INSERT INTO accounts VALUES (1, 0), (2, 0)`) })
		ticks := 0
		s.Seed(func() { ticks = 0 })
		for _, c := range []struct{ name, q string }{
			{"alice", `UPDATE accounts SET n = n + 1 WHERE id = 1`},
			{"bob", `UPDATE accounts SET n = n + 1 WHERE id = 2`},
		} {
			name, q := c.name, c.q
			s.Manual(name, 1, func(p *Proc) error {
				for range 2 {
					if _, err := db.ExecContext(p.Context(), q); err != nil {
						return err
					}
				}
				return nil
			})
		}
		s.Loop("sweeper", 3, func(p *Proc) error {
			ticks++
			if err := sweep(p, db); err != nil {
				return err
			}
			return ErrIdle
		})
		s.AtQuiescence(func(*State) error {
			if ticks > 1 {
				return fmt.Errorf("the sweeper ticked %d times", ticks)
			}
			return nil
		})
	}
}

func pgIdleSchema() []string {
	return []string{
		`CREATE TABLE accounts (id int PRIMARY KEY, n int NOT NULL)`,
		`CREATE TABLE jobs (id int PRIMARY KEY, status text NOT NULL)`,
		`CREATE VIEW pending AS SELECT id FROM jobs WHERE status = 'pending'`,
	}
}

func idleQuery(q string) func(p *Proc, db *sql.DB) error {
	return func(p *Proc, db *sql.DB) error {
		rows, err := db.QueryContext(p.Context(), q)
		if err != nil {
			return err
		}
		return rows.Close()
	}
}

// Under PartialOrder, an idle loop is not woken by commits to a table its
// idle tick did not read, however the tick's statement reads the ones it
// does: through an aggregate, a subquery, a view, or a snapshot of
// Repeatable Read. Without PartialOrder, every commit wakes it.
func TestPartialOrderWakesNoIdleLoopOnAnUnreadTable(t *testing.T) {
	t.Parallel()
	pg := func() Server { return postgres.New() }
	for _, c := range []struct {
		name  string
		model func(t *testing.T, s *Sim)
	}{
		{"aggregate", idleModel(pg, idleQuery(`SELECT count(*) FROM jobs WHERE status = 'pending'`), pgIdleSchema()...)},
		{"subquery", idleModel(pg, idleQuery(`SELECT id FROM jobs WHERE id IN (SELECT id FROM jobs WHERE status = 'pending')`), pgIdleSchema()...)},
		{"view", idleModel(pg, idleQuery(`SELECT count(*) FROM pending`), pgIdleSchema()...)},
		{"update", idleModel(pg, func(p *Proc, db *sql.DB) error {
			_, err := db.ExecContext(p.Context(), `UPDATE jobs SET status = 'running' WHERE id = (SELECT min(id) FROM jobs WHERE status = 'pending')`)
			return err
		}, pgIdleSchema()...)},
		{"mysql repeatable read", idleModel(func() Server { return mysql.New() }, func(p *Proc, db *sql.DB) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			var n int
			return tx.QueryRowContext(p.Context(), `SELECT count(*) FROM jobs WHERE status = 'pending'`).Scan(&n)
		}, `CREATE TABLE accounts (id int PRIMARY KEY, n int NOT NULL)`, `CREATE TABLE jobs (id int PRIMARY KEY, status varchar(10) COLLATE utf8mb4_bin NOT NULL)`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			res, _ := exploreBubble(t, c.model, []Option{PartialOrder()}, nil, 0)
			if res.Violated || res.Fatal != nil || !res.Complete || len(res.Unsupported) > 0 {
				t.Fatalf("want a complete exploration in which the sweeper ticks once, got %s", res.report())
			}
			res, _ = exploreBubble(t, c.model, nil, nil, 0)
			if !res.Violated || !strings.Contains(res.report(), "the sweeper ticked") {
				t.Fatalf("want the sweeper woken without PartialOrder, got %s", res.report())
			}
		})
	}
}

// Under PartialOrder, an idle loop is still woken by a statement that may
// read any table, such as one with a function in FROM.
func TestPartialOrderWakesAnIdleLoopThatMayReadAnyTable(t *testing.T) {
	t.Parallel()
	model := idleModel(func() Server { return postgres.New() }, idleQuery(`SELECT g FROM generate_series(1, 2) AS g`), pgIdleSchema()...)
	res, _ := exploreBubble(t, model, []Option{PartialOrder()}, nil, 0)
	if !res.Violated || !strings.Contains(res.report(), "the sweeper ticked") {
		t.Fatalf("want the sweeper woken, got %s", res.report())
	}
}

// Under PartialOrder, an idle loop is woken by a change to what its idle
// tick read, however the tick reads it: a change that gives it work is
// never missed, so the work is done at quiescence.
func TestPartialOrderWakesAnIdleLoopOnWhatItRead(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		produce []string
		sweep   string
		flagged bool // only flagged jobs are work
	}{
		{"insert", []string{`INSERT INTO jobs VALUES (1, 'pending')`},
			`UPDATE jobs SET status = 'done' WHERE status = 'pending'`, false},
		{"update into the predicate", []string{`INSERT INTO jobs VALUES (1, 'new')`, `UPDATE jobs SET status = 'pending' WHERE id = 1`},
			`UPDATE jobs SET status = 'done' WHERE status = 'pending'`, false},
		{"subquery", []string{`INSERT INTO jobs VALUES (1, 'new')`, `INSERT INTO flags VALUES (1)`},
			`UPDATE jobs SET status = 'done' WHERE id IN (SELECT job FROM flags) AND status <> 'done'`, true},
		{"exists", []string{`INSERT INTO jobs VALUES (1, 'new')`, `INSERT INTO flags VALUES (1)`},
			`UPDATE jobs SET status = 'done' WHERE EXISTS (SELECT 1 FROM flags WHERE job = jobs.id) AND status <> 'done'`, true},
		{"view", []string{`INSERT INTO jobs VALUES (1, 'pending')`},
			`UPDATE jobs SET status = 'done' WHERE id IN (SELECT id FROM pending)`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
				db, store := s.DB("app", postgres.New())
				for _, q := range append(pgIdleSchema(), `CREATE TABLE flags (job int PRIMARY KEY)`) {
					mustExec(t, db, q)
				}
				s.Seed(func() { mustExec(t, db, `INSERT INTO accounts VALUES (1, 0)`) })
				s.Manual("producer", 1, func(p *Proc) error {
					for _, q := range c.produce {
						if _, err := db.ExecContext(p.Context(), q); err != nil {
							return err
						}
					}
					return nil
				})
				s.Manual("other", 1, func(p *Proc) error {
					for range 2 {
						if _, err := db.ExecContext(p.Context(), `UPDATE accounts SET n = n + 1 WHERE id = 1`); err != nil {
							return err
						}
					}
					return nil
				})
				s.Loop("sweeper", 3, func(p *Proc) error {
					res, err := db.ExecContext(p.Context(), c.sweep)
					if err != nil {
						return err
					}
					if n, _ := res.RowsAffected(); n == 0 {
						return ErrIdle
					}
					return nil
				})
				s.AtQuiescence(func(st *State) error {
					flags := map[string]bool{}
					for _, f := range st.Rows(store, "flags") {
						flags[f.Key()] = true
					}
					for _, j := range st.Rows(store, "jobs") {
						work := j.Str("status") == "pending"
						if c.flagged {
							work = flags[j.Key()] && j.Str("status") != "done"
						}
						if work {
							return fmt.Errorf("job %s left %s", j.Key(), j.Str("status"))
						}
					}
					return nil
				})
			}, []Option{PartialOrder()}, nil, 0)
			if res.Violated || res.Fatal != nil || !res.Complete || len(res.Unsupported) > 0 {
				t.Fatalf("want a complete exploration with the work done, got %s", res.report())
			}
		})
	}
}

// Without PartialOrder, an idle loop that reads a variable another process
// sets is woken by that process's commit, though the commit is to a table
// the tick does not read.
func TestIdleLoopWokenByAnyCommitWithoutPartialOrder(t *testing.T) {
	t.Parallel()
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		for _, q := range pgIdleSchema() {
			mustExec(t, db, q)
		}
		s.Seed(func() { mustExec(t, db, `INSERT INTO accounts VALUES (1, 0)`) })
		ready, done := false, false
		s.Seed(func() { ready, done = false, false })
		s.Manual("producer", 1, func(p *Proc) error {
			p.Step("prepares")
			ready = true
			_, err := db.ExecContext(p.Context(), `UPDATE accounts SET n = n + 1 WHERE id = 1`)
			return err
		})
		s.Loop("sweeper", 3, func(p *Proc) error {
			if err := idleQuery(`SELECT count(*) FROM jobs`)(p, db); err != nil {
				return err
			}
			if !ready || done {
				return ErrIdle
			}
			done = true
			return nil
		})
		s.AtQuiescence(func(*State) error {
			if ready && !done {
				return fmt.Errorf("ready but not done")
			}
			return nil
		})
	}, nil, nil, 0)
	if res.Violated || res.Fatal != nil || !res.Complete {
		t.Fatalf("want the sweeper woken by the commit, got %s", res.report())
	}
}
