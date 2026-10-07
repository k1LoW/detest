package detest

import (
	"errors"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// External.Do wraps a real in-process callee: with FailAfter the callee's
// write is committed although the caller sees a transport error.
func TestExternalDoFailAfterKeepsCalleeEffect(t *testing.T) {
	var runs, callerSawError, calleeCommitted int // accumulated across runs
	Explore(t, func(t *testing.T, s *Sim) {
		orderDB, db := s.DB("shared", postgres.New())
		invDB := db.Open()
		rpc := s.External("CancelOrder")
		s.Seed(func() {
			runs++
			_, _ = orderDB.Exec(`INSERT INTO "orders" ("id","status") VALUES ($1,$2)`, "o1", "PENDING")
		})
		s.Manual("caller", 1, func(p *Proc) error {
			err := rpc.Do(p, "o1", func() error { // the callee: a real handler would go here
				_, err := invDB.Exec(`UPDATE "orders" SET "status"=$1 WHERE "id" = $2`, "CANCELED", "o1")
				return err
			})
			if errors.Is(err, ErrUnavailable) {
				callerSawError++
			}
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			row, _ := st.Row(db, "orders", "o1")
			if row.Str("status") == "CANCELED" {
				calleeCommitted++
			}
			return nil
		})
	}, MaxFailures(1))
	// Three runs: success, fail-before, fail-after. The caller saw two errors,
	// yet the callee's write landed in two runs (success and fail-after).
	if runs != 3 || callerSawError != 2 || calleeCommitted != 2 {
		t.Fatalf("runs=%d callerSawError=%d calleeCommitted=%d", runs, callerSawError, calleeCommitted)
	}
}
