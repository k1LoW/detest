package detest

import (
	"fmt"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// An INSERT reads its source once it runs, after its yield point, as
// Postgres takes the statement's snapshot when it executes it. Taken at the
// end of the process's previous step, the snapshot missed a commit of
// another process between the two statements.
func TestInsertRunsAfterItsYieldPoint(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("EagerStart=%v", on), func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, _ := s.DB("app", postgres.New())
				mustExec(t, db, `CREATE TABLE src (name text PRIMARY KEY)`)
				mustExec(t, db, `CREATE TABLE dst (name text PRIMARY KEY)`)
				var before, copied int
				s.Seed(func() { before, copied = -1, 0 })
				s.Manual("alice", 1, func(p *Proc) error {
					if err := db.QueryRowContext(p.Context(), `SELECT count(*) FROM src`).Scan(&before); err != nil {
						return err
					}
					res, err := db.ExecContext(p.Context(), `INSERT INTO dst SELECT name FROM src`)
					if err != nil {
						return err
					}
					n, err := res.RowsAffected()
					if err != nil {
						return err
					}
					copied = int(n)
					return nil
				})
				s.Manual("bob", 1, func(p *Proc) error {
					_, err := db.ExecContext(p.Context(), `INSERT INTO src VALUES ('bob')`)
					return err
				})
				s.Sometimes("alice copies a row committed after her count", func(*State) bool { return before == 0 && copied == 1 })
			}, EagerStart(on))
		})
	}
}
