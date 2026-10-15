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
	mustExec(t, db, `CREATE VIEW seqv AS SELECT nextval('sq') AS n`)
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
		{`SELECT coalesce(sum(n), -1, sum(1 / n)) FROM t`, []string{"0"}},
		{`SELECT coalesce(0, sum(1 / n)) FROM t`, []string{"0"}},
		{`SELECT coalesce(sum(n), coalesce(0, sum(1 / n))) FROM t`, []string{"0"}},
		{`SELECT coalesce(sum(n), CASE WHEN true THEN 0 ELSE sum(1 / n) END) FROM t`, []string{"0"}},
	} {
		if got := rowsOf(t, db, tt.q); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
	// Postgres computes the aggregates before COALESCE picks among them.
	if _, err := db.Query(`SELECT coalesce(sum(n), sum(1 / n)) FROM t`); err == nil || !strings.Contains(err.Error(), "division by zero") {
		t.Errorf("an aggregate of an argument not reached: got %v, want division by zero", err)
	}
	// Postgres computes each aggregate once before the row's expressions,
	// where detest computes it at each reference, so a sequence call in
	// one is refused.
	for _, q := range []string{
		`SELECT coalesce(sum(n), max(nextval('sq'))) FROM t`,
		`SELECT coalesce(nextval('sq'), max(nextval('sq'))) FROM t`,
		`SELECT coalesce(nextval('sq'), max((SELECT n FROM seqv))) FROM t`,
		`SELECT coalesce(nextval('sq'), sum(1 / n) + nextval('sq')) FROM t`,
		`SELECT coalesce(sum(NULL::int), nextval('sq') + sum(1 / n)) FROM t`,
		`SELECT coalesce(sum(NULL::int), (SELECT n FROM seqv) + sum(1 / n)) FROM t`,
		`SELECT coalesce(sum(NULL::int), (SELECT n FROM t WHERE id = 1 FOR UPDATE) + sum(1 / n)) FROM t`,
		`WITH c AS (SELECT n FROM seqv) SELECT coalesce(sum(NULL::int), (SELECT n FROM c) + sum(1 / n)) FROM t`,
		`SELECT coalesce(nextval('sq'), max((SELECT n FROM t WHERE id = 1 FOR UPDATE))) FROM t`,
	} {
		if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	if got := rowsOf(t, db, `SELECT nextval('sq')`); !reflect.DeepEqual(got, []string{"4"}) {
		t.Errorf("nextval after the refusals: got %v, want [4]", got)
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
