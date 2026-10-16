package detest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/k1LoW/detest/db/mysql"
	"github.com/k1LoW/detest/db/postgres"
)

// An aggregate or a window function in an aggregate's arguments fails on
// the server whatever rows the statement reads, so it is refused before any
// row, over an empty table as over one with rows. One in a window function's
// arguments is refused too, as detest does not evaluate it.
func TestNestedAggregateRefused(t *testing.T) {
	refused := []string{
		`SELECT max(min(n)) FROM t`,
		`SELECT max(min(1)) WHERE false`,
		`SELECT count(*) FROM t HAVING max(min(n)) > 0`,
		`SELECT max(n) FROM t GROUP BY n HAVING sum(max(n)) > 0`,
		`SELECT max(count(*)) FROM t GROUP BY n`,
		`SELECT coalesce(max(abs(min(n))), 0) FROM t`,
		`SELECT max(CASE WHEN n > 0 THEN min(n) END) FROM t`,
		`SELECT (SELECT max(min(u.n)) FROM t u) FROM t`,
		`SELECT max(sum(n) OVER ()) FROM t`,
		`SELECT sum(max(n)) OVER () FROM t`,
		`SELECT sum(sum(n) OVER ()) OVER () FROM t`,
	}
	accepted := []struct {
		q    string
		want []string
	}{
		{`SELECT max(abs(n)) FROM t`, []string{"NULL"}},
		{`SELECT max(n) + min(n) FROM t`, []string{"NULL"}},
		{`SELECT coalesce(max(n), 0) FROM t`, []string{"0"}},
		{`SELECT count(DISTINCT n) FROM t`, []string{"0"}},
		{`SELECT max(n), (SELECT min(u.n) FROM t u) FROM t`, []string{"NULL,NULL"}},
		{`SELECT sum(n) OVER (ORDER BY n) FROM t`, nil},
		{`SELECT row_number() OVER (ORDER BY n) FROM t`, nil},
	}
	for _, tt := range []struct {
		name string
		srv  Server
	}{
		{"postgres", postgres.New()},
		{"mysql", mysql.New()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newSim(t)
			db, _ := s.DB("app", tt.srv)
			mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int)`)
			for _, q := range refused {
				if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
					t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
				}
			}
			for _, a := range accepted {
				if got := rowsOf(t, db, a.q); !reflect.DeepEqual(got, a.want) {
					t.Errorf("%s: got %v, want %v", a.q, got, a.want)
				}
			}
		})
	}
}
