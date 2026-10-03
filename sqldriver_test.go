package detest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/k1LoW/detest/postgres"
)

// Two processes read-check-write the same row through database/sql with
// Postgres-dialect SQL. Under Read Committed the plain UPDATE loses an update;
// the CAS form (status in the WHERE) does not.
func TestSQLDriverLostUpdate(t *testing.T) {
	for _, cas := range []bool{false, true} {
		t.Run(fmt.Sprintf("cas=%v", cas), func(t *testing.T) {
			Explore(t, func(t *testing.T, sim *Sim) {
				sqlDB, db := sim.DB("app", postgres.New())
				commits := 0
				sim.Seed(func() {
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
				sim.Manual("withdraw_a", 1, withdraw)
				sim.Manual("withdraw_b", 1, withdraw)
				sim.AtQuiescence(func(s *State) error {
					row, _ := s.Row(db, "accounts", "a")
					if b := row.Int64("balance"); b != 100-30*int64(commits) {
						return fmt.Errorf("lost update: balance %d after %d committed withdrawals", b, commits)
					}
					return nil
				})
				if !cas {
					sim.ExpectViolation("lost update")
				}
			})
		})
	}
}
