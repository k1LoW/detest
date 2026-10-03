package detest

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/k1LoW/detest/postgres"
)

// claimJob declares two workers that each claim the next pending job with
// query and mark it running, and the invariant that no job is claimed twice.
func claimJob(t *testing.T, s *Sim, query string) {
	t.Helper()
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE queues (id text PRIMARY KEY)`)
	mustExec(t, db, `CREATE TABLE jobs (id text PRIMARY KEY, queue text NOT NULL, status text NOT NULL)`)
	s.Seed(func() {
		mustExec(t, db, `INSERT INTO queues VALUES ('q')`)
		mustExec(t, db, `INSERT INTO jobs VALUES ('j1', 'q', 'pending')`)
	})
	var claims map[string]int
	s.Seed(func() { claims = map[string]int{} })
	claim := func(p *Proc) error {
		tx, err := db.BeginTx(p.Context(), nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		var id string
		if err := tx.QueryRow(query).Scan(&id); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE jobs SET status = 'running' WHERE id = $1`, id); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		claims[id]++
		return nil
	}
	s.Manual("x", 1, claim)
	s.Manual("y", 1, claim)
	s.AtQuiescence(func(*State) error {
		if claims["j1"] > 1 {
			return fmt.Errorf("j1 claimed %d times", claims["j1"])
		}
		return nil
	})
}

// A locking read on a join locks the rows of the tables it reads.
func TestForUpdateWithJoinLocks(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		claimJob(t, s, `SELECT j.id FROM jobs j JOIN queues q ON q.id = j.queue WHERE j.status = 'pending' ORDER BY j.id LIMIT 1 FOR UPDATE OF j`)
	})
}

// After waiting for a row, a locking read evaluates its predicate again on
// the row's new version and leaves out a row that no longer matches.
func TestForUpdateRechecksAfterWait(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		claimJob(t, s, `SELECT id FROM jobs WHERE status = 'pending' ORDER BY id LIMIT 1 FOR UPDATE`)
	})
}

func TestForUpdateTargets(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE a (id text PRIMARY KEY)`)
	mustExec(t, db, `CREATE TABLE b (id text PRIMARY KEY)`)
	if _, err := db.Exec(`SELECT * FROM a LEFT JOIN b ON b.id = a.id FOR UPDATE`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("nullable side: got %v", err)
	}
	mustExec(t, db, `SELECT * FROM a LEFT JOIN b ON b.id = a.id FOR UPDATE OF a`)
	if _, err := db.Exec(`SELECT * FROM a FOR UPDATE OF c`); !errors.Is(err, ErrUndefinedTable) {
		t.Errorf("unknown OF: got %v", err)
	}
	if _, err := db.Exec(`SELECT * FROM a FOR UPDATE OF a FOR SHARE OF a`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("two locking clauses: got %v", err)
	}
}

// Postgres locks the tables behind a view or subquery, which detest does not
// do, so such a locking read is refused rather than run without locks.
func TestForUpdateOverDerivedItems(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE a (id text PRIMARY KEY)`)
	mustExec(t, db, `CREATE VIEW v AS SELECT id FROM a`)
	for _, q := range []string{
		`SELECT * FROM v FOR UPDATE`,
		`SELECT * FROM (SELECT id FROM a) s FOR UPDATE`,
		`SELECT * FROM a, LATERAL (SELECT a.id AS x) l FOR UPDATE OF a`,
		`WITH c AS (SELECT id FROM a) SELECT * FROM c FOR UPDATE OF c`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v", q, err)
		}
	}
	// A WITH query is not locked unless named, as in Postgres.
	mustExec(t, db, `WITH c AS (SELECT id FROM a) SELECT * FROM a JOIN c ON c.id = a.id FOR UPDATE`)

	// The locking clause is refused before the query runs.
	mustExec(t, db, `CREATE SEQUENCE s`)
	if _, err := db.Exec(`SELECT * FROM (SELECT nextval('s')) q FOR UPDATE`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("subquery: got %v", err)
	}
	if _, err := db.Exec(`WITH c AS (SELECT nextval('s') AS v) SELECT * FROM a JOIN c ON true FOR UPDATE OF missing`); !errors.Is(err, ErrUndefinedTable) {
		t.Errorf("unknown OF with a WITH query: got %v", err)
	}
	var n int64
	if err := db.QueryRow(`SELECT nextval('s')`).Scan(&n); err != nil || n != 1 {
		t.Errorf("nextval after a refused locking read: %d, %v", n, err)
	}
}

// After a wait, a row whose join partner no longer matches is left out, and
// a LEFT JOIN partner that no longer matches becomes NULL.
func TestForUpdateRecheckRejoins(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE b (id text PRIMARY KEY)`)
		mustExec(t, db, `CREATE TABLE a (id text PRIMARY KEY, bid text)`)
		s.Seed(func() {
			mustExec(t, db, `INSERT INTO b VALUES ('b1'), ('b2')`)
			mustExec(t, db, `INSERT INTO a VALUES ('a1', 'b1')`)
		})
		var pairs []string
		s.Seed(func() { pairs = nil })
		s.Manual("mover", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`UPDATE a SET bid = 'b2' WHERE id = 'a1'`); err != nil {
				return err
			}
			p.Step("holds a1")
			return tx.Commit()
		})
		for _, r := range []struct{ name, query string }{
			{"inner", `SELECT a.bid, b.id FROM a JOIN b ON b.id = a.bid AND b.id = 'b1' FOR UPDATE OF a`},
			{"left", `SELECT a.bid, b.id FROM a LEFT JOIN b ON b.id = a.bid AND b.id = 'b1' FOR UPDATE OF a`},
		} {
			s.Manual(r.name, 1, func(p *Proc) error {
				tx, err := db.BeginTx(p.Context(), nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				rows, err := tx.Query(r.query)
				if err != nil {
					return err
				}
				for rows.Next() {
					var abid string
					var bid sql.NullString
					if err := rows.Scan(&abid, &bid); err != nil {
						return err
					}
					pairs = append(pairs, abid+"/"+bid.String)
				}
				if err := rows.Close(); err != nil {
					return err
				}
				return tx.Commit()
			})
		}
		s.AtQuiescence(func(*State) error {
			for _, pr := range pairs {
				if pr == "b2/b1" {
					return fmt.Errorf("a row moved to b2 returned joined to b1")
				}
			}
			return nil
		})
	})
}

// The rows OFFSET skips are locked too, as in Postgres.
func TestForUpdateLocksOffsetRows(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE items (id text PRIMARY KEY)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO items VALUES ('a'), ('b')`) })
		refused := false
		s.Seed(func() { refused = false })
		s.Manual("x", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`SELECT id FROM items ORDER BY id OFFSET 1 LIMIT 1 FOR UPDATE`); err != nil {
				return err
			}
			p.Step("holds the rows")
			return tx.Commit()
		})
		s.Manual("y", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`SELECT id FROM items WHERE id = 'a' FOR UPDATE NOWAIT`); errors.Is(err, ErrLockNotAvailable) {
				refused = true
				return nil
			} else if err != nil {
				return err
			}
			return tx.Commit()
		})
		s.Sometimes("the row OFFSET skipped is locked", func(*State) bool { return refused })
	})
}

// A statement that waited on a row whose primary key the holder changed
// follows the row to its new key, as Postgres follows the update chain,
// rather than taking it for deleted.
func TestWaitFollowsPrimaryKeyChange(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"locking read", `SELECT id FROM jobs WHERE status = 'pending' FOR UPDATE`},
		{"update", `UPDATE jobs SET status = 'done' WHERE status = 'pending'`},
		{"delete", `DELETE FROM jobs WHERE status = 'pending'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, _ := s.DB("app", postgres.New())
				mustExec(t, db, `CREATE TABLE jobs (id int PRIMARY KEY, status text NOT NULL)`)
				s.Seed(func() { mustExec(t, db, `INSERT INTO jobs VALUES (1, 'pending')`) })
				var n int64
				s.Seed(func() { n = -1 })
				s.Manual("mover", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					if _, err := tx.Exec(`UPDATE jobs SET id = 2 WHERE id = 1`); err != nil {
						return err
					}
					p.Step("holds the row")
					return tx.Commit()
				})
				s.Manual("worker", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					if tc.name == "locking read" {
						rows, err := tx.Query(tc.query)
						if err != nil {
							return err
						}
						n = 0
						for rows.Next() {
							n++
						}
						if err := rows.Close(); err != nil {
							return err
						}
					} else {
						res, err := tx.Exec(tc.query)
						if err != nil {
							return err
						}
						if n, err = res.RowsAffected(); err != nil {
							return err
						}
					}
					return tx.Commit()
				})
				s.AtQuiescence(func(*State) error {
					if n != 1 {
						return fmt.Errorf("%s found %d rows, want the moved row", tc.name, n)
					}
					return nil
				})
			})
		})
	}
}
