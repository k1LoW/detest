package transfer

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/k1LoW/detest"
	"github.com/k1LoW/detest/postgres"
)

// simulate declares a transfer from a to b and one from b to a, and the
// invariants that every transfer succeeds and that money is neither created
// nor destroyed.
func simulate(transfer func(context.Context, *sql.DB, string, string, int) error) func(t *testing.T, s *detest.Sim) {
	return func(t *testing.T, s *detest.Sim) {
		db, store := s.DB("bank", postgres.New())
		if _, err := db.Exec(`CREATE TABLE accounts (id text PRIMARY KEY, balance int NOT NULL CHECK (balance >= 0))`); err != nil {
			t.Fatal(err)
		}
		s.Seed(func() {
			if _, err := db.Exec(`INSERT INTO accounts VALUES ('a', 100), ('b', 100)`); err != nil {
				t.Fatal(err)
			}
		})
		var failed []error
		s.Seed(func() { failed = nil })
		for _, tr := range [][2]string{{"a", "b"}, {"b", "a"}} {
			s.Manual(tr[0]+"_to_"+tr[1], 1, func(p *detest.Proc) error {
				if err := transfer(p.Context(), db, tr[0], tr[1], 10); err != nil {
					failed = append(failed, err)
				}
				return nil
			})
		}
		s.AtQuiescence(func(st *detest.State) error {
			if len(failed) > 0 {
				return fmt.Errorf("a transfer failed: %w", failed[0])
			}
			total := int64(0)
			for _, r := range st.Rows(store, "accounts") {
				total += r.Int64("balance")
			}
			if total != 200 {
				return fmt.Errorf("total balance %d, want 200", total)
			}
			return nil
		})
	}
}

func TestDebitFirstDeadlocks(t *testing.T) {
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		simulate(DebitFirst)(t, s)
		s.ExpectViolation("deadlock detected")
	})
}

func TestTransfer(t *testing.T) {
	detest.Explore(t, simulate(Transfer))
}
