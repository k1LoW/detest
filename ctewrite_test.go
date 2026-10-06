package detest

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/k1LoW/detest/mysql"
	"github.com/k1LoW/detest/postgres"
)

// WITH ... UPDATE and WITH ... DELETE run their CTEs once, before the rows to
// write are chosen, and FROM, USING, WHERE, SET and RETURNING read them.
func TestCTEWrites(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE jobs (id int PRIMARY KEY, status text NOT NULL, worker int)`)
	mustExec(t, db, `INSERT INTO jobs VALUES (1, 'queued', NULL), (2, 'queued', NULL), (3, 'done', NULL)`)
	claim := `WITH c AS (SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED)
		UPDATE jobs SET status = 'running', worker = $1 FROM c WHERE jobs.id = c.id RETURNING jobs.id`
	for _, tt := range []struct{ worker, job int64 }{{10, 1}, {11, 2}} {
		if got := queryIDs(t, db, claim, tt.worker); !slices.Equal(got, []int64{tt.job}) {
			t.Errorf("claim by worker %d: got %v, want [%d]", tt.worker, got, tt.job)
		}
	}
	if got := queryIDs(t, db, claim, 9); len(got) != 0 {
		t.Errorf("claim with no job queued: got %v, want none", got)
	}
	if got := queryIDs(t, db, `WITH q AS (SELECT id FROM jobs WHERE status = 'running')
		UPDATE jobs SET worker = (SELECT count(*) FROM q) WHERE id IN (SELECT id FROM q) RETURNING worker`); !slices.Equal(got, []int64{2, 2}) {
		t.Errorf("a CTE read by SET and WHERE: got %v, want [2 2]", got)
	}
	if got := queryIDs(t, db, `WITH d AS (SELECT id FROM jobs WHERE status = 'done')
		DELETE FROM jobs USING d WHERE jobs.id = d.id RETURNING jobs.id`); !slices.Equal(got, []int64{3}) {
		t.Errorf("WITH ... DELETE ... USING: got %v, want [3]", got)
	}
	// RETURNING reads the FROM and USING rows the written row was joined with.
	mustExec(t, db, `INSERT INTO jobs VALUES (5, 'queued', NULL)`)
	if got := queryIDs(t, db, `WITH c AS (SELECT 5 AS id, 70 AS v) UPDATE jobs SET worker = c.v FROM c WHERE jobs.id = c.id RETURNING c.v + jobs.id`); !slices.Equal(got, []int64{75}) {
		t.Errorf("RETURNING of a FROM item: got %v, want [75]", got)
	}
	if got := queryIDs(t, db, `WITH c AS (SELECT 5 AS id, 70 AS v) DELETE FROM jobs USING c WHERE jobs.id = c.id RETURNING c.v`); !slices.Equal(got, []int64{70}) {
		t.Errorf("RETURNING of a USING item: got %v, want [70]", got)
	}
	if _, err := db.Exec(`WITH c AS (SELECT id FROM jobs) UPDATE jobs SET worker = 1 FROM c WHERE c.nope = 1`); !errors.Is(err, ErrUndefinedColumn) {
		t.Errorf("a column the CTE does not give: got %v, want 42703", err)
	}
}

// claimJobs runs two workers that each claim a job with claim and record
// the claim, and checks that no job is claimed twice.
func claimJobs(claim string) func(t *testing.T, s *Sim) {
	return func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE jobs (id int PRIMARY KEY, status text NOT NULL)`)
		mustExec(t, db, `CREATE TABLE claims (job_id int NOT NULL, worker text NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO jobs VALUES (1, 'queued'), (2, 'queued')`) })
		for _, name := range []string{"w1", "w2"} {
			s.Manual(name, 1, func(p *Proc) error {
				tx, err := db.BeginTx(p.Context(), nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				var id int
				if err := tx.QueryRow(claim).Scan(&id); errors.Is(err, sql.ErrNoRows) {
					return nil
				} else if err != nil {
					return err
				}
				if _, err := tx.Exec(`INSERT INTO claims VALUES ($1, $2)`, id, name); err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE jobs SET status = 'done' WHERE id = $1`, id); err != nil {
					return err
				}
				return tx.Commit()
			})
		}
		s.AtQuiescence(func(st *State) error {
			seen := map[int]string{}
			for _, r := range st.Rows(store, "claims") {
				if w, ok := seen[r.Int("job_id")]; ok {
					return fmt.Errorf("job %d claimed by %s and %s", r.Int("job_id"), w, r.Str("worker"))
				}
				seen[r.Int("job_id")] = r.Str("worker")
			}
			return nil
		})
	}
}

func TestCTEClaimExploration(t *testing.T) {
	t.Run("skip locked", func(t *testing.T) {
		Explore(t, claimJobs(`WITH c AS (SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED)
			UPDATE jobs SET status = 'running' FROM c WHERE jobs.id = c.id RETURNING jobs.id`))
	})
	// Without a lock in the CTE both read job 1, and the second UPDATE waits
	// for the first and then updates the row again, as nothing in its
	// predicate fails on the new version.
	t.Run("no lock in the CTE", func(t *testing.T) {
		Explore(t, func(t *testing.T, s *Sim) {
			claimJobs(`WITH c AS (SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1)
				UPDATE jobs SET status = 'running' FROM c WHERE jobs.id = c.id RETURNING jobs.id`)(t, s)
			s.ExpectViolation("job 1 claimed by")
		})
	})
}

func TestCTEWritesUnsupported(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE jobs (id int PRIMARY KEY, status text NOT NULL)`)
	for _, q := range []string{
		`WITH jobs AS (SELECT 1 AS id) UPDATE jobs SET status = 'x' WHERE id IN (SELECT id FROM jobs)`,
		`WITH jobs AS (SELECT 1 AS id) DELETE FROM jobs WHERE id IN (SELECT id FROM jobs)`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	for _, tt := range []struct {
		srv Server
		q   string
	}{
		{postgres.New(), `WITH RECURSIVE c AS (SELECT 1 AS id) UPDATE jobs SET status = 'x' FROM c WHERE jobs.id = c.id`},
		{postgres.New(), `WITH c(id) AS (SELECT 1) UPDATE jobs SET status = 'x' FROM c WHERE jobs.id = c.id`},
		{postgres.New(), `WITH d AS (DELETE FROM jobs RETURNING id) UPDATE jobs SET status = 'x' FROM d WHERE jobs.id = d.id`},
		{postgres.New(), `WITH c AS (SELECT 1 AS id) INSERT INTO jobs VALUES (1, 'x')`},
		{postgres.New(), `WITH c AS (SELECT 1 AS id), c AS (SELECT 2 AS id) UPDATE jobs SET status = 'x' FROM c WHERE jobs.id = c.id`},
		{postgres.New(), `WITH c AS (SELECT 1 AS id), c AS (SELECT 2 AS id) SELECT id FROM c`},
		{postgres.New(), `WITH c AS (SELECT 1 AS id) UPDATE jobs SET status = 'x' WHERE id IN (WITH c AS (SELECT 2 AS id) SELECT id FROM c)`},
		// A CTE with effects in FROM or USING runs in full under every plan
		// only when it reads the target alone and WHERE only joins the two.
		{postgres.New(), `WITH c AS (SELECT id FROM jobs FOR UPDATE) UPDATE jobs SET status = 'x' FROM c WHERE jobs.id = c.id AND jobs.status = 'queued'`},
		{postgres.New(), `WITH c AS (SELECT id FROM jobs FOR UPDATE) UPDATE jobs SET status = 'x' FROM c WHERE false`},
		{postgres.New(), `WITH c AS (SELECT id FROM jobs FOR UPDATE) UPDATE jobs SET status = 'x' FROM c WHERE jobs.id < c.id`},
		{postgres.New(), `WITH c AS (SELECT id FROM jobs FOR UPDATE), d AS (SELECT 1 AS id) UPDATE jobs SET status = 'x' FROM c, d WHERE jobs.id = c.id AND d.id = c.id`},
		{postgres.New(), `WITH c AS (SELECT id FROM others FOR UPDATE) UPDATE jobs SET status = 'x' FROM c WHERE jobs.id = c.id`},
		{postgres.New(), `WITH n AS (SELECT nextval('s') AS id) DELETE FROM jobs USING n WHERE jobs.id = n.id`},
		{postgres.New(), `WITH c AS (SELECT id FROM jobs FOR UPDATE) DELETE FROM jobs USING c WHERE jobs.id = c.id OR jobs.id = 1`},
		{postgres.New(), `WITH c AS (WITH unused AS (SELECT id FROM jobs) SELECT nextval('s') AS id) UPDATE jobs SET status = 'x' FROM c WHERE jobs.id = c.id`},
		{postgres.New(), `WITH c AS (SELECT nextval('s') AS id WHERE EXISTS (SELECT 1 FROM jobs)) UPDATE jobs SET status = 'x' FROM c WHERE jobs.id = c.id`},
		{postgres.New(), `WITH c AS (SELECT id FROM jobs UNION ALL SELECT nextval('s')) UPDATE jobs SET status = 'x' FROM c WHERE jobs.id = c.id`},
		{postgres.New(), `WITH c AS (SELECT count(*) + nextval('s') AS id FROM jobs) UPDATE jobs SET status = 'x' FROM c WHERE jobs.id = c.id`},
		{postgres.New(), `WITH c AS (SELECT j.id FROM jobs j JOIN jobs k ON k.id = j.id FOR UPDATE OF j) UPDATE jobs SET status = 'x' FROM c WHERE jobs.id = c.id`},
		{postgres.New(), `WITH c AS (SELECT id FROM jobs LIMIT nextval('s')) UPDATE jobs SET status = 'x' FROM c WHERE jobs.id = c.id`},
		{postgres.New(), `WITH c AS (SELECT id FROM jobs FOR UPDATE OFFSET (SELECT 0 FROM jobs LIMIT 1 FOR UPDATE)) UPDATE jobs SET status = 'x' FROM c WHERE jobs.id = c.id`},
		// A CTE sees only the ones before it.
		{postgres.New(), `WITH p AS (SELECT id FROM q), q AS (SELECT 1 AS id) UPDATE jobs SET status = 'x' FROM p, q WHERE jobs.id = p.id`},
		{postgres.New(), `WITH p AS (SELECT id FROM p) DELETE FROM jobs USING p WHERE jobs.id = p.id`},
		// Postgres's RETURNING * gives the FROM or USING items' columns too.
		{postgres.New(), `WITH c AS (SELECT 1 AS id) UPDATE jobs SET status = 'x' FROM c WHERE jobs.id = c.id RETURNING *`},
		{postgres.New(), `WITH c AS (SELECT 1 AS id) DELETE FROM jobs USING c WHERE jobs.id = c.id RETURNING *`},
		// An unread CTE is checked too, as Postgres analyzes it.
		{postgres.New(), `WITH c AS (SELECT to_char(now(), 'YYYY') AS d) UPDATE jobs SET status = 'x'`},
		{postgres.New(), `WITH c AS (SELECT to_char(now(), 'YYYY') AS d) DELETE FROM jobs`},
		// A CTE with effects runs in Postgres only as far as SET, RETURNING,
		// a subquery or another CTE asks for its rows.
		{postgres.New(), `WITH q AS (SELECT id FROM jobs FOR UPDATE SKIP LOCKED) DELETE FROM jobs WHERE id IN (SELECT id FROM q)`},
		{postgres.New(), `WITH q AS (SELECT id FROM jobs FOR UPDATE) UPDATE jobs SET status = (SELECT 'x' FROM q LIMIT 1)`},
		{postgres.New(), `WITH n AS (SELECT nextval('s') AS v) UPDATE jobs SET status = 'x' RETURNING (SELECT v FROM n)`},
		{postgres.New(), `WITH q AS (SELECT id FROM jobs FOR UPDATE), r AS (SELECT id FROM q) UPDATE jobs SET status = 'x' FROM r WHERE jobs.id = r.id`},
		{mysql.New(), "WITH c AS (SELECT 1 AS id) UPDATE jobs SET status = 'x' WHERE id IN (SELECT id FROM c)"},
		{mysql.New(), "WITH c AS (SELECT 1 AS id) DELETE FROM jobs WHERE id IN (SELECT id FROM c)"},
	} {
		if err := CheckSQL(tt.srv, tt.q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", tt.q, err)
		}
	}
	for _, q := range []string{
		`WITH c AS (SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED) UPDATE jobs SET status = 'running' FROM c WHERE jobs.id = c.id RETURNING jobs.*`,
		`WITH c AS (SELECT id FROM jobs WHERE status = 'done') DELETE FROM jobs WHERE id IN (SELECT id FROM c)`,
		`WITH c AS (SELECT id FROM jobs ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED) UPDATE jobs AS j SET status = 'x' FROM c WHERE c.id = j.id`,
		`WITH c AS (SELECT id FROM jobs WHERE status = 'done' FOR UPDATE) DELETE FROM jobs USING c WHERE jobs.id = c.id RETURNING jobs.id`,
	} {
		if err := CheckSQL(postgres.New(), q); err != nil {
			t.Errorf("%s: got %v, want nil", q, err)
		}
	}
}
