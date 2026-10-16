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
	mustExec(t, db, `CREATE TABLE docs (j jsonb)`)
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
		// The only operator over text is jsonb - text, so text is
		// refused on the left, beside any other operator, and on the
		// right beside a type that is not jsonb.
		`SELECT s + NULL FROM t`,
		`SELECT s + NULL FROM t WHERE false`,
		`SELECT NULL + s FROM t WHERE false`,
		`SELECT s - NULL FROM t WHERE false`,
		`SELECT s - '{}' FROM t WHERE false`,
		`SELECT s - '{}'::jsonb FROM t WHERE false`,
		`SELECT 1 - s FROM t WHERE false`,
		// Postgres resolves a CASE, a COALESCE, a LAG or a LEAD of literals to
		// text.
		`SELECT -coalesce('1', NULL) WHERE false`,
		`SELECT coalesce('1', NULL) + 1 WHERE false`,
		`SELECT CASE WHEN true THEN '1' END + 1 WHERE false`,
		`SELECT lag('1') OVER () + 1 FROM t WHERE false`,
		`SELECT lead('1', 1, NULL) OVER () + 1 FROM t WHERE false`,
		// Or a wrapper of such, or of literals alone.
		`SELECT min(coalesce('1', NULL)) + 1 FROM t WHERE false`,
		`SELECT max('1') + 1 FROM t WHERE false`,
		`SELECT first_value(coalesce('1', NULL)) OVER () + 1 FROM t WHERE false`,
		`SELECT last_value('1') OVER () + 1 FROM t WHERE false`,
		`SELECT coalesce(coalesce('1', NULL), NULL) + 1 FROM t WHERE false`,
		`SELECT NULLIF('1', '2') + 1 WHERE false`,
		`SELECT coalesce(NULLIF('1', '2'), NULL) + 1 WHERE false`,
		`SELECT NULLIF(NULL, NULL) + 1 WHERE false`,
		`SELECT min(NULL) + 1 WHERE false`,
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
		{`SELECT j - 'a'::bpchar FROM docs WHERE false`, nil},
		{`SELECT ('{}' - s) - 'b'::text FROM t WHERE false`, nil},
		{`SELECT coalesce('{}' - s, NULL) - 'b'::text FROM t WHERE false`, nil},
		{`SELECT (SELECT '{}' - s) - 'b'::text FROM t WHERE false`, nil},
		{`SELECT NULL - s FROM t WHERE false`, nil},
		// abs of no type takes float8, the preferred number type.
		{`SELECT abs(NULL) + 1`, []string{"NULL"}},
		{`SELECT -abs(NULL) FROM t WHERE false`, nil},
		{`SELECT j - coalesce('a', NULL) FROM docs WHERE false`, nil},
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

// The type checks a statement gets before it runs read a scalar subquery's
// target in the subquery's own scope, not as the outer column of that name.
func TestSubqueryTargetTypedInItsOwnScope(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE DOMAIN day AS date`)
	mustExec(t, db, `CREATE DOMAIN span AS interval`)
	mustExec(t, db, `CREATE TABLE dates (id int PRIMARY KEY, n date)`)
	mustExec(t, db, `CREATE TABLE words (id int PRIMARY KEY, n text)`)
	mustExec(t, db, `CREATE TABLE nums (id int PRIMARY KEY, n int)`)
	mustExec(t, db, `INSERT INTO dates VALUES (1, '2026-01-02')`)
	mustExec(t, db, `INSERT INTO words VALUES (1, 'A')`)
	mustExec(t, db, `INSERT INTO nums VALUES (1, 2)`)
	for _, q := range []string{
		// Postgres gives an integer of days, where detest would give an
		// interval.
		`SELECT (SELECT n FROM dates LIMIT 1) - (SELECT n FROM dates LIMIT 1) FROM words`,
		// Postgres has no lower of an integer.
		`SELECT lower((SELECT n FROM nums LIMIT 1)) FROM words WHERE false`,
		// A domain over a date is a date there.
		`SELECT n::day - n::day FROM dates`,
		`WITH x AS (SELECT n::day AS n FROM dates) SELECT (SELECT n FROM x LIMIT 1) - (SELECT n FROM x LIMIT 1) FROM dates`,
		// date_trunc of an interval domain is an interval, which Postgres
		// does not compare with a timestamptz.
		`WITH x AS (SELECT interval '1 day' AS n) SELECT count(*) FROM (SELECT n::span AS n FROM x) y WHERE false AND date_trunc('day', n) = now()`,
	} {
		if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
}

// Postgres resolves a query output of string literals, parameters or NULLs
// alone to text where a query around reads it, so a comparison or
// arithmetic of it with a number fails when it plans the statement (42883),
// whatever rows the statement reads.
func TestQueryOutputOfLiteralsIsText(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int)`)
	mustExec(t, db, `CREATE VIEW v AS SELECT '1' AS x, NULL AS y`)
	for _, q := range []string{
		`SELECT x.v = 1 FROM (SELECT '1' AS v) x WHERE false`,
		`SELECT (SELECT '1') = 1 WHERE false`,
		`SELECT (SELECT max('1')) + 1 WHERE false`,
		`SELECT x.v + 1 FROM (SELECT '1' AS v) x WHERE false`,
		`SELECT x.v + 1 FROM (SELECT NULL AS v) x WHERE false`,
		`SELECT x.v = 1 FROM (SELECT NULL AS v) x WHERE false`,
		`SELECT x.v = 1 FROM (SELECT max(NULL) AS v) x WHERE false`,
		`SELECT x.v = 1 FROM (SELECT DISTINCT '1' AS v) x WHERE false`,
		`SELECT x.v = 1 FROM (SELECT min(NULL) OVER () AS v) x WHERE false`,
		`SELECT x.v = 1.5 FROM (SELECT '1' AS v) x WHERE false`,
		`SELECT s.a = 1 FROM (SELECT '1') s(a) WHERE false`,
		`SELECT s.v = 1 FROM (SELECT *, '1' AS v FROM t) s WHERE false`,
		`SELECT s.v = 1 FROM t, LATERAL (SELECT '1' AS v) s WHERE false`,
		`WITH c AS (SELECT '1' AS v) SELECT v = 1 FROM c WHERE false`,
		`SELECT x = 1 FROM v WHERE false`,
		`SELECT y + 1 FROM v WHERE false`,
		`SELECT n FROM t WHERE n = (SELECT max('1'))`,
		// These take anyelement, whose type Postgres cannot determine
		// from an untyped value (42804).
		`SELECT first_value('1') OVER ()`,
		`SELECT last_value(NULL) OVER ()`,
		`SELECT nth_value('1', 1) OVER ()`,
		`SELECT lag('1') OVER ()`,
		`SELECT lead(NULL, 1) OVER ()`,
		`SELECT x.v || 'a' FROM (SELECT first_value('1') OVER () AS v) x WHERE false`,
		// Postgres has no sum of text (42883).
		`SELECT x.v || 'x' FROM (SELECT sum(coalesce('1', NULL)) AS v WHERE false) x`,
		// Nor nests aggregates (42803).
		`SELECT x.v || 'a' FROM (SELECT max(min('1')) AS v WHERE false) x`,
		// Text has no assignment cast to an integer (42804).
		`INSERT INTO t (id, n) SELECT 1, x.v FROM (SELECT '1' AS v) x`,
		`INSERT INTO t (id, n) SELECT 1, min(NULL) OVER ()`,
	} {
		if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	for _, tt := range []struct {
		q   string
		arg any
	}{
		{`SELECT x.v + 1 FROM (SELECT $1 AS v) x WHERE false`, "1"},
		{`SELECT x.v = 1 FROM (SELECT $1 AS v) x WHERE false`, "1"},
		// The parameter is text for its other uses too.
		{`SELECT x.v || 'a' FROM (SELECT $1 AS v) x WHERE false AND $1 = 1`, "1"},
		{`SELECT x.v FROM (SELECT max($1) AS v) x WHERE $1 = 1`, "1"},
		{`SELECT (SELECT $1) = 'a', $1 = 1`, "1"},
		{`SELECT x.v || 'a', $1 + NULL FROM (SELECT $1 AS v) x WHERE false`, "1"},
		{`SELECT x.v || 'a', abs($1) FROM (SELECT $1 AS v) x WHERE false`, "1"},
		// The run reads a parameter bound to another value as bound,
		// where Postgres reads it as text.
		{`SELECT x.v || '', (x.v = '01')::text FROM (SELECT max($1) AS v) x`, int64(1)},
		{`SELECT (x.v = '01')::text FROM (SELECT $1 AS v) x`, int64(1)},
		{`SELECT coalesce($1, NULL) = '01'`, int64(1)},
		{`SELECT (SELECT $1) = '01'`, int64(1)},
		// Nor reads bytes there as the text they hold.
		{`SELECT CASE WHEN x.v = '\x41' THEN 'matched' ELSE x.v || '' END FROM (SELECT $1 AS v) x`, []byte("A")},
		{`SELECT (SELECT $1) = '\x41'`, []byte("A")},
		{`SELECT x.v || '' FROM (SELECT max($1) AS v) x`, []byte("A")},
	} {
		if _, err := db.Query(tt.q, tt.arg); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", tt.q, err)
		}
	}
	if got := rowsOf(t, db, `SELECT x.v || 'a', y.w FROM (SELECT $1 AS v) x, (SELECT $1 AS w) y`, "1"); !reflect.DeepEqual(got, []string{"1a,1"}) {
		t.Errorf("a text parameter read through derived tables: got %v, want [1a,1]", got)
	}
	// COALESCE reads bytes as the text they hold.
	if got := rowsOf(t, db, `SELECT x.v || 'b' FROM (SELECT coalesce($1, NULL) AS v) x`, []byte("A")); !reflect.DeepEqual(got, []string{"Ab"}) {
		t.Errorf("bytes under COALESCE read through a derived table: got %v, want [Ab]", got)
	}
	// An INSERT reads its own query's literals and parameters as the
	// columns' input.
	mustExec(t, db, `INSERT INTO t (id, n) SELECT 1, '1'`)
	mustExec(t, db, `INSERT INTO t (id, n) SELECT 2, NULL`)
	mustExec(t, db, `INSERT INTO t (id, n) SELECT 3, $1`, int64(3))
	if got := rowsOf(t, db, `SELECT n FROM t ORDER BY id`); !reflect.DeepEqual(got, []string{"1", "NULL", "3"}) {
		t.Errorf("INSERT ... SELECT of literals: got %v, want [1 NULL 3]", got)
	}
}

// Text has no assignment cast to a number, a boolean, a uuid, a time, an
// enum or json, so Postgres fails an UPDATE's SET, an INSERT's VALUES or an
// ON CONFLICT DO UPDATE's SET that writes text to such a column when it
// plans the statement (42804), whatever rows it reads. A literal, a
// parameter or a NULL is read as the column's input instead.
func TestTextAssignedToNonTextColumn(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TYPE mood AS ENUM ('a', 'b')`)
	mustExec(t, db, `CREATE DOMAIN dtext AS text`)
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int, s text, v varchar(10), d dtext, m mood, u uuid, j jsonb, ts timestamp, nm name, b bool)`)
	mustExec(t, db, `INSERT INTO t (id, n, s) VALUES (1, 0, '5')`)
	for _, q := range []string{
		`UPDATE t SET n = s WHERE false`,
		`UPDATE t SET n = s`,
		`UPDATE t SET n = v`,
		`UPDATE t SET n = d`,
		`UPDATE t SET n = '5'::text`,
		`UPDATE t SET n = s || '1'`,
		`UPDATE t SET n = '5' || '6'`,
		`UPDATE t SET n = lower('5')`,
		`UPDATE t SET n = CASE WHEN true THEN '5' END`,
		`UPDATE t SET n = CASE WHEN true THEN NULL END`,
		`UPDATE t SET n = COALESCE('5', NULL)`,
		`UPDATE t SET n = (SELECT '7')`,
		`UPDATE t SET n = (SELECT NULL)`,
		`UPDATE t SET n = (SELECT s FROM t LIMIT 1)`,
		`UPDATE t SET n = (SELECT max('1'))`,
		`UPDATE t SET n = s COLLATE "C"`,
		`UPDATE t SET m = s`,
		`UPDATE t SET u = s`,
		`UPDATE t SET j = s`,
		`UPDATE t SET ts = s`,
		`UPDATE t SET b = s`,
		`INSERT INTO t (id, n) VALUES (9, '5'::text)`,
		`INSERT INTO t (id, n) VALUES (10, '5'), (11, '6'::text)`,
		`INSERT INTO t (id, n) VALUES (14, CASE WHEN true THEN NULL END)`,
		`INSERT INTO t VALUES (9, '5'::text)`,
		`INSERT INTO t (id, n) VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET n = excluded.s`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	if _, err := db.Exec(`UPDATE t SET n = (SELECT $1)`, "7"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("UPDATE t SET n = (SELECT $1): got %v, want ErrUnsupportedSQL", err)
	}
	for _, q := range []string{
		`UPDATE t SET n = '5'`,
		`UPDATE t SET n = NULL`,
		`UPDATE t SET n = '5' COLLATE "C"`,
		`UPDATE t SET v = s, d = s, nm = s`,
		`INSERT INTO t (id, n) VALUES (12, '5'), (13, NULL)`,
	} {
		mustExec(t, db, q)
	}
	mustExec(t, db, `UPDATE t SET n = $1`, "6")
}
