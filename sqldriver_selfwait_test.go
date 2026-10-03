package detest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/k1LoW/detest/postgres"
)

// An order handler updates an orders row inside a transaction and, while
// holding the row lock, calls the inventory service over RPC. The inventory
// service shares the database and writes the same row in its own
// transaction. In Postgres the callee waits for the caller's lock while the
// caller waits for the callee's response: no deadlock is detected, the request
// hangs until a timeout. detest reports it as a progress violation.
func TestSQLDriverRPCInsideTransactionSharedDB(t *testing.T) {
	for _, sameRow := range []bool{true, false} {
		name := "callee writes another row"
		if sameRow {
			name = "callee writes the same row"
		}
		t.Run(name, func(t *testing.T) {
			Explore(t, func(t *testing.T, sim *Sim) {
				orderDB, db := sim.DB("shared", postgres.New()) // order service's handle
				invDB := db.Open()                              // inventory service's handle, same database
				sim.Seed(func() {
					_, _ = orderDB.Exec(`INSERT INTO "orders" ("id","status") VALUES ($1,$2)`, "o1", "PENDING")
					_, _ = orderDB.Exec(`INSERT INTO "orders" ("id","status") VALUES ($1,$2)`, "o2", "PENDING")
				})
				inventoryService := func(ctx context.Context) error { // the RPC callee, in-process
					tx, err := invDB.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					target := "o2"
					if sameRow {
						target = "o1"
					}
					if _, err := tx.Exec(`UPDATE "orders" SET "status"=$1 WHERE "id" = $2`, "CANCELED", target); err != nil {
						_ = tx.Rollback()
						return err
					}
					return tx.Commit()
				}
				sim.Manual("order_handler", 1, func(p *Proc) error {
					ctx := p.Context()
					tx, err := orderDB.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					if _, err := tx.Exec(`UPDATE "orders" SET "status"=$1 WHERE "id" = $2`, "RESERVED", "o1"); err != nil {
						_ = tx.Rollback()
						return err
					}
					p.Step("calls the inventory service over RPC while holding the row lock")
					if err := inventoryService(ctx); err != nil {
						_ = tx.Rollback()
						return err
					}
					return tx.Commit()
				})
				if sameRow {
					sim.ExpectViolation("own open transaction")
				}
			})
		})
	}
}

var _ = sql.ErrNoRows
