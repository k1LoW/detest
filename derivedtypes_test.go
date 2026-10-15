package detest

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/k1LoW/detest/db/postgres"
)

// A column of a *, a set operation or generate_series in FROM has the type
// Postgres gives it, so a literal beside it runs where Postgres runs it.
func TestDerivedColumnTypes(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text, n int)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'a', 5), (2, NULL, NULL)`)
	mustExec(t, db, `CREATE VIEW tv AS SELECT id, name FROM t UNION ALL SELECT 3, NULL`)
	mustExec(t, db, `CREATE TABLE f (id int PRIMARY KEY, flag boolean, u uuid)`)
	mustExec(t, db, `INSERT INTO f VALUES (1, NULL, NULL)`)
	mustExec(t, db, `CREATE DOMAIN span AS interval`)
	mustExec(t, db, `CREATE DOMAIN amount AS numeric`)
	mustExec(t, db, `CREATE DOMAIN wall AS timestamp`)
	mustExec(t, db, `CREATE SEQUENCE seq`)
	mustExec(t, db, `CREATE TABLE ch (id int PRIMARY KEY, c char(3))`)
	mustExec(t, db, `CREATE TABLE t2 (a int, b int)`)
	mustExec(t, db, `CREATE TABLE big (n bigint)`)
	mustExec(t, db, `CREATE TABLE nm (m numeric)`)
	mustExec(t, db, `CREATE VIEW seqv AS SELECT nextval('seq') AS n`)
	mustExec(t, db, `CREATE VIEW seqw AS SELECT n FROM seqv`)
	mustExec(t, db, `CREATE TABLE b (id int PRIMARY KEY, payload bytea)`)
	mustExec(t, db, `CREATE TABLE d (id int PRIMARY KEY, d date)`)
	mustExec(t, db, `INSERT INTO d VALUES (1, NULL)`)
	mustExec(t, db, `CREATE TABLE ts (id int PRIMARY KEY, ts timestamp)`)
	mustExec(t, db, `INSERT INTO ts VALUES (1, '2024-01-01 00:00')`)
	for _, tt := range []struct {
		q    string
		want []string
	}{
		{`SELECT coalesce(s.name, 'x') FROM (SELECT * FROM t) s ORDER BY s.id`, []string{"a", "x"}},
		{`SELECT coalesce(s.name, 'x') FROM (SELECT t.* FROM t) s ORDER BY s.id`, []string{"a", "x"}},
		{`WITH c AS (SELECT * FROM t) SELECT coalesce(c.name, 'x') FROM c ORDER BY c.id`, []string{"a", "x"}},
		{`SELECT coalesce(name, 'x') FROM tv ORDER BY id`, []string{"a", "x", "x"}},
		{`SELECT CASE WHEN s.id = 1 THEN s.name ELSE 'x' END FROM (SELECT * FROM t) s ORDER BY s.id`, []string{"a", "x"}},
		{`SELECT greatest(s.n, 1) FROM (SELECT * FROM t) s ORDER BY s.id`, []string{"5", "1"}},
		{`SELECT coalesce(u.name, 'x') FROM (SELECT name FROM t UNION SELECT name FROM t) u ORDER BY 1`, []string{"a", "x"}},
		{`SELECT coalesce(u.name, 'x') FROM (SELECT name FROM t UNION ALL SELECT NULL) u ORDER BY 1`, []string{"a", "x", "x"}},
		{`SELECT coalesce(u.n, 0) FROM (SELECT NULL AS n UNION ALL (SELECT NULL UNION ALL SELECT n FROM t)) u ORDER BY 1`, []string{"0", "0", "0", "5"}},
		{`SELECT coalesce(u.name, 'x') FROM (SELECT 'b' AS name UNION ALL SELECT name FROM t) u ORDER BY 1`, []string{"a", "b", "x"}},
		{`SELECT coalesce(g, 0) FROM generate_series(1, 2) AS g ORDER BY 1`, []string{"1", "2"}},
		{`SELECT coalesce(k, 0) FROM generate_series(1, 2) AS g(k) ORDER BY 1`, []string{"1", "2"}},
		{`SELECT * FROM generate_series(1, 2) g ORDER BY 1 DESC`, []string{"2", "1"}},
	} {
		if got := rowsOf(t, db, tt.q); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
	// A * gives its items' columns, so a name none of them has is no
	// column, and one two of them have is refused, as of a select list
	// that names two columns alike.
	if _, err := db.Query(`SELECT s.nope FROM (SELECT * FROM t) s`); !errors.Is(err, ErrUndefinedColumn) {
		t.Errorf("a column a * does not give: got %v", err)
	}
	if _, err := db.Query(`SELECT s.id FROM (SELECT * FROM t a JOIN t b ON a.id = b.id) s`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a column a * gives twice: got %v", err)
	}
	// Queries of a set operation that disagree, or one of a type the check
	// does not know, leave the column unresolved, as it may be an interval.
	// Postgres resolves a pair of NULLs, or of CASE or COALESCE over them,
	// to text, which an integer does not join, and converts a string
	// literal beside an interval, which detest does not.
	for _, q := range []string{
		`SELECT coalesce(u.v, 0) FROM (SELECT NULL AS v UNION ALL SELECT NULL UNION ALL SELECT 1) u`,
		`SELECT coalesce(u.v, 0) FROM (SELECT coalesce(NULL, NULL) AS v UNION ALL SELECT 1) u`,
		`SELECT coalesce(u.v, interval '0 days') FROM (SELECT NULL::interval AS v UNION ALL SELECT 'bogus') u`,
		`SELECT coalesce(u.v, 0) FROM (SELECT n AS v FROM t UNION ALL SELECT '5') u`,
		// A text column beside a number fails in Postgres whatever the
		// rows.
		`SELECT coalesce(u.v, 0) FROM (SELECT NULL AS v UNION ALL SELECT NULL) u`,
		`SELECT CASE WHEN false THEN s.name ELSE s.n END FROM (SELECT * FROM t) s`,
		`SELECT CASE WHEN false THEN coalesce('x', 'y') ELSE s.n END FROM (SELECT * FROM t) s`,
		`SELECT coalesce(g, 0) FROM generate_series($1, 2) AS g`,
		// detest converts branches to no boolean, uuid or interval, nor
		// a domain over one, and Postgres fails branches of a uuid and
		// text with 42804.
		`SELECT s.id FROM (SELECT * FROM f) s WHERE coalesce(s.flag, 'true')`,
		`SELECT id FROM f WHERE coalesce(flag, $1)`,
		`SELECT CASE WHEN true THEN s.u ELSE 'x'::text END FROM (SELECT * FROM f) s`,
		`SELECT CASE WHEN true THEN u ELSE 'x'::text END FROM f`,
		// A CASE or COALESCE of literals only is text, beside a bytea.
		`SELECT coalesce(s.payload, coalesce('x', 'y')) FROM (SELECT * FROM b) s`,
		`SELECT CASE WHEN false THEN coalesce('x', 'y') ELSE s.payload END FROM (SELECT * FROM b) s`,
		// Postgres fails the branches' types before it reads the literal.
		`SELECT coalesce(s.payload, '\xZZ', 'x'::text) FROM (SELECT * FROM b) s`,
		// functionRows runs the integer overload only.
		`SELECT *, g / 2 AS half FROM generate_series(1.0, 2.0) g ORDER BY 1`,
		// Postgres fails it as ambiguous, with 42725.
		`SELECT * FROM generate_series(NULL, NULL) g ORDER BY 1`,
		`SELECT * FROM generate_series(coalesce('1', '2'), 2) g ORDER BY 1`,
		`SELECT * FROM generate_series(NULL::int, 'bogus') g ORDER BY 1`,
		// COLLATE makes a literal text.
		`SELECT coalesce(s.n, 'bogus' COLLATE "C", 0) FROM (SELECT * FROM t) s`,
		`SELECT coalesce(s.flag, 'true' COLLATE "C") FROM (SELECT * FROM f) s`,
		// The time readers take the type's own name, not a domain's base.
		`SELECT coalesce(s.ts, NULL::timestamptz) FROM (SELECT t.* FROM (SELECT '2024-05-20 00:30'::wall AS ts) t) s`,
		// Postgres fails a numeric offset with 42883.
		`SELECT lag(s.n, s.m, 0) OVER (ORDER BY s.id) FROM (SELECT t.*, 1.5 AS m FROM t) s`,
		`SELECT lead(s.n, s.m, 0) OVER (ORDER BY s.id) FROM (SELECT t.*, 1.5 AS m FROM t) s`,
		`SELECT lag(s.n, 'bogus', 0) OVER (ORDER BY s.id) FROM (SELECT * FROM t) s`,
		`SELECT lag(s.n, coalesce('bogus', 'x'), 0) OVER () FROM (SELECT * FROM t) s`,
		`SELECT lead(s.n, coalesce('bogus', 'x'), 0) OVER () FROM (SELECT * FROM t) s`,
		// The evaluators would run the fallback, which Postgres skips.
		`SELECT coalesce(s.id, nextval('seq')) FROM (SELECT * FROM t) s`,
		`SELECT coalesce(s.n, (SELECT n FROM t WHERE id = 2 FOR UPDATE)) FROM (SELECT * FROM t WHERE id = 1) s`,
		`SELECT coalesce(s.n, NULL COLLATE "C", 0) FROM (SELECT * FROM t) s`,
		`SELECT coalesce(s.n, 1 COLLATE "C") FROM (SELECT * FROM t) s`,
		`SELECT coalesce(s.n, abs(NULL)) / 2 FROM (SELECT * FROM t) s`,
		`SELECT lag(s.n, abs(NULL), 0) OVER () FROM (SELECT * FROM t) s`,
		`SELECT lead(s.n, abs(NULL), 0) OVER () FROM (SELECT * FROM t) s`,
		`SELECT coalesce(s.name, abs(NULL), 'x') FROM (SELECT * FROM t) s`,
		`SELECT * FROM generate_series(abs(NULL), 2) g ORDER BY 1`,
		// The text conversion does not give a char's trailing-space
		// semantics to a literal beside it.
		`SELECT coalesce(s.c, 'a ') = 'a' FROM (SELECT * FROM ch) s`,
		`SELECT coalesce(s.c, 'a '::bpchar) = 'a'::bpchar FROM (SELECT * FROM ch) s`,
		// evalSetOp does not convert the integer side to a numeric.
		`SELECT coalesce(u.v / 2, 0) FROM (SELECT 1 AS v UNION ALL SELECT m FROM nm) u`,
		// A view's sequence call is an effect too.
		`SELECT coalesce(s.id, (SELECT n FROM seqv)) FROM (SELECT * FROM t) s`,
		`WITH c AS (SELECT nextval('seq') AS n) SELECT coalesce(s.id, (SELECT n FROM c)) FROM (SELECT * FROM t) s`,
		// A CTE of a view's name does not hide the view from a CTE
		// declared before it, nor from another view.
		`WITH c AS (SELECT n FROM seqv), seqv AS (SELECT 0 AS n) SELECT coalesce(s.id, (SELECT n FROM c)) FROM (SELECT * FROM t) s`,
		`WITH seqv AS (SELECT 0 AS n) SELECT coalesce(s.id, (SELECT n FROM seqw)) FROM (SELECT * FROM t) s`,
		`WITH c AS (SELECT n FROM seqv) SELECT (WITH seqv AS (SELECT 0 AS n) SELECT coalesce(s.id, (SELECT n FROM c)) FROM (SELECT * FROM t) s LIMIT 1)`,
		`SELECT lead(s.n, 1::bigint, 0) OVER (ORDER BY s.id) FROM (SELECT * FROM t) s`,
		`SELECT *, g / 2 AS half FROM generate_series((SELECT 1.0 UNION SELECT 1.0), 2) g ORDER BY 1`,
		// The NULL of a scalar subquery is text, beside an integer.
		`SELECT coalesce(s.n, (SELECT NULL), 0) FROM (SELECT * FROM t) s`,
		`SELECT s.id FROM t CROSS JOIN LATERAL (SELECT t.*) s`,
		// Postgres fails a date that is none whichever branch a row
		// takes, or with no rows.
		`SELECT coalesce(s.d, '2025-02-30') FROM (SELECT * FROM d) s WHERE false`,
		`SELECT CASE WHEN false THEN '2025-02-30' ELSE s.d END FROM (SELECT * FROM d) s`,
		// A LAG of literals only is text, beside an integer.
		`SELECT coalesce(s.n, lag('x', 1, 'y') OVER (), 0) FROM (SELECT * FROM t) s`,
	} {
		if _, err := db.Query(q, 1); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	// Postgres reads the literal as the column's type before any row, and
	// fails with the type's input error.
	for _, q := range []string{
		`SELECT coalesce(u.v, 'x') FROM (SELECT id AS v FROM t UNION SELECT n::int8 FROM t) u`,
		`SELECT coalesce(s.u, 'bogus') FROM (SELECT * FROM f) s`,
		`SELECT coalesce(u.v, 'bogus') FROM (SELECT NULL::span AS v UNION ALL SELECT NULL::span) u`,
		`SELECT coalesce(u.v, 'bogus') FROM (SELECT NULL::amount AS v UNION ALL SELECT NULL::amount) u`,
	} {
		_, err := db.Query(q)
		if err == nil || errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want the type's input error", q, err)
		}
	}
	// Postgres binds a parameter among number branches as that number,
	// whichever branch a row takes.
	for _, q := range []string{
		`SELECT CASE WHEN true THEN s.n ELSE $1 END FROM (SELECT * FROM t) s`,
		`SELECT coalesce(n, $1) FROM t WHERE false`,
	} {
		if _, err := db.Query(q, "bogus"); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%s with bogus: got %v, want 22P02", q, err)
		}
	}
	// A parameter Postgres gives a number type is not also text, nor a
	// value the driver sends as other than a number.
	for _, tt := range []struct {
		q   string
		arg any
	}{
		{`SELECT coalesce(s.n, $1), coalesce(s.name, $1) FROM (SELECT * FROM t) s`, "5"},
		{`SELECT coalesce(n, $1) FROM t WHERE $1 = '8'::text`, "8"},
		{`SELECT coalesce($1, 'x'), coalesce(s.n, $1) FROM (SELECT * FROM t) s`, "5"},
		// Postgres binds it as the number, which another use reads.
		{`SELECT coalesce(s.n, $1) FROM (SELECT * FROM t) s WHERE $1::text = '5'`, "05"},
		{`SELECT coalesce(s.n, $1), $1 / 2 FROM (SELECT * FROM t) s`, int64(1)},
		// The driver sends it as text, which Postgres reads as the
		// branches' type.
		{`SELECT coalesce(s.name, $1) = '01' FROM (SELECT * FROM t) s`, int64(1)},
		{`SELECT coalesce(s.d, $1) FROM (SELECT * FROM d) s WHERE false`, true},
		{`SELECT coalesce(s.name, lag($1) OVER (ORDER BY s.id), 'x') = '01' FROM (SELECT * FROM t) s ORDER BY s.id`, int64(1)},
		{`SELECT coalesce(s.name, $1 COLLATE "C", 'x') = '01' FROM (SELECT * FROM t) s`, int64(1)},
		{`SELECT coalesce(s.name, (SELECT $1)) = '01' FROM (SELECT * FROM t) s`, int64(1)},
		{`SELECT coalesce(s.name, (SELECT $1 UNION ALL SELECT NULL LIMIT 1)) = '01' FROM (SELECT * FROM t) s`, int64(1)},
		{`SELECT CASE WHEN true THEN (SELECT $1) ELSE s.name END = '\x41' FROM (SELECT * FROM t) s`, []byte("A")},
		{`SELECT coalesce(s.v, 'x') = '01' FROM (SELECT t.*, $1 COLLATE "C" AS v FROM t) s`, int64(1)},
		{`SELECT coalesce((SELECT $1 COLLATE "C"), 'x') = '01' FROM t`, int64(1)},
		{`SELECT coalesce(s.name, $1 COLLATE "C") FROM (SELECT * FROM t) s WHERE s.n = $1`, "5"},
		// generate_series and a LAG offset type the parameter as int4.
		{`SELECT * FROM generate_series(1, $1) g WHERE $1 = '1'::text ORDER BY 1`, int64(1)},
		{`SELECT lag(s.n, $1, 0) OVER (ORDER BY s.id) FROM (SELECT * FROM t) s WHERE $1 = '1'::text`, int64(1)},
		{`SELECT lead(s.n, $1, 0) OVER (ORDER BY s.id) FROM (SELECT * FROM t) s WHERE $1 = '1'::text`, int64(1)},
		{`SELECT * FROM generate_series(NULL::int, $1) g ORDER BY 1`, true},
		{`SELECT * FROM generate_series(NULL::int, $1) g ORDER BY 1`, 1.5},
		{`SELECT coalesce(s.d, $1) FROM (SELECT * FROM d) s WHERE false`, []byte("bogus")},
		{`SELECT coalesce(u.v, 'x') = '01' FROM (SELECT NULL AS v UNION ALL SELECT $1) u`, int64(1)},
		{`SELECT coalesce(s.n, $1) FROM (SELECT * FROM t) s`, true},
		{`SELECT coalesce(n, $1) FROM t`, 1.5},
	} {
		if _, err := db.Query(tt.q, tt.arg); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s with %v: got %v, want ErrUnsupportedSQL", tt.q, tt.arg, err)
		}
	}
	// generate_series and a LAG offset type a parameter as an integer,
	// which an integer comparison of it agrees with.
	if got := rowsOf(t, db, `SELECT g FROM generate_series(1, $1) g WHERE $1 = 2 ORDER BY 1`, int64(2)); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("a series bound parameter compared with an integer: got %v", got)
	}
	if _, err := db.Query(`SELECT lag(n, $1, 0) OVER () FROM t WHERE $1 = 1`, int64(1)); err != nil {
		t.Errorf("a LAG offset parameter compared with an integer: %v", err)
	}
	// The refusal leaves the sequence where it was.
	if got := rowsOf(t, db, `SELECT nextval('seq')`); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("nextval after a refused COALESCE: got %v", got)
	}
	var at time.Time
	// NULLIF of a string literal and a date is a date.
	if err := db.QueryRow(`SELECT nullif('2025-01-02', s.d) FROM (SELECT * FROM d) s`).Scan(&at); err != nil || at.Format("2006-01-02") != "2025-01-02" {
		t.Errorf("NULLIF of a literal and a NULL date: got %v, %v", at, err)
	}
	// A time parameter beside a timestamp is its own clock, as Postgres
	// types it, in LAG's default as in COALESCE.
	if err := db.QueryRow(`SELECT lag(s.ts, 1, $1) OVER (ORDER BY s.id) FROM (SELECT * FROM ts) s`, time.Date(2024, 5, 20, 0, 30, 0, 0, time.FixedZone("", 9*60*60))).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if at.Hour() != 0 || at.Minute() != 30 {
		t.Errorf("a time parameter as LAG's default beside a timestamp: got %v", at)
	}
	// A * in a set operation gives more columns than targets.
	if got := rowsOf(t, db, `SELECT u.b FROM (SELECT 1 AS a, 2 AS b UNION ALL SELECT * FROM t2) u`); !reflect.DeepEqual(got, []string{"2"}) {
		t.Errorf("a set operation with a *: got %v", got)
	}
	// generate_series takes a scalar subquery's type in its own scope, a
	// bigint here beside an outer integer column of the same name.
	if got := rowsOf(t, db, `SELECT coalesce(g, '3000000000') FROM t CROSS JOIN generate_series((SELECT n FROM big WHERE false), 1) g`); len(got) != 0 {
		t.Errorf("generate_series over a bigint subquery: got %v", got)
	}
	if got := rowsOf(t, db, `SELECT s.id FROM (SELECT * FROM f) s WHERE coalesce(s.flag, true)`); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("a boolean beside a boolean column: got %v", got)
	}
	if got := rowsOf(t, db, `SELECT coalesce(n, $1) / 2 FROM t ORDER BY id`, "8"); !reflect.DeepEqual(got, []string{"2", "4"}) {
		t.Errorf("a parameter's text beside an integer column: got %v", got)
	}
}

// NULLIF of a date and a timestamp is a date, which date_trunc reads at
// midnight in the session's TimeZone.
func TestDerivedNullifDate(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.TimeZone("Asia/Tokyo")))
	mustExec(t, db, `CREATE TABLE d (id int PRIMARY KEY, d date)`)
	mustExec(t, db, `INSERT INTO d VALUES (1, '2025-01-02')`)
	for _, q := range []string{
		`SELECT date_trunc('day', nullif(s.d, NULL::timestamp)) FROM (SELECT * FROM d) s`,
		// A scalar subquery is typed in its own scope.
		`SELECT date_trunc('day', (SELECT coalesce(s.d, '2025-01-02') FROM (SELECT * FROM d) s))`,
	} {
		var at time.Time
		if err := db.QueryRow(q).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if want := time.Date(2025, 1, 1, 15, 0, 0, 0, time.UTC); !at.Equal(want) {
			t.Errorf("%s: got %v, want %v", q, at, want)
		}
	}
}
