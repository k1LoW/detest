package detest

import (
	"fmt"
	"testing"

	"github.com/k1LoW/detest/postgres"
)

// orderModel declares a checkout that records an order and publishes an event
// for a consumer to complete it, optionally with a sweeper that completes
// pending orders from the database, and the invariant that none stays
// pending.
func orderModel(sweeper bool) func(t *testing.T, s *Sim) {
	return func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE orders (id text PRIMARY KEY, status text NOT NULL)`)
		q := s.Queue("orders", Losses(1))
		complete := func(p *Proc, id string) error {
			_, err := db.ExecContext(p.Context(), `UPDATE orders SET status = 'done' WHERE id = $1`, id)
			return err
		}
		s.Manual("checkout", 1, func(p *Proc) error {
			if _, err := db.ExecContext(p.Context(), `INSERT INTO orders VALUES ('o1', 'pending')`); err != nil {
				return err
			}
			q.Enqueue(p, Msg{"id": "o1"})
			return nil
		})
		s.OnMessage("complete", q, func(p *Proc, msg Msg) error { return complete(p, msg.Str("id")) })
		if sweeper {
			pending := func(st *State) bool {
				row, ok := st.Row(store, "orders", "o1")
				return ok && row.Str("status") == "pending"
			}
			s.Manual("sweeper", 1, func(p *Proc) error { return complete(p, "o1") }, After(pending))
		}
		s.AtQuiescence(func(st *State) error {
			if row, _ := st.Row(store, "orders", "o1"); row.Str("status") == "pending" {
				return fmt.Errorf("order o1 left pending")
			}
			return nil
		})
	}
}

// A lost event leaves its order pending when nothing else completes it.
func TestLostMessageLeavesWorkUndone(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		orderModel(false)(t, s)
		s.ExpectViolation("left pending")
	})
}

// A sweeper over the database covers the lost event.
func TestBackstopCoversLostMessages(t *testing.T) {
	Explore(t, orderModel(true))
}

// Without Losses nothing is lost.
func TestNoLossByDefault(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE handled (id text PRIMARY KEY)`)
		q := s.Queue("events")
		s.Seed(func() { q.SeedMsg(Msg{"id": "e1"}) })
		s.OnMessage("consumer", q, func(p *Proc, msg Msg) error {
			_, err := db.ExecContext(p.Context(), `INSERT INTO handled VALUES ($1)`, msg.Str("id"))
			return err
		})
		s.AtQuiescence(func(st *State) error {
			if len(st.Rows(store, "handled")) == 0 {
				return fmt.Errorf("message e1 lost")
			}
			return nil
		})
	})
}
