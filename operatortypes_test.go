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

// Postgres has no sum or avg of a type other than a number or an interval,
// and fails the statement when it plans it, whatever rows it reads.
func TestSumAndAvgArgumentTypesBeforeAnyRow(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE DOMAIN word AS text`)
	mustExec(t, db, `CREATE DOMAIN dint AS int`)
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, s text, b boolean, d date, at timestamptz, u uuid, j jsonb, a int[], v varchar(10), w word, k int)`)
	for _, q := range []string{
		`SELECT sum(s) FROM t`,
		`SELECT avg(s) FROM t`,
		`SELECT sum(b) FROM t`,
		`SELECT avg(b) FROM t`,
		`SELECT sum(d) FROM t`,
		`SELECT sum(at) FROM t`,
		`SELECT sum(u) FROM t`,
		`SELECT sum(j) FROM t`,
		`SELECT sum(a) FROM t`,
		`SELECT sum(v) FROM t`,
		`SELECT sum(w) FROM t`,
		`SELECT sum(s || 'a') FROM t`,
		`SELECT sum(DISTINCT s) FROM t`,
		`SELECT sum(s) OVER () FROM t`,
		`SELECT count(*) FROM t HAVING sum(s) IS NULL`,
		`SELECT (SELECT avg(b) FROM t)`,
		`SELECT sum(x.v) FROM (SELECT '1' AS v) x WHERE false`,
		`SELECT sum('1') FROM t`,
		`SELECT avg(NULL) FROM t`,
		`SELECT sum(coalesce(NULL, NULL)) FROM t`,
		`SELECT avg(CASE WHEN b THEN '1' END) FROM t`,
		`SELECT sum(ROW(1, 2)) FROM t`,
		`SELECT avg(ROW(k, k)) FROM t`,
		`SELECT sum(coalesce(ROW(1, 2), NULL)) FROM t`,
		`SELECT avg(CASE WHEN true THEN ROW(1, 2) END) FROM t`,
		`SELECT sum(nullif(ROW(k, k), ROW(1, 1))) FROM t`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
	for _, q := range []string{
		`SELECT sum(k), avg(k), sum(b::int), sum(at - at) FROM t`,
		`SELECT sum(x.k) FROM (SELECT k FROM t) x`,
		`SELECT sum(CASE WHEN b THEN 1 END), sum(coalesce(k, NULL)) FROM t`,
		`SELECT sum(k::dint), avg(k::dint) FROM t`,
		`SELECT sum(CASE WHEN ROW(k, k) = ROW(1, 1) THEN 1 END) FROM t`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("%s: %v, want nil", q, err)
		}
	}
}

// A query output that is a row is a record to the query around it, which
// has no arithmetic, no function of a number or of text and no sum or avg,
// so Postgres fails a statement using one so with 42883 whatever rows it
// reads, and one testing it as a condition with 42804.
func TestRowValuedOutputBeforeAnyRow(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, k int)`)
	for _, q := range []string{
		`SELECT x.v + 1 FROM (SELECT ROW(1, 2) AS v) x WHERE false`,
		`SELECT abs(x.v) FROM (SELECT ROW(1, 2) AS v) x WHERE false`,
		`SELECT lower(x.v) FROM (SELECT ROW(1, 2) AS v) x WHERE false`,
		`SELECT sum(x.v) FROM (SELECT ROW(1, 2) AS v) x WHERE false`,
		`SELECT avg(x.v) FROM (SELECT ROW(1, 2) AS v) x WHERE false`,
		`SELECT -x.v FROM (SELECT ROW(1, 2) AS v) x WHERE false`,
		`SELECT length(x.v) FROM (SELECT ROW(1, 2) AS v) x WHERE false`,
		`SELECT 1 FROM (SELECT ROW(1, 2) AS v) x WHERE x.v`,
		`SELECT x.v + 1 FROM (SELECT (1, 2) AS v) x WHERE false`,
		`SELECT x.v + 1 FROM (SELECT ROW(k, k) AS v FROM t) x`,
		`SELECT x.v + 1 FROM (SELECT CASE WHEN true THEN ROW(1, 2) END AS v) x WHERE false`,
		`SELECT abs(x.v) FROM (SELECT coalesce(ROW(1, 2), NULL) AS v) x WHERE false`,
		`WITH c AS (SELECT ROW(1, 2) AS v) SELECT c.v + 1 FROM c WHERE false`,
		`SELECT (SELECT ROW(1, 2)) + 1 WHERE false`,
		`SELECT y.w + 1 FROM (SELECT x.v AS w FROM (SELECT ROW(1, 2) AS v) x) y WHERE false`,
		`SELECT x.v + 1 FROM (SELECT * FROM (SELECT ROW(1, 2) AS v) y) x WHERE false`,
		`SELECT x.v + 1 FROM (SELECT ROW(1, 2) AS v UNION ALL SELECT ROW(3, 4)) x WHERE false`,
		`SELECT abs(x.v) FROM (SELECT ROW(1, 2) AS v UNION ALL SELECT NULL) x WHERE false`,
		`SELECT abs(x.v) FROM (VALUES (ROW(1, 2))) x(v) WHERE false`,
		`SELECT abs((VALUES (ROW(1, 2)))) WHERE false`,
		`SELECT abs((SELECT ROW(1, 2) UNION ALL SELECT ROW(3, 4))) WHERE false`,
		`SELECT abs(x.v) FROM (VALUES (ROW(1, 2)) UNION ALL VALUES (ROW(3, 4))) x(v) WHERE false`,
		`SELECT abs(x.v) FROM (VALUES (ROW(1, 2), 1) UNION ALL SELECT ROW(3, 4), 2) x(v, a) WHERE false`,
		`SELECT 1 IN (VALUES (ROW(1, 2))) WHERE false`,
		`SELECT abs(x.v) FROM (VALUES ((SELECT ROW(1, 2))) UNION ALL VALUES ((SELECT ROW(3, 4)))) x(v) WHERE false`,
		`SELECT abs(x.v) FROM (SELECT y.v FROM (SELECT ROW(1, 2) AS v) y UNION ALL VALUES (NULL)) x(v) WHERE false`,
		`SELECT abs(x.v) FROM (VALUES (NULL) UNION ALL SELECT ROW(1, 2)) x(v) WHERE false`,
		`SELECT abs(x.v) FROM (SELECT first_value(ROW(k, k)) OVER () AS v FROM t) x WHERE false`,
		`SELECT abs(x.v) FROM (SELECT last_value(ROW(k, k)) OVER () AS v FROM t) x WHERE false`,
		`SELECT abs(x.v) FROM (SELECT nth_value(ROW(k, k), 1) OVER () AS v FROM t) x WHERE false`,
		`SELECT abs(x.v) FROM (SELECT lag(ROW(k, k)) OVER () AS v FROM t) x WHERE false`,
		`SELECT abs(x.v) FROM (SELECT lead(ROW(k, k), 1, NULL) OVER () AS v FROM t) x WHERE false`,
		`SELECT abs(x.v) FROM (SELECT max(ROW(k, k)) AS v FROM t) x WHERE false`,
		`SELECT abs(x.v) FROM (SELECT min(ROW(k, k)) OVER () AS v FROM t) x WHERE false`,
		`SELECT abs(x.v) FROM (SELECT *, ROW(1, 2) AS v FROM t UNION ALL SELECT 1, 2, ROW(3, 4)) x WHERE false`,
		// A record has no comparison with a value of another type either.
		`SELECT x.v = 1 FROM (SELECT ROW(1, 2) AS v) x WHERE false`,
		`SELECT x.v < 'a' FROM (SELECT ROW(1, 2) AS v) x WHERE false`,
		`SELECT (x.v, 1) = (1, 1) FROM (SELECT ROW(1, 2) AS v) x WHERE false`,
		`SELECT x.v IN (1, 2) FROM (SELECT ROW(1, 2) AS v) x WHERE false`,
		`SELECT x.v BETWEEN 1 AND 2 FROM (SELECT ROW(1, 2) AS v) x WHERE false`,
		`SELECT CASE x.v WHEN 1 THEN 1 END FROM (SELECT ROW(1, 2) AS v) x WHERE false`,
		`SELECT x.v IN (SELECT 1) FROM (SELECT ROW(1, 2) AS v) x WHERE false`,
		`SELECT 1 IN (SELECT x.v FROM (SELECT ROW(1, 2) AS v) x) WHERE false`,
		`SELECT 1 IN (SELECT ROW(1, 2) UNION ALL SELECT ROW(3, 4)) WHERE false`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
	for _, q := range []string{
		`SELECT x.v IS NULL, count(x.v) FROM (SELECT ROW(1, 2) AS v) x GROUP BY x.v`,
		`SELECT coalesce(x.v, ROW(3, 4)) IS NULL FROM (SELECT ROW(1, 2) AS v) x`,
		`SELECT CASE WHEN x.v IS NULL THEN 0 ELSE 1 END FROM (SELECT ROW(1, 2) AS v) x`,
		`SELECT x.v IS NULL FROM (VALUES (ROW(1, 2))) x(v)`,
		`SELECT abs(x.a) FROM (SELECT ROW(1, 2) AS v, 1 AS a) x`,
		`SELECT x.v IS NULL, abs(x.a) FROM (VALUES (ROW(1, 2), -1) UNION ALL VALUES (ROW(3, 4), -2)) x(v, a)`,
		`SELECT (VALUES (ROW(1, 2))) IS NULL`,
		`SELECT x.v IS NULL FROM (SELECT first_value(ROW(k, k)) OVER () AS v, max(ROW(k, k)) OVER () AS w FROM t) x`,
		`SELECT x.v IS NULL FROM (SELECT *, ROW(1, 2) AS v FROM t UNION ALL SELECT 1, 2, ROW(3, 4)) x`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("%s: %v, want nil", q, err)
		}
	}
}

// Postgres has no arithmetic, no unary minus and no function of a number,
// of text, of a time or of a lock key that takes a record, so it fails a row
// of columns used so with 42883 when it plans the statement, whatever rows
// it reads.
func TestRowOfColumnsAsOperandBeforeAnyRow(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int)`)
	for _, q := range []string{
		`SELECT ROW(n, 1) + 1 FROM t`,
		`SELECT 1 * ROW(n, 1) FROM t`,
		`SELECT ROW(n, 1) - 'a'::text FROM t`,
		`SELECT -ROW(n, 1) FROM t`,
		`SELECT -(CASE WHEN n > 0 THEN ROW(n, 1) END) FROM t`,
		`SELECT max(ROW(n, 1)) + 1 FROM t`,
		`SELECT abs(ROW(n, 1)) FROM t`,
		`SELECT floor(coalesce(ROW(n, 1), NULL)) FROM t`,
		`SELECT power(2, ROW(n, 1)) FROM t`,
		`SELECT lower(ROW(n, 1)) FROM t`,
		`SELECT length(ROW(n, 1)) FROM t`,
		`SELECT hashtext(ROW(n, 1)) FROM t`,
		`SELECT left(ROW(n, 1), 1) FROM t`,
		`SELECT left('a', ROW(n, 1)) FROM t`,
		`SELECT date_trunc('day', ROW(n, 1)) FROM t`,
		`SELECT date_trunc(ROW(n, 1), now()) FROM t`,
		`SELECT pg_advisory_xact_lock(ROW(n, 1)) FROM t`,
		`SELECT 1 FROM t WHERE false AND ROW(n, 1) + 1 IS NULL`,
		`UPDATE t SET n = 1 WHERE abs(ROW(n, 1)) IS NULL`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
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

// Postgres reads a string literal or a parameter in a column of VALUES as
// the type of the column's other rows, which the run converts it to where
// it can and refuses elsewhere.
func TestValuesColumnUntypedTakesRowsType(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	for _, tt := range []struct {
		q    string
		args []any
	}{
		{`SELECT column1 FROM (VALUES ('true'), (false)) v`, nil},
		{`SELECT column1 FROM (VALUES ($1), (false)) v`, []any{"true"}},
		{`SELECT column1 FROM (VALUES ('00000000-0000-0000-0000-000000000000'), (gen_random_uuid())) v`, nil},
		{`SELECT column1 FROM (VALUES ($1), (2)) v`, []any{true}},
		{`SELECT column1 FROM (VALUES ($1), (2)) v`, []any{1.5}},
	} {
		if _, err := db.Query(tt.q, tt.args...); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s %v: got %v, want ErrUnsupportedSQL", tt.q, tt.args, err)
		}
	}
	for _, tt := range []struct {
		q    string
		arg  any
		want []string
	}{
		{`SELECT (column1 = '01')::text FROM (VALUES ($1), (2)) v ORDER BY 1`, "1", []string{"false", "true"}},
		{`SELECT (column1 = '01')::text FROM (VALUES ($1), (2.5)) v ORDER BY 1`, "1.0", []string{"false", "true"}},
	} {
		if got := rowsOf(t, db, tt.q, tt.arg); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
	if _, err := db.Query(`SELECT column1 FROM (VALUES ($1), (2)) v`, "x"); !errors.Is(err, ErrInvalidTextRepresentation) {
		t.Errorf("x beside an integer: got %v, want 22P02", err)
	}
}

// Postgres resolves a column of VALUES of string literals, parameters and
// NULLs alone to text, or one beside text to text, and reads a parameter
// there as the text the driver sends, which the run does not reproduce for
// a value bound to other than a string.
func TestValuesColumnParamIsText(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	for _, tt := range []struct {
		q   string
		arg any
	}{
		{`SELECT (coalesce(v.column1, 'z') = '01')::text FROM (VALUES ($1)) v`, int64(1)},
		{`SELECT (coalesce((SELECT column1 FROM (VALUES ($1)) v), 'z') = '01')::text`, int64(1)},
		{`SELECT (coalesce((SELECT * FROM (VALUES ($1)) v), 'z') = '01')::text`, int64(1)},
		{`WITH v AS (VALUES ($1)) SELECT (column1 = '01')::text FROM v`, int64(1)},
		{`SELECT (x = '01')::text FROM (VALUES ($1, 1)) v(x, y)`, int64(1)},
		{`SELECT (column1 = '01')::text FROM (VALUES ($1), (NULL)) v`, int64(1)},
		{`SELECT (column1 = '01')::text FROM (VALUES ($1), ('a'::text)) v`, int64(1)},
		{`SELECT column1 FROM (VALUES ($1)) v`, true},
		{`SELECT column1 || '' FROM (VALUES ($1)) v`, []byte("A")},
	} {
		if _, err := db.Query(tt.q, tt.arg); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", tt.q, err)
		}
	}
	for _, tt := range []struct {
		q    string
		arg  any
		want []string
	}{
		{`SELECT (coalesce(v.column1, 'z') = '01')::text FROM (VALUES ($1)) v`, "1", []string{"false"}},
		// Postgres resolves the column to integer, and the parameter with
		// it.
		{`SELECT (column1 = '01')::text FROM (VALUES ($1), (2)) v ORDER BY 1`, int64(1), []string{"false", "true"}},
		{`SELECT (column1 = '01')::text FROM (VALUES ($1::int)) v`, int64(1), []string{"true"}},
	} {
		if got := rowsOf(t, db, tt.q, tt.arg); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
}

// Postgres resolves the operator of each pair of fields of a row comparison
// when it plans the statement, so a pair it has no operator for fails it
// (42883), as does IN (SELECT ...) of another width (42601), whatever rows
// it reads.
func TestRowComparisonFieldTypesBeforeAnyRow(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, s text, n int, b boolean)`)
	for _, q := range []string{
		`SELECT (1, 2) = (s, 2) FROM t WHERE false`,
		`SELECT (1, 2) < (s, 2) FROM t WHERE false`,
		`SELECT (1, b) = (n, 2) FROM t WHERE false`,
		`SELECT (now(), 2) = (n, 2) FROM t WHERE false`,
		`SELECT (1, 2) IN ((s, 2)) FROM t WHERE false`,
		`SELECT (1, 2) IN ((2, 3), (n::text, 2)) FROM t WHERE false`,
		`SELECT (1, 2) = (SELECT s, 2 FROM t) WHERE false`,
		`SELECT (1, 2) = (SELECT x.s, 2 FROM (SELECT '1' AS s) x) WHERE false`,
		`SELECT (1, 2) = (SELECT 1, max('1')) WHERE false`,
		`SELECT 1 FROM t WHERE (1, 2) IN (SELECT s, 2 FROM t)`,
		`SELECT 1 FROM t WHERE (1, 2) NOT IN (SELECT s, 2 FROM t)`,
		`SELECT 1 FROM t WHERE (1, 2) = ANY (SELECT s, 2 FROM t)`,
		`SELECT (s, 2) IN (SELECT n, 2 FROM t) FROM t WHERE false`,
		`SELECT 1 FROM t WHERE 1 IN (SELECT s FROM t)`,
		`SELECT 1 = ANY (SELECT s FROM t) WHERE false`,
		// Postgres resolves a string literal or a NULL among a subquery's
		// columns to text.
		`SELECT (1, 2) = (SELECT '1', 2) WHERE false`,
		`SELECT (1, 2) = (SELECT NULL, 2) WHERE false`,
		`SELECT (1, 2) IN (SELECT '1', 2) WHERE false`,
		`SELECT 1 IN (SELECT NULL) WHERE false`,
		`SELECT (1, 2) IN (VALUES ('1', 2)) WHERE false`,
		`SELECT 1 IN (VALUES ('1')) WHERE false`,
		// A field that is a row is a record, which Postgres has no
		// operator with a number or text for, and reads a string literal
		// beside it as a record's input, which it does not implement.
		`SELECT ((n, 2), 3) = (1, 3) FROM t WHERE false`,
		`SELECT (1, 3) = ((n, 2), 3) FROM t WHERE false`,
		`SELECT ((n, 2), 3) = (s, 3) FROM t WHERE false`,
		`SELECT ((n, 2), 3) = ('(1,2)', 3) FROM t WHERE false`,
		`SELECT ((n, 2), 3) IN (SELECT 1, 3) FROM t WHERE false`,
		`SELECT ((n, 2), 3) = (SELECT s, 3 FROM t) FROM t WHERE false`,
		`SELECT (1, 3) IN (SELECT (n, 2), 3 FROM t) WHERE false`,
		`SELECT ((1, 2), 3) = ((SELECT 1), 3) WHERE false`,
		// Postgres fails IN (SELECT ...) of another width (42601).
		`SELECT (1, 2) IN (SELECT s FROM t) WHERE false`,
		`SELECT 1 IN (SELECT n, 2 FROM t) WHERE false`,
	} {
		if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	// So does a parameter among a subquery's columns, which Postgres
	// resolves to text too.
	for _, q := range []string{
		`SELECT (1, 2) = (SELECT $1, 2) WHERE false`,
		`SELECT (1, 2) IN (SELECT $1, 2) WHERE false`,
		`SELECT 1 IN (SELECT $1) WHERE false`,
	} {
		if _, err := db.Query(q, "1"); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	// Postgres resolves a set operation's or VALUES' column whatever a
	// parameter in it is bound to.
	for _, q := range []string{
		`SELECT (1, 2) IN (VALUES (1, $1)) WHERE false`,
		`SELECT (1, 2) IN (VALUES ('x', $1), ('y', 2)) WHERE false`,
	} {
		if _, err := db.Query(q, int64(2)); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	// A string literal among a row's fields is read as the type of its
	// pair's before any row (22P02).
	for _, q := range []string{
		`SELECT (1, 'x') = (n, 2) FROM t WHERE false`,
		`SELECT (1, 'x') = (SELECT 1, 2) WHERE false`,
		`SELECT (1, 'x') IN (SELECT n, n FROM t) WHERE false`,
	} {
		if _, err := db.Query(q); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%s: got %v, want 22P02", q, err)
		}
	}
}

// A * over a derived table, a CTE or a view gives the columns of the item
// in order, so Postgres types a subquery whose select list it is when it
// plans the statement, and fails text beside a number with 42883 whatever
// rows the statement reads.
func TestStarOverDerivedItemTypedBeforeAnyRow(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, s text, n int)`)
	mustExec(t, db, `CREATE VIEW sv AS SELECT s FROM t`)
	mustExec(t, db, `CREATE VIEW uv AS SELECT s FROM t UNION SELECT 'x'`)
	for _, q := range []string{
		`SELECT 1 = (SELECT * FROM (SELECT '1' AS v) x) WHERE false`,
		`SELECT 1 = (SELECT x.* FROM (SELECT '1' AS v) x) WHERE false`,
		`SELECT 1 = (SELECT * FROM (VALUES ('1')) v) WHERE false`,
		`SELECT 1 IN (SELECT * FROM (SELECT '1' AS v) x) WHERE false`,
		`SELECT 1 = (SELECT * FROM (SELECT s FROM t) x) WHERE false`,
		`SELECT 1 = (SELECT * FROM (SELECT s FROM t UNION SELECT 'x') u) WHERE false`,
		`SELECT 1 = (SELECT * FROM (SELECT * FROM (SELECT '1' AS v) x) y) WHERE false`,
		`SELECT 1 = (SELECT x.* FROM (SELECT '1' AS v) x JOIN t ON true) WHERE false`,
		`WITH c AS (SELECT '1' AS v) SELECT 1 = (SELECT * FROM c) WHERE false`,
		`SELECT 1 = (SELECT * FROM sv) WHERE false`,
		`SELECT 1 = (SELECT * FROM uv) WHERE false`,
		`SELECT (1, 2) = (SELECT * FROM (SELECT 1 AS a, '2' AS b) x) WHERE false`,
		`SELECT (1, 2) IN (SELECT x.* FROM (SELECT n, s FROM t) x) WHERE false`,
		`WITH c AS (SELECT * FROM (VALUES ('1')) a UNION ALL SELECT * FROM (VALUES ('2')) b) SELECT 1 IN (SELECT * FROM c) WHERE false`,
		`WITH c AS (SELECT * FROM sv UNION ALL SELECT * FROM sv) SELECT 1 = (SELECT * FROM c) WHERE false`,
	} {
		if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
}

// A set operation takes its column names from its first query, so where
// that is a * over a derived table, a CTE or a view, the * gives the names
// and types of the item's columns in order, and Postgres types the set
// operation read as a FROM item or a CTE when it plans the statement.
func TestSetOpOfStarOverDerivedItemTypedBeforeAnyRow(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, s text, n int)`)
	mustExec(t, db, `CREATE VIEW sv(w) AS SELECT * FROM (SELECT s FROM t) y`)
	for _, q := range []string{
		`SELECT abs(x.v) FROM (SELECT * FROM (SELECT 'a'::text AS v) y UNION ALL SELECT 'b'::text) x WHERE false`,
		`SELECT abs(x.v) FROM (SELECT * FROM (SELECT ROW(1, 2) AS v) y UNION ALL SELECT ROW(3, 4)) x WHERE false`,
		`SELECT abs(x.v) FROM (SELECT y.* FROM (SELECT 'a'::text AS v) y JOIN t ON true UNION ALL SELECT 'b'::text) x WHERE false`,
		`SELECT abs(x.w) FROM (SELECT * FROM (SELECT 'a'::text AS v) y(w) UNION ALL SELECT 'b'::text) x WHERE false`,
		`WITH c AS (SELECT 'a'::text AS v) SELECT abs(x.v) FROM (SELECT * FROM c UNION ALL SELECT 'b'::text) x WHERE false`,
		`WITH u AS (SELECT * FROM (SELECT 'a'::text AS v) y UNION ALL SELECT 'b'::text) SELECT abs(v) FROM u WHERE false`,
		`SELECT abs(x.w) FROM (SELECT * FROM sv UNION ALL SELECT 'b'::text) x WHERE false`,
	} {
		if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
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
		// Nor has name.
		`UPDATE t SET n = nm WHERE false`,
		`UPDATE t SET b = nm WHERE false`,
		`UPDATE t SET n = (SELECT nm FROM t LIMIT 1) WHERE false`,
		`INSERT INTO t (id, n) VALUES (20, '5'::name)`,
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
		`UPDATE t SET s = nm, v = nm, d = nm`,
		`INSERT INTO t (id, n) VALUES (12, '5'), (13, NULL)`,
	} {
		mustExec(t, db, q)
	}
	mustExec(t, db, `UPDATE t SET n = $1`, "6")
}

// Postgres has LIKE and ILIKE over the character types and bytea only, and
// fails another operand when it plans the statement, whatever rows it reads.
func TestLikeOperandTypesBeforeAnyRow(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TYPE mood AS ENUM ('a')`)
	mustExec(t, db, `CREATE DOMAIN word AS text`)
	mustExec(t, db, `CREATE DOMAIN num AS int`)
	mustExec(t, db, `CREATE DOMAIN positive AS int CHECK (VALUE > 0)`)
	mustExec(t, db, `CREATE TABLE p (k positive)`)
	mustExec(t, db, `CREATE TABLE t (n int, b bool, s text, v varchar(3), c char(3), nm name, w word, k num, m mood, j jsonb, by bytea, a text[])`)
	for _, q := range []string{
		`SELECT 1 FROM t WHERE n LIKE 'a'`,
		`SELECT 1 FROM t WHERE b LIKE 'a'`,
		`SELECT x.v LIKE 'a' FROM (SELECT ROW(1, 2) AS v) x WHERE false`,
		`SELECT 1 FROM t WHERE ROW(n, s) LIKE 'a'`,
		`SELECT 1 FROM t WHERE coalesce(ROW(n), NULL) LIKE 'a'`,
		`SELECT 1 FROM t WHERE n ILIKE 'a'`,
		`SELECT 1 FROM t WHERE n NOT LIKE 'a'`,
		`SELECT 1 FROM t WHERE n NOT ILIKE 'a'`,
		`SELECT 1 FROM t WHERE 'a' LIKE n`,
		`SELECT 1 FROM t WHERE s LIKE n`,
		`SELECT 1 LIKE 'a'`,
		`SELECT 1 FROM t WHERE k LIKE 'a'`,
		`SELECT 1 FROM t WHERE m LIKE 'a'`,
		`SELECT 1 FROM t WHERE j LIKE 'a'`,
		`SELECT 1 FROM t WHERE a LIKE 'a'`,
		// A domain whose base type detest hides may be one over a type
		// LIKE does not take.
		`SELECT 1 FROM p WHERE k LIKE 'a'`,
		// Postgres takes bytea beside another bytea only, and likeMatch
		// reads a value as text, so detest refuses it.
		`SELECT 1 FROM t WHERE by LIKE 'a'`,
	} {
		if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	for _, q := range []string{
		`SELECT 1 FROM t WHERE s LIKE 'a' AND v ILIKE 'a' AND c NOT LIKE 'a' AND nm NOT ILIKE 'a' AND w LIKE 'a'`,
		`SELECT 1 FROM t WHERE v LIKE s AND c ILIKE nm`,
		`SELECT 1 FROM t WHERE n::text LIKE 'a'`,
		`SELECT NULL LIKE NULL, 'a' LIKE NULL`,
	} {
		if _, err := db.Query(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	for _, q := range []string{
		`SELECT 1 FROM t WHERE $1 LIKE 'a'`,
		`SELECT 1 FROM t WHERE $1 = s AND $1 LIKE 'a'`,
		`SELECT 1 FROM t WHERE c ILIKE $1 AND $1 = v`,
	} {
		if _, err := db.Query(q, "a"); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	// LIKE types a parameter as text, which another use takes as another
	// type.
	for _, q := range []string{
		`SELECT 1 FROM t WHERE $1 = n AND $1 LIKE 'a'`,
		`SELECT 1 FROM t WHERE s NOT ILIKE $1 AND b = $1`,
	} {
		if _, err := db.Query(q, "1"); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	for _, q := range []string{
		`SELECT 1 LIKE 'a'`,
		`SELECT 'a' ILIKE true`,
		`SELECT ROW(1, 2) LIKE 'a'`,
		// A built-in type is known without a schema, as no domain may
		// take its name.
		`SELECT NULL::jsonb LIKE 'a' WHERE false`,
		`SELECT NULL::uuid ILIKE 'a' WHERE false`,
		`SELECT NULL::date NOT LIKE 'a' WHERE false`,
		`SELECT NULL::int4 NOT ILIKE 'a' WHERE false`,
		`SELECT NULL::pg_catalog._int4 LIKE 'a' WHERE false`,
		`SELECT NULL::pg_lsn LIKE 'a' WHERE false`,
		`SELECT NULL::pg_snapshot LIKE 'a' WHERE false`,
		// Postgres takes this, as it reads the literal as a bytea, but
		// detest refuses bytea.
		`SELECT 'a' LIKE NULL::bytea WHERE false`,
	} {
		if err := CheckSQL(postgres.New(), q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("CheckSQL %s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	// Without a schema a type CheckSQL does not know may be a domain over
	// text.
	if err := CheckSQL(postgres.New(), `SELECT 'a'::word LIKE 'a'`); err != nil {
		t.Errorf("CheckSQL 'a'::word LIKE 'a': %v", err)
	}
	// An alias such as integer names no array, so _integer may be a domain.
	// Nor does a pseudo-type such as trigger have one.
	mustExec(t, db, `CREATE DOMAIN _integer AS text`)
	mustExec(t, db, `CREATE DOMAIN _trigger AS text`)
	for _, q := range []string{
		`SELECT NULL::_integer LIKE 'a'`,
		`SELECT NULL::_trigger LIKE 'a'`,
	} {
		if _, err := db.Query(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
		if err := CheckSQL(postgres.New(), q); err != nil {
			t.Errorf("CheckSQL %s: %v", q, err)
		}
	}
}
