package detest

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
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
		`SELECT 1 FROM a ORDER BY count(*) FOR UPDATE`,
		`SELECT row_number() OVER () IS NULL FROM a FOR UPDATE`,
		`SELECT count(*) IS NULL FROM a FOR UPDATE`,
		`SELECT string_agg(id, ',') FROM a FOR UPDATE`,
		`SELECT * FROM a WHERE count(*) > 0 FOR UPDATE`,
		`SELECT * FROM a JOIN a b ON row_number() OVER () > 0 FOR UPDATE`,
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
	t.Parallel()
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
			if slices.Contains(pairs, "b2/b1") {
				return fmt.Errorf("a row moved to b2 returned joined to b1")
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
		{"Tx.UpdateWhere", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, store := s.DB("app", postgres.New())
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
					if tc.query == "" {
						return store.Tx(p, func(tx *Tx) error {
							m, err := tx.UpdateWhere("jobs", func(r Row) bool { return r.Str("status") == "pending" }, Row{"status": "done"}, "status = pending")
							n = int64(m)
							return err
						})
					}
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

// A key a row was moved to and then deleted from ends the chain there, so a
// waiter does not follow an older move out of that key to another row.
func TestMoveChainEndsAtDeletedKey(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY)`)
	mustExec(t, db, `INSERT INTO t VALUES (1), (2)`)
	mustExec(t, db, `UPDATE t SET id = 3 WHERE id = 2`) // 2 -> 3
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE t SET id = 2 WHERE id = 1`); err != nil { // 1 -> 2
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM t WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	waiter := &Tx{db: store, writes: map[lockKey]Row{}, deleted: map[lockKey]bool{}}
	if key, _, ok := waiter.latest("t", "1"); ok {
		t.Errorf("row 1 was deleted, but its chain leads to %s", key)
	}
}

// Writing a column a generated key is computed from changes the key, so the
// update takes FOR UPDATE, as Postgres does.
func TestUpdateLockCountsGeneratedKeys(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE p (id int PRIMARY KEY, email text, note text, email_l text GENERATED ALWAYS AS (lower(email)) STORED UNIQUE)`)
	table := store.resolve("p")
	if got := store.updateLock(table, []string{"email"}); got != lockUpdate {
		t.Errorf("email: got %v, want FOR UPDATE", got)
	}
	if got := store.updateLock(table, []string{"note"}); got != lockNoKeyUpdate {
		t.Errorf("note: got %v, want FOR NO KEY UPDATE", got)
	}
}

// ON CONFLICT DO UPDATE that waited on the conflicting row follows it to the
// key it moved to and updates it there.
func TestUpsertFollowsMovedRow(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE u (id int PRIMARY KEY, email text UNIQUE, n int NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO u VALUES (1, 'a', 0)`) })
		s.Manual("mover", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`UPDATE u SET id = 2 WHERE id = 1`); err != nil {
				return err
			}
			p.Step("holds the row")
			return tx.Commit()
		})
		s.Manual("upsert", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), `INSERT INTO u VALUES (3, 'a', 1) ON CONFLICT (email) DO UPDATE SET n = u.n + 1`)
			return err
		})
		s.AtQuiescence(func(st *State) error {
			row, ok := st.Row(store, "u", "2")
			if !ok || row.Int64("n") != 1 {
				return fmt.Errorf("row 2 = %v, want n = 1", row)
			}
			return nil
		})
	})
}

// A referential action that waited on a child whose key moved follows it, so
// the child does not keep referencing a deleted parent.
func TestCascadeFollowsMovedChild(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE p (id int PRIMARY KEY)`)
		mustExec(t, db, `CREATE TABLE c (id int PRIMARY KEY, pid int REFERENCES p ON DELETE CASCADE)`)
		s.Seed(func() {
			mustExec(t, db, `INSERT INTO p VALUES (1)`)
			mustExec(t, db, `INSERT INTO c VALUES (1, 1)`)
		})
		s.Manual("mover", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`UPDATE c SET id = 2 WHERE id = 1`); err != nil {
				return err
			}
			p.Step("holds the child")
			return tx.Commit()
		})
		s.Manual("deleter", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), `DELETE FROM p WHERE id = 1`)
			return err
		})
		s.AtQuiescence(func(st *State) error {
			for _, key := range []string{"1", "2"} {
				if row, ok := st.Row(store, "c", key); ok {
					return fmt.Errorf("child %v outlived its parent", row)
				}
			}
			return nil
		})
	})
}

// The Tx API refuses a value for a generated column, as Postgres would.
func TestTxAPIRefusesGeneratedValues(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE g (id int PRIMARY KEY, a int, b int GENERATED ALWAYS AS (a * 2) STORED)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO g (id, a) VALUES (1, 1)`) })
		s.Manual("api", 1, func(p *Proc) error {
			return store.Tx(p, func(tx *Tx) error {
				if err := tx.Insert("g", Row{"id": 2, "a": 1, "b": 9}); !errors.As(err, new(*ErrUnsupportedSQL)) {
					t.Errorf("Insert: got %v", err)
				}
				if _, err := tx.Update("g", "1", Row{"b": 9}); !errors.As(err, new(*ErrUnsupportedSQL)) {
					t.Errorf("Update: got %v", err)
				}
				return nil
			})
		})
	})
}

// ON CONFLICT DO UPDATE that waited on a row whose conflicting value changed
// meanwhile inserts the proposed row instead of updating that one.
func TestUpsertRechecksConflictAfterWait(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE u (id int PRIMARY KEY, email text UNIQUE, n int NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO u VALUES (1, 'a', 0)`) })
		var written string
		s.Seed(func() { written = "" })
		s.Manual("renamer", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`UPDATE u SET email = 'b' WHERE id = 1`); err != nil {
				return err
			}
			p.Step("holds the row")
			return tx.Commit()
		})
		s.Manual("upsert", 1, func(p *Proc) error {
			return db.QueryRowContext(p.Context(), `INSERT INTO u VALUES (3, 'a', 1) ON CONFLICT (email) DO UPDATE SET n = u.n + 1 RETURNING email`).Scan(&written)
		})
		s.AtQuiescence(func(*State) error {
			// The row written, inserted or updated, holds the proposed email.
			if written != "a" {
				return fmt.Errorf("upsert wrote a row with email %q", written)
			}
			return nil
		})
	})
}

// A locking read that waited on a row deleted meanwhile keeps no lock on its
// key, so an insert of that key does not wait for the reader to finish.
func TestNoLockKeptOnDeletedRow(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE j (id int PRIMARY KEY)`)
	mustExec(t, db, `INSERT INTO j VALUES (1)`)
	reader := &Tx{db: store, writes: map[lockKey]Row{}, deleted: map[lockKey]bool{}}
	table := store.resolve("j")
	mustExec(t, db, `DELETE FROM j WHERE id = 1`)
	if _, _, ok, err := reader.lockLatest(table, "1", reader.lock); err != nil || ok {
		t.Fatalf("lockLatest: %v %v", ok, err)
	}
	if len(reader.locks) != 0 || len(store.locks[lockKey{table, "1"}]) != 0 {
		t.Errorf("lock kept on the deleted row: %v", reader.locks)
	}
}

// Following a moved row keeps no lock on the key it left, so an insert of
// that key does not wait.
func TestNoLockKeptOnLeftKey(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE j (id int PRIMARY KEY)`)
	mustExec(t, db, `INSERT INTO j VALUES (1)`)
	mustExec(t, db, `UPDATE j SET id = 2 WHERE id = 1`)
	reader := &Tx{db: store, writes: map[lockKey]Row{}, deleted: map[lockKey]bool{}}
	table := store.resolve("j")
	key, _, ok, err := reader.lockLatest(table, "1", reader.lock)
	if err != nil || !ok || key != "2" {
		t.Fatalf("lockLatest: %q %v %v", key, ok, err)
	}
	if len(store.locks[lockKey{table, "1"}]) != 0 || len(store.locks[lockKey{table, "2"}]) != 1 {
		t.Errorf("locks: %v", reader.locks)
	}
}

// A locking read evaluates its predicate once for a row no other transaction
// changed: a volatile predicate runs as often as in Postgres.
func TestLockingReadEvaluatesPredicateOnce(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE j (id int PRIMARY KEY)`)
	mustExec(t, db, `INSERT INTO j VALUES (1)`)
	mustExec(t, db, `CREATE SEQUENCE s`)
	mustExec(t, db, `SELECT id FROM j WHERE nextval('s') > 0 FOR UPDATE`)
	var n int64
	if err := db.QueryRow(`SELECT nextval('s')`).Scan(&n); err != nil || n != 2 {
		t.Errorf("nextval after one locking read of one row: %d, %v, want 2", n, err)
	}
}

// Postgres breaks a deadlock in whichever waiter's check finds the cycle
// first, which depends on timing, so the first waiter may be the victim as
// well as the transaction closing the cycle.
func TestDeadlockVictimIsEitherWaiter(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL)`)
		s.Seed(func() {
			mustExec(t, db, `INSERT INTO stock VALUES ('a', 1), ('b', 1)`)
		})
		var (
			victim           string
			aLocked, bLocked chan struct{}
		)
		s.Seed(func() {
			victim = ""
			aLocked, bLocked = make(chan struct{}), make(chan struct{})
		})
		// update writes first, closes locked, runs beforeSecond and writes
		// second, in one transaction.
		update := func(p *Proc, name, first string, locked chan struct{}, beforeSecond func(), second string) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`UPDATE stock SET n = 0 WHERE sku = $1`, first); err != nil {
				return err
			}
			close(locked)
			beforeSecond()
			if _, err := tx.Exec(`UPDATE stock SET n = 0 WHERE sku = $1`, second); errors.Is(err, ErrDeadlock) {
				victim = name
				return nil
			} else if err != nil {
				return err
			}
			return tx.Commit()
		}
		// x waits for b first; y closes the cycle once nothing else can run,
		// that is once x waits.
		s.Manual("x", 1, func(p *Proc) error {
			return update(p, "x", "a", aLocked, func() { <-bLocked }, "b")
		})
		s.Manual("y", 1, func(p *Proc) error {
			<-aLocked
			return update(p, "y", "b", bLocked, func() { p.WaitUntil(p.Now() + 1) }, "a")
		})
		s.Sometimes("the first waiter is the victim", func(*State) bool { return victim == "x" })
		s.Sometimes("the transaction closing the cycle is the victim", func(*State) bool { return victim == "y" })
	})
}

// lockAfterFailure declares a process that writes rows in a transaction, fails a
// statement and then waits for mu before ending the transaction, and another
// that writes row sku while holding mu. If the failed transaction still held
// the row lock, the two would wait for each other.
func lockAfterFailure(t *testing.T, s *Sim, savepoint bool, sku string) {
	t.Helper()
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL)`)
	s.Seed(func() {
		mustExec(t, db, `INSERT INTO stock VALUES ('a', 1), ('b', 1)`)
	})
	mu := s.Mutex("mu")
	s.Manual("x", 1, func(p *Proc) error {
		tx, err := db.BeginTx(p.Context(), nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`UPDATE stock SET n = 0 WHERE sku = 'a'`); err != nil {
			return err
		}
		if savepoint {
			if _, err := tx.Exec(`SAVEPOINT s`); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE stock SET n = 0 WHERE sku = 'b'`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO stock VALUES ('a', 1)`); !errors.Is(err, ErrUniqueViolation) {
			return fmt.Errorf("insert: got %w", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return nil
	})
	s.Manual("y", 1, func(p *Proc) error {
		mu.Lock()
		defer mu.Unlock()
		_, err := db.ExecContext(p.Context(), `UPDATE stock SET n = 5 WHERE sku = $1`, sku)
		return err
	})
}

// A failed statement aborts the transaction, and Postgres releases its row
// locks then rather than at ROLLBACK.
func TestFailedStatementReleasesLocks(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		lockAfterFailure(t, s, false, "a")
	})
}

// After a savepoint, a failed statement aborts only the subtransaction: the
// locks taken since the savepoint are released, the ones before are kept.
func TestFailedStatementAfterSavepointReleasesItsLocks(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		lockAfterFailure(t, s, true, "b")
	})
	Explore(t, func(t *testing.T, s *Sim) {
		s.ExpectViolation("closing a cycle of waits the database cannot detect")
		lockAfterFailure(t, s, true, "a")
	})
}

// A lock taken before a savepoint and strengthened after it goes back to its
// earlier strength when the subtransaction aborts, by a failed statement or by
// ROLLBACK TO. x holds FOR KEY SHARE on a while it waits for mu, which does
// not conflict with y's UPDATE; the FOR UPDATE it took after the savepoint
// would.
func TestSubtransactionAbortWeakensUpgradedLock(t *testing.T) {
	for _, end := range []string{"failure", "rollback to"} {
		t.Run(end, func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, _ := s.DB("app", postgres.New())
				mustExec(t, db, `CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL)`)
				s.Seed(func() {
					mustExec(t, db, `INSERT INTO stock VALUES ('a', 1)`)
				})
				mu := s.Mutex("mu")
				s.Manual("x", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					for _, q := range []string{
						`SELECT sku FROM stock WHERE sku = 'a' FOR KEY SHARE`,
						`SAVEPOINT s`,
						`SELECT sku FROM stock WHERE sku = 'a' FOR UPDATE`,
					} {
						if _, err := tx.Exec(q); err != nil {
							return err
						}
					}
					if end == "failure" {
						if _, err := tx.Exec(`INSERT INTO stock VALUES ('a', 1)`); !errors.Is(err, ErrUniqueViolation) {
							return fmt.Errorf("insert: got %w", err)
						}
					} else if _, err := tx.Exec(`ROLLBACK TO SAVEPOINT s`); err != nil {
						return err
					}
					mu.Lock()
					defer mu.Unlock()
					return nil
				})
				s.Manual("y", 1, func(p *Proc) error {
					mu.Lock()
					defer mu.Unlock()
					_, err := db.ExecContext(p.Context(), `UPDATE stock SET n = 5 WHERE sku = 'a'`)
					return err
				})
			})
		})
	}
}

// When two transactions sharing a row lock both wait for the one that closes
// the cycle, all three are on a cycle, and each may be the victim.
func TestDeadlockVictimOfBranchedCycle(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL)`)
		s.Seed(func() {
			mustExec(t, db, `INSERT INTO stock VALUES ('a', 1), ('b', 1)`)
		})
		var (
			victims         []string
			shared, bLocked chan struct{}
		)
		s.Seed(func() {
			victims = nil
			shared, bLocked = make(chan struct{}, 2), make(chan struct{})
		})
		run := func(p *Proc, name string, steps func(tx *sql.Tx) error) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if err := steps(tx); errors.Is(err, ErrDeadlock) {
				victims = append(victims, name)
				return nil
			} else if err != nil {
				return err
			}
			return tx.Commit()
		}
		for _, name := range []string{"x", "y"} {
			s.Manual(name, 1, func(p *Proc) error {
				return run(p, name, func(tx *sql.Tx) error {
					if _, err := tx.Exec(`SELECT sku FROM stock WHERE sku = 'a' FOR SHARE`); err != nil {
						return err
					}
					shared <- struct{}{}
					<-bLocked
					_, err := tx.Exec(`UPDATE stock SET n = 0 WHERE sku = 'b'`)
					return err
				})
			})
		}
		// z closes the cycle once x and y both wait for b.
		s.Manual("z", 1, func(p *Proc) error {
			<-shared
			<-shared
			return run(p, "z", func(tx *sql.Tx) error {
				if _, err := tx.Exec(`UPDATE stock SET n = 0 WHERE sku = 'b'`); err != nil {
					return err
				}
				close(bLocked)
				p.WaitUntil(p.Now() + 1)
				_, err := tx.Exec(`UPDATE stock SET n = 0 WHERE sku = 'a'`)
				return err
			})
		})
		for _, name := range []string{"x", "y", "z"} {
			s.Sometimes(name+" is the first victim", func(*State) bool { return len(victims) > 0 && victims[0] == name })
		}
	}, MaxPreemptions(0))
}

// A lock timeout that ends the wait closing a cycle breaks the cycle, so the
// other waiter goes on and no transaction is aborted as a deadlock victim.
func TestLockTimeoutBreaksDeadlock(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL)`)
		s.Seed(func() {
			mustExec(t, db, `INSERT INTO stock VALUES ('a', 1), ('b', 1)`)
		})
		var (
			errs             map[string]error
			aLocked, bLocked chan struct{}
		)
		s.Seed(func() {
			errs = map[string]error{}
			aLocked, bLocked = make(chan struct{}), make(chan struct{})
		})
		update := func(p *Proc, name, first string, locked chan struct{}, beforeSecond func(), second string) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if name == "y" {
				if _, err := tx.Exec(`SET LOCAL lock_timeout = '1s'`); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(`UPDATE stock SET n = 0 WHERE sku = $1`, first); err != nil {
				return err
			}
			close(locked)
			beforeSecond()
			if _, err := tx.Exec(`UPDATE stock SET n = 0 WHERE sku = $1`, second); err != nil {
				errs[name] = err
				return nil
			}
			return tx.Commit()
		}
		s.Manual("x", 1, func(p *Proc) error {
			return update(p, "x", "a", aLocked, func() { <-bLocked }, "b")
		})
		s.Manual("y", 1, func(p *Proc) error {
			<-aLocked
			return update(p, "y", "b", bLocked, func() { p.WaitUntil(p.Now() + 1) }, "a")
		})
		s.AtQuiescence(func(*State) error {
			if errors.Is(errs["y"], ErrLockNotAvailable) && errs["x"] != nil {
				return fmt.Errorf("y timed out, and x still failed: %w", errs["x"])
			}
			return nil
		})
		s.Sometimes("y times out", func(*State) bool { return errors.Is(errs["y"], ErrLockNotAvailable) })
	})
}

// A transaction its process keeps open while a second connection of the same
// process waits is idle, not waiting, so a cycle through it is not a deadlock
// Postgres detects but a hang.
func TestIdleTransactionIsNoDeadlock(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		s.ExpectViolation("closing a cycle of waits the database cannot detect")
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL)`)
		s.Seed(func() {
			mustExec(t, db, `INSERT INTO stock VALUES ('a', 1), ('b', 1)`)
		})
		var (
			aLocked, bLocked chan struct{}
			deadlocked       bool
		)
		s.Seed(func() {
			aLocked, bLocked = make(chan struct{}), make(chan struct{})
			deadlocked = false
		})
		s.Always(func(*State) error {
			if deadlocked {
				return errors.New("a deadlock was reported")
			}
			return nil
		})
		fail := func(err error) error {
			if errors.Is(err, ErrDeadlock) {
				deadlocked = true
				return nil
			}
			return err
		}
		s.Manual("x", 1, func(p *Proc) error {
			idle, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = idle.Rollback() }()
			if _, err := idle.Exec(`UPDATE stock SET n = 0 WHERE sku = 'a'`); err != nil {
				return err
			}
			close(aLocked)
			<-bLocked
			_, err = db.ExecContext(p.Context(), `UPDATE stock SET n = 0 WHERE sku = 'b'`)
			return fail(err)
		})
		s.Manual("y", 1, func(p *Proc) error {
			<-aLocked
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`UPDATE stock SET n = 0 WHERE sku = 'b'`); err != nil {
				return err
			}
			close(bLocked)
			p.WaitUntil(p.Now() + 1)
			_, err = tx.Exec(`UPDATE stock SET n = 0 WHERE sku = 'a'`)
			return fail(err)
		})
	})
}
