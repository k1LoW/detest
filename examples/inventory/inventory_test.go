package inventory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/k1LoW/detest"
	"github.com/k1LoW/detest/postgres"
)

// simulate declares two buyers of the last item, each calling reserve, and
// the invariant that no more items are reserved than were in stock.
func simulate(reserve func(context.Context, *sql.DB, string) error) func(t *testing.T, s *detest.Sim) {
	return func(t *testing.T, s *detest.Sim) {
		db, _ := s.DB("shop", postgres.New())
		if _, err := db.Exec(`CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		s.Seed(func() {
			if _, err := db.Exec(`INSERT INTO stock VALUES ('apple', 1)`); err != nil {
				t.Fatal(err)
			}
		})
		reserved := 0
		s.Seed(func() { reserved = 0 })
		for _, buyer := range []string{"alice", "bob"} {
			s.Manual(buyer, 1, func(p *detest.Proc) error {
				err := reserve(p.Context(), db, "apple")
				switch {
				case err == nil:
					reserved++
				case errors.Is(err, ErrOutOfStock):
				default:
					return err
				}
				return nil
			})
		}
		s.AtQuiescence(func(st *detest.State) error {
			if reserved > 1 {
				return fmt.Errorf("%d reservations of 1 item", reserved)
			}
			return nil
		})
	}
}

func TestReserveUnlockedLosesAnUpdate(t *testing.T) {
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		simulate(ReserveUnlocked)(t, s)
		s.ExpectViolation("2 reservations of 1 item")
	})
}

func TestReserve(t *testing.T) {
	detest.Explore(t, simulate(Reserve))
}
