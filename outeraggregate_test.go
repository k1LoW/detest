package detest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// An aggregate whose arguments read only outer columns belongs to the outer
// query in Postgres, which detest does not model, so it is refused.
func TestOuterLevelAggregateRefused(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int)`)
	mustExec(t, db, `CREATE TABLE u (id int PRIMARY KEY, m int)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 2), (2, 3)`)
	mustExec(t, db, `INSERT INTO u VALUES (1, 10)`)
	for _, q := range []string{
		`SELECT (SELECT sum(t.n)) FROM t`,
		`SELECT count(*), (SELECT max(t.n)) FROM t`,
		`SELECT (SELECT sum(t.n) FROM u) FROM t`,
		`SELECT id FROM t WHERE EXISTS (SELECT 1 FROM u HAVING max(t.n) > 0)`,
		`SELECT coalesce(sum(t.n), (SELECT sum(1 / t.n))) FROM t`,
		`SELECT (SELECT sum((SELECT t.n))) FROM t`,
		`SELECT (SELECT sum((SELECT u.m)) FROM u) FROM t`,
		`SELECT (SELECT sum(n) FROM generate_series(1, 1) WITH ORDINALITY AS g(k, ord)) FROM t`,
	} {
		if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	// An aggregate reading a column of its own query, or no column, is the
	// subquery's.
	for _, tt := range []struct {
		q    string
		want []string
	}{
		{`SELECT (SELECT sum(u.m) FROM u) FROM t ORDER BY id`, []string{"10", "10"}},
		{`SELECT (SELECT sum(u.m + t.n) FROM u) FROM t ORDER BY id`, []string{"12", "13"}},
		{`SELECT (SELECT count(*) FROM u WHERE u.m > t.n) FROM t ORDER BY id`, []string{"1", "1"}},
		{`SELECT (SELECT sum(m) FROM u) FROM t ORDER BY id`, []string{"10", "10"}},
		{`SELECT (SELECT sum(t.n) OVER () FROM u) FROM t ORDER BY id`, []string{"2", "3"}},
		{`SELECT (SELECT sum(k) FROM generate_series(1, 2) WITH ORDINALITY AS g(k, ord)) FROM t ORDER BY id`, []string{"3", "3"}},
	} {
		if got := rowsOf(t, db, tt.q); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
}
