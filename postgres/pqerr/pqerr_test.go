package pqerr_test

import (
	"errors"
	"testing"

	"github.com/lib/pq"

	"github.com/k1LoW/detest"
	"github.com/k1LoW/detest/postgres"
	"github.com/k1LoW/detest/postgres/pqerr"
)

// Production code that branches on *pq.Error takes the same branch on
// detest's store.
func TestConvert(t *testing.T) {
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		db, _ := s.DB("app", postgres.New(postgres.Errors(pqerr.Convert)))
		if _, err := db.Exec(`CREATE TABLE users (id text PRIMARY KEY, email text UNIQUE)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO users (id, email) VALUES ('u1', 'a@x')`); err != nil {
			t.Fatal(err)
		}
		_, err := db.Exec(`INSERT INTO users (id, email) VALUES ('u2', 'a@x')`)
		var pqErr *pq.Error
		if !errors.As(err, &pqErr) || pqErr.Code.Name() != "unique_violation" || pqErr.Constraint != "users_email_key" || pqErr.Table != "users" {
			t.Errorf("unique violation: %#v", err)
		}
	})
}
