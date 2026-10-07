package detest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
	"github.com/k1LoW/detest/internal/sqlir"
)

func TestPostgresScriptSessionLockTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial bool
		query   string
		finish  string
		want    bool
		wantErr bool
	}{
		{name: "autocommit reset", initial: true, query: `RESET ALL; SELECT 1`},
		{name: "autocommit set", query: `SET lock_timeout = '1s'; SELECT 1`, want: true},
		{name: "commit reset", initial: true, query: `RESET ALL; SELECT 1`, finish: "commit"},
		{name: "rollback reset", initial: true, query: `RESET ALL; SELECT 1`, finish: "rollback", want: true},
		{name: "failed script", initial: true, query: `RESET ALL; SELECT 1 / 0`, want: true, wantErr: true},
		{name: "local after session", query: `SET lock_timeout = '1s'; SET LOCAL lock_timeout = '0'`, finish: "commit", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSim(t)
			db, _ := s.DB("app", postgres.New())
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if tc.initial {
				if _, err := conn.ExecContext(context.Background(), `SET lock_timeout = '1s'`); err != nil {
					t.Fatal(err)
				}
			}
			if tc.finish == "" {
				_, err = conn.ExecContext(context.Background(), tc.query)
			} else {
				tx, beginErr := conn.BeginTx(context.Background(), nil)
				if beginErr != nil {
					t.Fatal(beginErr)
				}
				defer func() { _ = tx.Rollback() }()
				if _, err = tx.Exec(tc.query); err == nil {
					if tc.finish == "commit" {
						err = tx.Commit()
					} else {
						err = tx.Rollback()
					}
				}
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("got %v, want error: %v", err, tc.wantErr)
			}
			if err := conn.Raw(func(dc any) error {
				c, ok := dc.(*sqlConn)
				if !ok {
					return fmt.Errorf("unexpected connection type %T", dc)
				}
				if got := c.lockTimeout; got != tc.want {
					t.Errorf("session lock timeout: %v, want %v", got, tc.want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Two processes read-check-write the same row through database/sql with
// Postgres-dialect SQL. Under Read Committed the plain UPDATE loses an update;
// the CAS form (status in the WHERE) does not.
func TestSQLDriverLostUpdate(t *testing.T) {
	for _, cas := range []bool{false, true} {
		t.Run(fmt.Sprintf("cas=%v", cas), func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				sqlDB, db := s.DB("app", postgres.New())
				commits := 0
				s.Seed(func() {
					commits = 0
					if _, err := sqlDB.Exec(`INSERT INTO "accounts" ("id","balance") VALUES ($1,$2)`, "a", int64(100)); err != nil {
						t.Fatal(err)
					}
				})
				withdraw := func(p *Proc) error {
					ctx := context.Background()
					tx, err := sqlDB.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					var bal int64
					if err := tx.QueryRow(`SELECT "balance" FROM "accounts" WHERE "id" = $1`, "a").Scan(&bal); err != nil {
						_ = tx.Rollback()
						return err
					}
					if bal < 30 {
						return tx.Rollback()
					}
					var res sql.Result
					if cas {
						res, err = tx.Exec(`UPDATE "accounts" SET "balance"=$1 WHERE "id" = $2 AND "balance" = $3`, bal-30, "a", bal)
					} else {
						res, err = tx.Exec(`UPDATE "accounts" SET "balance"=$1 WHERE "id" = $2`, bal-30, "a")
					}
					if err != nil {
						_ = tx.Rollback()
						return err
					}
					if n, _ := res.RowsAffected(); n == 0 {
						return tx.Rollback()
					}
					if err := tx.Commit(); err != nil {
						return err
					}
					commits++
					return nil
				}
				s.Manual("withdraw_a", 1, withdraw)
				s.Manual("withdraw_b", 1, withdraw)
				s.AtQuiescence(func(st *State) error {
					row, _ := st.Row(db, "accounts", "a")
					if b := row.Int64("balance"); b != 100-30*int64(commits) {
						return fmt.Errorf("lost update: balance %d after %d committed withdrawals", b, commits)
					}
					return nil
				})
				if !cas {
					s.ExpectViolation("lost update")
				}
			})
		})
	}
}

// A process woken from a channel reports back to the scheduler before its
// statement runs, with no yield point or choice, so a statement that fails
// before its own yield point, and releases the locks of its transaction, does
// not run alongside the process that woke it. Run with -race.
func TestStatementAfterWakeFromChannel(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL)`)
		s.Seed(func() {
			mustExec(t, db, `INSERT INTO stock VALUES ('a', 1)`)
		})
		var wake chan struct{}
		s.Seed(func() { wake = make(chan struct{}, 1) })
		s.Manual("worker", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`UPDATE stock SET n = 0 WHERE sku = 'a'`); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO stock VALUES ('a', 1)`); !errors.Is(err, ErrUniqueViolation) {
				return fmt.Errorf("insert: got %w", err)
			}
			<-wake
			// The transaction is aborted, so this fails before its yield point.
			if _, err := tx.Exec(`SELECT 1`); !errors.Is(err, sqlir.ErrInFailedTx) {
				return fmt.Errorf("select: got %w", err)
			}
			return nil
		})
		s.Manual("waker", 1, func(p *Proc) error {
			wake <- struct{}{}
			p.WaitUntil(p.Now() + 1)
			return nil
		})
	})
}
