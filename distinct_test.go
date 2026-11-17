package detest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// IS NOT DISTINCT FROM is = for two values that are not NULL, so the
// operands it has no = for fail as they do under =, whatever rows the
// statement reads and whether or not one of them is NULL.
func TestNotDistinctOperandsTypedAsEquals(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int, s text, at timestamptz)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, NULL, NULL, NULL)`)
	for _, q := range []string{
		`SELECT id FROM t WHERE n IS NOT DISTINCT FROM s`,
		`SELECT id FROM t WHERE n IS DISTINCT FROM s`,
		`SELECT id FROM t WHERE n IS NOT DISTINCT FROM s AND false`,
		`SELECT id FROM t WHERE at IS DISTINCT FROM n`,
		`SELECT id FROM t WHERE (n, s) IS NOT DISTINCT FROM (1, 'a')`,
		`SELECT id FROM t WHERE (n, s) IS DISTINCT FROM (SELECT 1, 'a')`,
		// A row that no row constructor shows the converter, which Postgres
		// tests as a whole against NULL and field by field against a row.
		`SELECT s.r IS NOT DISTINCT FROM NULL FROM (SELECT (n, s) AS r FROM t) s`,
		`SELECT s.r IS DISTINCT FROM s.r FROM (SELECT (n, s) AS r FROM t) s`,
		// IS DISTINCT FROM is NOT of the other, which detest does not
		// evaluate over an aggregate.
		`SELECT count(*) FROM t HAVING max(n) IS DISTINCT FROM 2`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	// A parameter takes the type of the other operand, NULL or not.
	for _, tt := range []struct {
		q    string
		arg  any
		want []string
	}{
		{`SELECT id FROM t WHERE n IS NOT DISTINCT FROM $1`, nil, []string{"1"}},
		{`SELECT id FROM t WHERE n IS NOT DISTINCT FROM $1`, int64(1), nil},
		{`SELECT id FROM t WHERE n IS DISTINCT FROM $1`, int64(1), []string{"1"}},
		{`SELECT id FROM t WHERE n IS DISTINCT FROM $1`, nil, nil},
	} {
		if got := rowsOf(t, db, tt.q, tt.arg); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s with %v: got %v, want %v", tt.q, tt.arg, got, tt.want)
		}
	}
	_, err := db.Exec(`SELECT id FROM t WHERE n IS NOT DISTINCT FROM $1`, "x")
	if se := new(*DBError); !errors.As(err, se) || (*se).Code != "22P02" {
		t.Errorf("text that is no integer: got %v, want SQLSTATE 22P02", err)
	}
}
