package detest

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// COALESCE stops at the first argument that is not NULL, so an argument
// after it neither fails the statement nor has its effect.
func TestCoalesceStopsAtFirstValue(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int)`)
	mustExec(t, db, `CREATE SEQUENCE sq`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 0)`)
	for _, tt := range []struct {
		q    string
		want []string
	}{
		{`SELECT coalesce(n, 1 / n) FROM t`, []string{"0"}},
		{`SELECT coalesce(sum(n), 1 / min(n)) FROM t`, []string{"0"}},
		{`SELECT coalesce(id, nextval('sq')) FROM t`, []string{"1"}},
		{`SELECT nextval('sq')`, []string{"1"}},
		{`SELECT coalesce(NULL, nextval('sq'), nextval('sq'))`, []string{"2"}},
		{`SELECT nextval('sq')`, []string{"3"}},
		{`SELECT coalesce(n, NULL, -1, 1 / 0) FROM t`, []string{"0"}},
		{`SELECT coalesce(sum(n), max(nextval('sq'))) FROM t`, []string{"0"}},
		{`SELECT nextval('sq')`, []string{"5"}},
	} {
		if got := rowsOf(t, db, tt.q); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
	// Postgres folds an argument of constants when it plans the statement,
	// and computes the aggregates before COALESCE picks among them.
	for _, q := range []string{
		`SELECT coalesce(n, 1 / 0) FROM t`,
		`SELECT coalesce(sum(n), sum(1 / n)) FROM t`,
	} {
		if _, err := db.Query(q); err == nil || !strings.Contains(err.Error(), "division by zero") {
			t.Errorf("%s: got %v, want division by zero", q, err)
		}
	}
	// Postgres reads a literal or a parameter as the arguments' type
	// whatever argument a row stops at.
	for _, tt := range []struct {
		q    string
		args []any
	}{
		{`SELECT coalesce(n, 'bogus') FROM t`, nil},
		{`SELECT coalesce(n, $1) FROM t`, []any{"bogus"}},
	} {
		_, err := db.Query(tt.q, tt.args...)
		if err == nil || !strings.Contains(err.Error(), "invalid input syntax for type integer") {
			t.Errorf("%s: got %v, want the integer input error", tt.q, err)
		}
	}
	// The evaluators run a CTE the statement reads before its rows, where
	// Postgres runs it only when a row reaches it.
	q := `WITH c AS (SELECT nextval('sq') AS v) SELECT coalesce(n, (SELECT v FROM c)) FROM t`
	if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
	}
}
