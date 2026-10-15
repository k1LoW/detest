package detest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// Postgres has no arithmetic over a boolean, a uuid or text, and fails the
// statement when it plans it, whatever rows it reads.
func TestArithmeticOperandTypesBeforeAnyRow(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int, b boolean, u uuid, s text)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 0, true, NULL, 'x')`)
	mustExec(t, db, `CREATE DOMAIN flag AS boolean`)
	mustExec(t, db, `CREATE DOMAIN uid AS uuid`)
	mustExec(t, db, `CREATE DOMAIN word AS text`)
	mustExec(t, db, `CREATE TABLE d (id int PRIMARY KEY, fb flag, fu uid, fw word)`)
	mustExec(t, db, `CREATE TABLE nums (n int)`)
	mustExec(t, db, `CREATE TABLE flags (n boolean)`)
	for _, q := range []string{
		`SELECT coalesce(n, n + true) FROM t`,
		`SELECT CASE WHEN n = 0 THEN 1 ELSE n + true END FROM t`,
		`SELECT n + true FROM t WHERE false`,
		`SELECT n * b FROM t WHERE false`,
		`SELECT u - u FROM t WHERE false`,
		`SELECT s + 1 FROM t WHERE false`,
		`SELECT -b FROM t WHERE false`,
		`SELECT n + s::text FROM t WHERE false`,
		// No operator takes a boolean or a uuid whatever type a literal
		// beside it would take.
		`SELECT b + 'true' FROM t WHERE false`,
		`SELECT 'true' + b FROM t WHERE false`,
		`SELECT u - 'x' FROM t WHERE false`,
		// Nor whatever type a bare NULL beside it would take.
		`SELECT b + NULL FROM t`,
		`SELECT NULL - u FROM t WHERE false`,
		// Postgres picks no prefix minus for an operand of no type.
		`SELECT -NULL FROM t WHERE false`,
		`SELECT -'1' FROM t WHERE false`,
		// Nor beside a type detest does not classify.
		`SELECT b + '{}'::jsonb FROM t WHERE false`,
		`SELECT '{}'::jsonb - u FROM t WHERE false`,
		// Postgres resolves a CASE, a COALESCE, a LAG or a LEAD of literals to
		// text.
		`SELECT -coalesce('1', NULL) WHERE false`,
		`SELECT coalesce('1', NULL) + 1 WHERE false`,
		`SELECT CASE WHEN true THEN '1' END + 1 WHERE false`,
		`SELECT lag('1') OVER () + 1 FROM t WHERE false`,
		`SELECT lead('1', 1, NULL) OVER () + 1 FROM t WHERE false`,
		// A character type other than text has no operator either.
		`SELECT '1'::bpchar + 1 WHERE false`,
		`SELECT -'1'::bpchar WHERE false`,
		`SELECT 1 * '1'::varchar WHERE false`,
		// A domain has the operators of its base type.
		`SELECT -fb FROM d WHERE false`,
		`SELECT fu - fu FROM d WHERE false`,
		`SELECT fw + 1 FROM d WHERE false`,
		`SELECT 1 + true::flag WHERE false`,
		// A scalar subquery's target is typed in its own scope.
		`SELECT (SELECT n FROM flags LIMIT 1) + 1 FROM nums WHERE false`,
	} {
		if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	for _, tt := range []struct {
		q   string
		arg any
	}{
		{`SELECT b + $1 FROM t WHERE false`, "true"},
		{`SELECT -$1 FROM t`, int64(5)},
		{`SELECT n + -$1 FROM t`, int64(5)},
		// Postgres takes this one, as the comparison it analyses first
		// types $1, but detest does not follow its order.
		{`SELECT $1 = n, -$1 FROM t`, int64(5)},
	} {
		if _, err := db.Query(tt.q, tt.arg); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", tt.q, err)
		}
	}
	// Numbers, and a literal that takes the other operand's type, run.
	for _, tt := range []struct {
		q    string
		want []string
	}{
		{`SELECT n + 1 FROM t`, []string{"1"}},
		{`SELECT -n FROM t`, []string{"0"}},
		{`SELECT coalesce(n, n + 1.5) FROM t`, []string{"0"}},
		{`SELECT (SELECT n FROM nums LIMIT 1) + 1 FROM flags WHERE false`, nil},
		{`SELECT n + NULL FROM t`, []string{"NULL"}},
		// '{}' - s resolves to jsonb - text.
		{`SELECT '{}'::jsonb - 'a'::bpchar WHERE false`, nil},
		{`SELECT ('{}' - s) - 'b'::text FROM t WHERE false`, nil},
		{`SELECT '{}'::jsonb - coalesce('a', NULL) WHERE false`, nil},
		{`SELECT '{}' - s FROM t WHERE false`, nil},
	} {
		if got := rowsOf(t, db, tt.q); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
	if got := rowsOf(t, db, `SELECT -$1::int FROM t`, int64(5)); !reflect.DeepEqual(got, []string{"-5"}) {
		t.Errorf("a cast parameter: got %v, want [-5]", got)
	}
}
