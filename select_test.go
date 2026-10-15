package detest

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/k1LoW/detest/db/postgres"
)

// rowsOf runs q and returns its rows as strings, columns joined by ",".
func rowsOf(t *testing.T, db *sql.DB, q string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("%s: scan: %v", q, err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			if v == nil {
				parts[i] = "NULL"
			} else {
				parts[i] = fmt.Sprint(v)
			}
		}
		out = append(out, strings.Join(parts, ","))
	}
	return out
}

func TestSelectPipeline(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE releases (id text PRIMARY KEY, name text, version int, created int)`)
	for _, r := range []struct {
		id, name string
		version  any
		created  int
	}{
		{"r1", "api", 1, 10}, {"r2", "api", 3, 30}, {"r3", "api", 2, 20},
		{"r4", "web", 1, 15}, {"r5", "web", 2, 25}, {"r6", "cli", nil, 5},
	} {
		mustExec(t, db, `INSERT INTO releases (id, name, version, created) VALUES ($1, $2, $3, $4)`, r.id, r.name, r.version, r.created)
	}
	for _, tc := range []struct {
		q    string
		want []string
	}{
		// DISTINCT ON keeps the first row of each key in ORDER BY's order.
		{`SELECT DISTINCT ON (name) name, version FROM releases ORDER BY name, version DESC NULLS LAST`,
			[]string{"api,3", "cli,NULL", "web,2"}},
		// DISTINCT applies before LIMIT.
		{`SELECT DISTINCT name FROM releases ORDER BY name LIMIT 2`, []string{"api", "cli"}},
		// GROUP BY honors ORDER BY by position, OFFSET and LIMIT.
		{`SELECT name, count(*) FROM releases GROUP BY name ORDER BY 2 DESC, 1 OFFSET 1 LIMIT 1`, []string{"web,2"}},
		// NULLs sort last ascending, first descending, unless NULLS says otherwise.
		{`SELECT id FROM releases ORDER BY version, id LIMIT 2`, []string{"r1", "r4"}},
		{`SELECT id FROM releases ORDER BY version DESC, id LIMIT 2`, []string{"r6", "r2"}},
		{`SELECT id FROM releases ORDER BY version NULLS FIRST, id LIMIT 1`, []string{"r6"}},
		// Window functions over the whole partition, as a named window.
		{`SELECT DISTINCT ON (name) name, MAX(version) OVER w AS latest, MIN(created) OVER w AS first FROM releases
		  WINDOW w AS (PARTITION BY name ORDER BY version ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)
		  ORDER BY name`, []string{"api,3,10", "cli,NULL,5", "web,2,15"}},
		// Ranking and the default frame, which runs to the current row's peers.
		{`SELECT id, row_number() OVER (PARTITION BY name ORDER BY created), sum(created) OVER (PARTITION BY name ORDER BY created)
		  FROM releases WHERE name = 'api' ORDER BY created`, []string{"r1,1,10", "r3,2,30", "r2,3,60"}},
		{`SELECT id, lag(id) OVER (ORDER BY created), lead(id, 1, 'none') OVER (ORDER BY created) FROM releases WHERE name = 'web' ORDER BY id`,
			[]string{"r4,NULL,r5", "r5,r4,none"}},
		// Set operations combine by position and remove duplicates unless ALL.
		{`SELECT name FROM releases WHERE version = 1 UNION SELECT name FROM releases WHERE created < 20 ORDER BY 1`, []string{"api", "cli", "web"}},
		{`SELECT name FROM releases WHERE version = 1 UNION ALL SELECT name FROM releases WHERE created < 12 ORDER BY 1`, []string{"api", "api", "cli", "web"}},
		{`SELECT name FROM releases INTERSECT SELECT name FROM releases WHERE version >= 2 ORDER BY name`, []string{"api", "web"}},
		{`SELECT name FROM releases EXCEPT SELECT name FROM releases WHERE version >= 2`, []string{"cli"}},
		{`SELECT count(*) FROM (SELECT id FROM releases WHERE name = 'api' UNION ALL SELECT id FROM releases WHERE name = 'web') AS u`, []string{"5"}},
		// VALUES as a query, renamed by the alias.
		{`SELECT v.n, v.label FROM (VALUES (1, 'one'), (2, 'two')) AS v(n, label) ORDER BY v.n DESC`, []string{"2,two", "1,one"}},
		// A set-returning function in FROM; a NULL bound yields nothing.
		{`SELECT gs FROM generate_series(1, 3) AS gs`, []string{"1", "2", "3"}},
		{`SELECT count(*) FROM generate_series(1, NULL) AS gs`, []string{"0"}},
		// sum over no rows is NULL.
		{`SELECT sum(version) FROM releases WHERE name = 'none'`, []string{"NULL"}},
	} {
		if got := rowsOf(t, db, tc.q); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s\n got %v\nwant %v", strings.Join(strings.Fields(tc.q), " "), got, tc.want)
		}
	}
}

func TestFromFunctionRefusedBeforeItsArguments(t *testing.T) {
	for _, q := range []string{
		`SELECT * FROM unnest(nextval('s'))`,
		`SELECT * FROM generate_series(nextval('s'))`,
		`SELECT * FROM generate_series(nextval('s'), 2, 1, 1)`,
	} {
		t.Run(q, func(t *testing.T) {
			s := newSim(t)
			db, _ := s.DB("app", postgres.New())
			mustExec(t, db, `CREATE SEQUENCE s`)
			if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
				t.Fatalf("got %v, want ErrUnsupportedSQL", err)
			}
			var n int64
			if err := db.QueryRow(`SELECT nextval('s')`).Scan(&n); err != nil || n != 1 {
				t.Errorf("nextval after the refused call: %d, %v; want 1", n, err)
			}
		})
	}
}

func TestPostgresExpressionTypesRefusedBeforeEvaluation(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE SEQUENCE s`)
	mustExec(t, db, `CREATE TABLE typed (id int PRIMARY KEY, j jsonb, a text[], name text)`)
	mustExec(t, db, `CREATE VIEW typed_view AS SELECT * FROM typed`)
	for _, q := range []string{
		`SELECT '{}'::jsonb || '{}'::jsonb`,
		`SELECT '{}'::text[] || '{}'::text[]`,
		`SELECT j || '{}' FROM typed`,
		`SELECT a || '{}' FROM typed`,
		`SELECT t.j || '{}' FROM typed t`,
		`SELECT d.j || '{}' FROM (SELECT j FROM typed) d`,
		`SELECT j || '{}' FROM typed_view`,
		`WITH d AS (SELECT j FROM typed) SELECT j || '{}' FROM d`,
		`UPDATE typed SET j = j || '{}'`,
		`SELECT j || nextval('s') FROM typed`,
		`SELECT lower(1)`,
		`SELECT lower(nextval('s'))`,
		`SELECT lower(id) FROM typed`,
		`SELECT lower(max(id)) FROM typed`,
		`SELECT lower(round(1))`,
		`SELECT lower(pg_try_advisory_xact_lock(1))`,
		`SELECT left(name, 1.5) FROM typed`,
		`SELECT left('abc', nextval('s'))`,
		`SELECT lower(row_number() OVER ())`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	var n int64
	if err := db.QueryRow(`SELECT nextval('s')`).Scan(&n); err != nil || n != 1 {
		t.Errorf("nextval after refused expressions: %d, %v; want 1", n, err)
	}
	mustExec(t, db, `INSERT INTO typed (id, name) VALUES (1, 'A')`)
	if got := rowsOf(t, db, `SELECT lower(name), name || id FROM typed`); !reflect.DeepEqual(got, []string{"a,A1"}) {
		t.Errorf("supported text expressions: %v", got)
	}
}

func TestSavepoint(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE items (id text PRIMARY KEY, n int)`)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string) error {
		_, err := tx.Exec(q)
		return err
	}
	for _, q := range []string{
		`INSERT INTO items (id, n) VALUES ('a', 1)`,
		`SAVEPOINT sp1`,
		`INSERT INTO items (id, n) VALUES ('b', 2)`,
	} {
		if err := exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// A failing statement aborts the transaction until ROLLBACK TO.
	if err := exec(`INSERT INTO items (id, n) VALUES ('a', 9)`); err == nil {
		t.Fatal("duplicate insert must fail")
	}
	if err := exec(`INSERT INTO items (id, n) VALUES ('c', 3)`); err == nil {
		t.Fatal("an aborted transaction must refuse statements")
	}
	if err := exec(`ROLLBACK TO SAVEPOINT sp1`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO items (id, n) VALUES ('d', 4)`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`RELEASE SAVEPOINT sp1`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range store.Peek("items") {
		ids = append(ids, r.Str("id"))
	}
	if strings.Join(ids, ",") != "a,d" {
		t.Fatalf("rows after the savepoint was rolled back to: %v", ids)
	}
	if _, err := db.Exec(`SAVEPOINT outside`); err == nil {
		t.Fatal("SAVEPOINT outside a transaction must fail")
	}
}

func TestSequences(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE orders (id serial PRIMARY KEY, note text)`)
	if got := rowsOf(t, db, `SELECT setval('"public"."orders_id_seq"', 41)`); got[0] != "41" {
		t.Fatalf("setval: %v", got)
	}
	if got := rowsOf(t, db, `INSERT INTO orders (note) VALUES ('x') RETURNING id`); got[0] != "42" {
		t.Fatalf("nextval after setval: %v", got)
	}
	rowsOf(t, db, `SELECT setval('orders_id_seq', 100, false)`)
	if got := rowsOf(t, db, `SELECT nextval('public.orders_id_seq')`); got[0] != "100" {
		t.Fatalf("nextval after setval(..., false): %v", got)
	}
}

// With lock_timeout set, a lock wait can end in 55P03 as well as succeed,
// and the explorer tries both.
func TestLockTimeout(t *testing.T) {
	var outcomes []string
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE counters (id text PRIMARY KEY, n int)`)
		s.Seed(func() {
			outcomes = outcomes[:0]
			mustExec(t, db, `INSERT INTO counters (id, n) VALUES ('c', 0)`)
		})
		bump := func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`SET LOCAL lock_timeout = '1s'`); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE counters SET n = n + 1 WHERE id = 'c'`); err != nil {
				if errors.Is(err, ErrLockNotAvailable) {
					outcomes = append(outcomes, "timeout")
					return nil
				}
				return err
			}
			p.Step("holds the row lock")
			outcomes = append(outcomes, "ok")
			return tx.Commit()
		}
		s.Manual("a", 1, bump)
		s.Manual("b", 1, bump)
		s.AtQuiescence(func(st *State) error {
			if strings.Contains(strings.Join(outcomes, ","), "timeout") {
				return fmt.Errorf("a lock wait timed out: %v", outcomes)
			}
			return nil
		})
		s.ExpectViolation("a lock wait timed out")
	})
}

// An aggregate under an expression the grouped evaluation does not take
// apart is refused rather than evaluated as a function of one row.
func TestAggregateUnderUnsupportedExpression(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY)`)
	mustExec(t, db, `INSERT INTO t VALUES (1)`)
	for _, q := range []string{
		`SELECT count(*) IS NULL FROM t`,
		`SELECT NOT count(*) > 0 FROM t`,
		`SELECT CASE WHEN count(*) > 0 THEN 1 END FROM t`,
		`SELECT count(*) IN (1, 2) FROM t`,
		`SELECT string_agg(id::text, ',') FROM t`,
		`SELECT json_agg_strict(id) FROM t`,
		`SELECT 1 FROM t ORDER BY string_agg(id::text, ',')`,
		`SELECT id FROM t LIMIT -1`,
		`SELECT id FROM t OFFSET -1`,
		`SELECT id FROM t LIMIT -1 FOR UPDATE`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v", q, err)
		}
	}
	var n int64
	if err := db.QueryRow(`SELECT count(*) + 1 FROM t`).Scan(&n); err != nil || n != 2 {
		t.Errorf("count(*) + 1: %d, %v", n, err)
	}
	// An aggregate only in ORDER BY is refused: Postgres makes the query one
	// group and checks the select list against it, which detest does not.
	for _, q := range []string{`SELECT 1 FROM t ORDER BY count(*)`, `SELECT id FROM t ORDER BY count(*)`} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v", q, err)
		}
	}
	// ORDER BY an aggregate the select list has too is fine.
	if err := db.QueryRow(`SELECT count(*) FROM t ORDER BY count(*)`).Scan(&n); err != nil || n != 1 {
		t.Errorf("count(*) ORDER BY count(*): %d, %v", n, err)
	}
}

// CURRENT_TIMESTAMP(p) and LOCALTIMESTAMP(p) round to p fractional digits.
func TestTimestampPrecision(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	for q, unit := range map[string]int{
		`SELECT CURRENT_TIMESTAMP(0)`: 1e9,
		`SELECT LOCALTIMESTAMP(3)`:    1e6,
		`SELECT CURRENT_TIMESTAMP(7)`: 1e3, // reduced to 6, as Postgres does
	} {
		var ts time.Time
		if err := db.QueryRow(q).Scan(&ts); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if ts.Nanosecond()%unit != 0 {
			t.Errorf("%s: %v has more digits", q, ts)
		}
	}
	// The other time functions take no argument.
	for _, q := range []string{`SELECT now(3)`, `SELECT clock_timestamp(3)`} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v", q, err)
		}
	}
}

// now() and CURRENT_TIMESTAMP are the time the transaction began, the same
// throughout it, while clock_timestamp() moves on.
func TestTransactionTimestamp(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	at := func(q string) time.Time {
		t.Helper()
		var ts time.Time
		if err := tx.QueryRow(q).Scan(&ts); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return ts
	}
	first, clock := at(`SELECT now()`), at(`SELECT clock_timestamp()`)
	if first.Nanosecond()%1000 != 0 || clock.Nanosecond()%1000 != 0 {
		t.Errorf("more than microsecond precision: %v, %v", first, clock)
	}
	time.Sleep(2 * time.Millisecond)
	for _, q := range []string{`SELECT now()`, `SELECT CURRENT_TIMESTAMP`, `SELECT transaction_timestamp()`} {
		if got := at(q); !got.Equal(first) {
			t.Errorf("%s: %v, want the transaction's start %v", q, got, first)
		}
	}
	if !at(`SELECT clock_timestamp()`).After(clock) {
		t.Error("clock_timestamp() did not move on")
	}
	var same bool
	if err := tx.QueryRow(`SELECT CURRENT_TIMESTAMP(6) = CURRENT_TIMESTAMP(6)`).Scan(&same); err != nil || !same {
		t.Errorf("CURRENT_TIMESTAMP(6) twice: %v %v", same, err)
	}
	// A statement outside a transaction begins its own at the same instant.
	if err := db.QueryRow(`SELECT now() = statement_timestamp()`).Scan(&same); err != nil || !same {
		t.Errorf("now() = statement_timestamp() in autocommit: %v %v", same, err)
	}
}

// OFFSET is evaluated before LIMIT, as Postgres does.
func TestOffsetBeforeLimit(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY)`)
	mustExec(t, db, `INSERT INTO t VALUES (1), (2), (3), (4), (5)`)
	mustExec(t, db, `CREATE SEQUENCE s`)
	rows, err := db.Query(`SELECT id FROM t ORDER BY id LIMIT nextval('s') + 1 OFFSET nextval('s')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	// OFFSET takes 1 and LIMIT 2 + 1.
	if !reflect.DeepEqual(got, []int64{2, 3, 4}) {
		t.Errorf("got %v", got)
	}
	// VALUES lists of different lengths are refused before any is evaluated.
	mustExec(t, db, `CREATE TABLE v (a int, b int)`)
	for _, q := range []string{`INSERT INTO v VALUES (nextval('s')), (1, 2)`, `INSERT INTO v (a) SELECT * FROM (VALUES (nextval('s')), (2, 3)) w`} {
		if _, err := db.Exec(q); !errors.Is(err, ErrSyntaxError) {
			t.Errorf("%s: got %v", q, err)
		}
	}
	// An aggregate detest does not implement is refused before WITH runs.
	if _, err := db.Exec(`WITH c AS (SELECT nextval('s') AS id) SELECT string_agg(id::text, ',') FROM c`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("string_agg: got %v", err)
	}
	// A bound may read a WITH query.
	var c int
	rows2, err := db.Query(`WITH c AS (VALUES (0)) SELECT 1 LIMIT (SELECT column1 FROM c)`)
	if err != nil {
		t.Fatal(err)
	}
	for rows2.Next() {
		c++
	}
	_ = rows2.Close()
	if c != 0 {
		t.Errorf("LIMIT from a WITH query: %d rows, want 0", c)
	}
	// OFFSET NULL is OFFSET 0.
	mustExec(t, db, `SELECT id FROM t OFFSET NULL`)
	// The largest bigint is a valid OFFSET.
	mustExec(t, db, `SELECT id FROM t OFFSET 9223372036854775807`)
	// OFFSET and LIMIT out of the bigint range are refused, not wrapped.
	mustExec(t, db, `SELECT id FROM t LIMIT '2'`)
	for _, q := range []string{`SELECT id FROM t OFFSET 1e100`, `SELECT id FROM t LIMIT 1e100`, `SELECT id FROM t LIMIT 'bad'`, `SELECT id FROM t LIMIT 1.5`, `SELECT id FROM t LIMIT true`} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v", q, err)
		}
	}
	// A negative LIMIT is refused before the select list runs.
	if _, err := db.Exec(`SELECT nextval('s') FROM t LIMIT -1`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("negative LIMIT: got %v", err)
	}
	var n int64
	if err := db.QueryRow(`SELECT nextval('s')`).Scan(&n); err != nil || n != 3 {
		t.Errorf("nextval after a refused LIMIT: %d, %v", n, err)
	}
}

// Postgres gives an untyped string literal compared with a number the
// number's type, so '01' matches 1 wherever the comparison is written.
func TestUntypedLiteralComparedWithNumber(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, price float8, name text)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 1.5, '01'), (9, 2, '9')`)
	for _, tc := range []struct {
		query string
		want  string
	}{
		{`SELECT count(*) FROM t WHERE '01' = 1`, "2"},
		{`SELECT count(*) FROM t WHERE '01' IN (1, 2)`, "2"},
		{`SELECT count(*) FROM t WHERE '01' = ANY (ARRAY[1, 2])`, "2"},
		{`SELECT count(*) FROM t WHERE id = '01'`, "1"},
		{`SELECT count(*) FROM t WHERE id IN ('01')`, "1"},
		{`SELECT count(*) FROM t WHERE id <> ' 1 '`, "1"},
		{`SELECT count(*) FROM t WHERE id < '10'`, "2"},
		{`SELECT count(*) FROM t WHERE price < '2'`, "1"},
		{`SELECT CASE id WHEN '09' THEN 'nine' ELSE 'other' END FROM t WHERE id = 9`, "nine"},
		{`SELECT count(*) FROM t HAVING count(*) = '02'`, "2"},
		{`SELECT count(*) FROM t WHERE '01' IN (SELECT id FROM t)`, "2"},
		{`SELECT count(*) FROM t WHERE ('09', '9') IN (SELECT id, name FROM t)`, "2"},
		{`SELECT count(*) FROM t WHERE NULLIF(id, '01') IS NULL`, "1"},
		// A text column keeps its value: only the literal takes the other side's type.
		{`SELECT count(*) FROM t WHERE name = '01'`, "1"},
		{`SELECT count(*) FROM t WHERE name = '1'`, "0"},
	} {
		if got := rowsOf(t, db, tc.query); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: got %v, want %s", tc.query, got, tc.want)
		}
	}
	// Postgres compares '1.5' with a float8 and refuses it for an integer,
	// and detest cannot tell the two apart by the value.
	for _, q := range []string{
		`SELECT count(*) FROM t WHERE price = '1.50'`,
		`SELECT count(*) FROM t WHERE id = '1.5'`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
	}
	for _, q := range []string{
		`SELECT count(*) FROM t WHERE id = 'abc'`,
		`SELECT count(*) FROM t WHERE price = 'abc'`,
	} {
		if _, err := db.Exec(q); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%s: got %v, want invalid input syntax", q, err)
		}
	}
}

// Postgres converts a string written to a number column to the column's
// type, so the stored value compares as the number it reads as.
func TestNumberColumnStoresNumbers(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, ratio float8, amount numeric, name text)`)
	mustExec(t, db, `INSERT INTO t VALUES ('1', '0.5', '99.00', 'a')`)
	mustExec(t, db, `INSERT INTO t VALUES ($1, $2, $3, $4)`, "2", "1.5", "100.50", "b")
	mustExec(t, db, `UPDATE t SET amount = ' 7 ' WHERE id = 2`)
	for _, tc := range []struct {
		query string
		args  []any
		want  []string
	}{
		{`SELECT id FROM t WHERE id = '01'`, nil, []string{"1"}},
		{`SELECT id FROM t WHERE id = $1`, []any{"02"}, []string{"2"}},
		{`SELECT id FROM t WHERE id = $1`, []any{[]byte("02")}, []string{"2"}},
		{`SELECT id FROM t WHERE ratio > 1 ORDER BY id`, nil, []string{"2"}},
		{`SELECT id FROM t WHERE amount < 100 ORDER BY id`, nil, []string{"1", "2"}},
		{`SELECT id, amount FROM t ORDER BY amount`, nil, []string{"2,7", "1,99"}},
	} {
		if got := rowsOf(t, db, tc.query, tc.args...); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.query, got, tc.want)
		}
	}
	// The key is the converted value, so '01' conflicts with the row of 1.
	if _, err := db.Exec(`INSERT INTO t (id) VALUES ('01')`); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("got %v, want a unique violation", err)
	}
	// Postgres rounds a numeric literal and refuses a float parameter with a
	// fraction, which detest cannot tell apart; a whole float is an integer.
	for _, q := range []string{`INSERT INTO t (id) VALUES (1.5)`, `UPDATE t SET id = 2.5 WHERE id = 1`} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
	}
	mustExec(t, db, `INSERT INTO t (id) VALUES ($1)`, 6.0)
	if got := rowsOf(t, db, `SELECT id FROM t WHERE id = '06'`); !reflect.DeepEqual(got, []string{"6"}) {
		t.Errorf("whole float: got %v", got)
	}
	// A numeric is not divided as an integer, however it was written.
	mustExec(t, db, `CREATE TABLE d (id int PRIMARY KEY, n numeric)`)
	mustExec(t, db, `INSERT INTO d VALUES (1, '1'), (2, $1), (3, $2)`, 1.0, int64(1))
	if got := rowsOf(t, db, `SELECT n / 2 FROM d ORDER BY id`); !reflect.DeepEqual(got, []string{"0.5", "0.5", "0.5"}) {
		t.Errorf("numeric division: got %v", got)
	}
	// A numeric is kept as a float, so an integer a float cannot keep
	// exactly is refused rather than kept as one with integer arithmetic.
	mustExec(t, db, `CREATE TABLE k (n numeric PRIMARY KEY)`)
	mustExec(t, db, `INSERT INTO k VALUES ($1)`, float64(9007199254740994))
	if _, err := db.Exec(`INSERT INTO k VALUES ('9007199254740994.0')`); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("numeric key beyond 2^53 with a decimal point: got %v, want a unique violation", err)
	}
	// An integer a float keeps exactly is taken, even beyond 2^53.
	if _, err := db.Exec(`INSERT INTO k VALUES ($1)`, int64(9007199254740994)); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("numeric integer a float keeps: got %v, want a unique violation", err)
	}
	for _, tc := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO k VALUES ($1)`, []any{int64(9007199254740993)}},
		{`INSERT INTO k VALUES ('9007199254740993')`, nil},
		{`INSERT INTO k VALUES (9007199254740993)`, nil},
	} {
		if _, err := db.Exec(tc.query, tc.args...); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", tc.query, err)
		}
	}
	mustExec(t, db, `CREATE TABLE z (n numeric PRIMARY KEY, r float8)`)
	mustExec(t, db, `INSERT INTO z (n) VALUES ('-0')`)
	if _, err := db.Exec(`INSERT INTO z (n) VALUES (0)`); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("numeric -0 and 0: got %v, want a unique violation", err)
	}
	if _, err := db.Exec(`INSERT INTO z VALUES (1, '-0')`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("float -0: got %v, want unsupported", err)
	}
	mustExec(t, db, `CREATE TABLE s (id int PRIMARY KEY, n numeric)`)
	mustExec(t, db, `INSERT INTO s VALUES (1, '1.5'), (2, '0.5')`)
	// Arithmetic on a numeric stays float arithmetic, even through a whole
	// intermediate result.
	for _, tc := range []struct{ query, want string }{
		{`SELECT (n * 2) / 2 FROM s WHERE id = 1`, "1.5"},
		{`SELECT sum(n) / 4 FROM s`, "0.5"},
	} {
		if got := rowsOf(t, db, tc.query); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: got %v, want %s", tc.query, got, tc.want)
		}
	}
	mustExec(t, db, `CREATE TABLE f (id int PRIMARY KEY, r float8)`)
	mustExec(t, db, `INSERT INTO f VALUES (1, 1), (2, $1)`, int64(1))
	if got := rowsOf(t, db, `SELECT r / 2 FROM f ORDER BY id`); !reflect.DeepEqual(got, []string{"0.5", "0.5"}) {
		t.Errorf("float division: got %v", got)
	}
	// A whole numeric has one key however it was written.
	mustExec(t, db, `CREATE TABLE u (n numeric UNIQUE)`)
	mustExec(t, db, `INSERT INTO u VALUES ($1)`, 1e6)
	for _, tc := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO u VALUES ('1000000')`, nil},
		{`INSERT INTO u VALUES ($1)`, []any{1e6}},
		{`INSERT INTO u VALUES ($1)`, []any{int64(1000000)}},
	} {
		if _, err := db.Exec(tc.query, tc.args...); !errors.Is(err, ErrUniqueViolation) {
			t.Errorf("%s: got %v, want a unique violation", tc.query, err)
		}
	}
	// A numeric a float cannot keep exactly would collapse into another value.
	if _, err := db.Exec(`INSERT INTO t (id, amount) VALUES (7, '0.12345678901234567890')`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("numeric beyond a float: got %v, want unsupported", err)
	}
	if _, err := db.Exec(`INSERT INTO t (id, amount) VALUES (7, '1e400')`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("numeric beyond a float's range: got %v, want unsupported", err)
	}
	mustExec(t, db, `INSERT INTO t (id, amount) VALUES (7, '1.10')`)
	// Postgres sorts NaN above every number, which detest does not.
	for _, v := range []any{"NaN", math.NaN()} {
		if _, err := db.Exec(`INSERT INTO t (id, ratio) VALUES (8, $1)`, v); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%v: got %v, want unsupported", v, err)
		}
	}
	if _, err := db.Exec(`SELECT id FROM t WHERE ratio < $1`, math.NaN()); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("comparison with NaN: got %v, want unsupported", err)
	}
	// A value of another type is not converted, where Postgres refuses it.
	for _, v := range []any{true, time.Unix(0, 0)} {
		if _, err := db.Exec(`INSERT INTO t (id, amount) VALUES (5, $1)`, v); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%T: got %v, want unsupported", v, err)
		}
	}
	// Postgres reads 0x10, 0x1p2 or 1_000 for some number types and not
	// others, which detest does not model.
	for _, q := range []string{
		`INSERT INTO t (id, ratio) VALUES (9, '0x1p2')`,
		`INSERT INTO t (id, amount) VALUES (9, '1_000')`,
		`INSERT INTO t (id) VALUES ('0x10')`,
		`SELECT '0x1p2'::numeric`,
		`SELECT '0x10'::int`,
		`SELECT count(*) FROM t WHERE ratio = '0x1p2'`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
	}
	for _, q := range []string{
		`INSERT INTO t (id) VALUES ('abc')`,
		`INSERT INTO t (id) VALUES ('1.5')`,
		`INSERT INTO t (id, ratio) VALUES (3, 'abc')`,
		`INSERT INTO t (id, amount) VALUES (3, 'abc')`,
		`UPDATE t SET id = 'x' WHERE id = 1`,
	} {
		if _, err := db.Exec(q); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%s: got %v, want invalid input syntax", q, err)
		}
	}
}

// Postgres has no operator comparing text with a number, and resolves only an
// untyped literal or a parameter to the other side's type.
func TestTextComparedWithNumber(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, '1')`)
	// A number written to a text column is stored as its text.
	mustExec(t, db, `INSERT INTO t VALUES (2, $1)`, int64(2))
	if got := rowsOf(t, db, `SELECT count(*) FROM t WHERE name = '02'`); len(got) != 1 || got[0] != "0" {
		t.Errorf("name = '02': got %v, want 0", got)
	}
	for _, v := range []any{true, time.Unix(0, 0), 1.2} {
		if _, err := db.Exec(`INSERT INTO t VALUES (3, $1)`, v); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%T to a text column: got %v, want unsupported", v, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (3, 1.20)`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("numeric literal to a text column: got %v, want unsupported", err)
	}
	// A parameter holding a boolean or a time cannot be sent as a number.
	for _, v := range []any{true, time.Unix(0, 0)} {
		if _, err := db.Exec(`SELECT count(*) FROM t WHERE id = $1`, v); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%T: got %v, want unsupported", v, err)
		}
	}
	// The text a driver sends for a float parameter is not modeled.
	for _, v := range []any{1.5, true, time.Unix(0, 0)} {
		for _, q := range []string{`SELECT count(*) FROM t WHERE name = $1`, `SELECT count(*) FROM t WHERE $1 = '1.5'`,
			`SELECT count(*) FROM t WHERE name = ANY (ARRAY['x', $1])`} {
			if _, err := db.Exec(q, v); !errors.As(err, new(*ErrUnsupportedSQL)) {
				t.Errorf("%s with %v: got %v, want unsupported", q, v, err)
			}
		}
	}
	// Bytes written to a text column order as the text they hold.
	mustExec(t, db, `CREATE TABLE b (id int PRIMARY KEY, s text)`)
	mustExec(t, db, `INSERT INTO b VALUES (1, $1), (2, '2')`, []byte("10"))
	if got := rowsOf(t, db, `SELECT id FROM b ORDER BY s LIMIT 1`); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("bytes in a text column: got %v, want [1]", got)
	}
	// A byte slice parameter compared with text orders as the text it is.
	if got := rowsOf(t, db, `SELECT count(*) FROM t WHERE name < $1`, []byte("10")); len(got) != 1 || got[0] != "1" {
		t.Errorf("name < []byte: got %v, want 1", got)
	}
	// A parameter compared with a text cast takes the text type too.
	for _, q := range []string{`SELECT '1'::text = $1`, `SELECT $1 = ANY (ARRAY['1'])`} {
		if got := rowsOf(t, db, q, int64(1)); !reflect.DeepEqual(got, []string{"true"}) {
			t.Errorf("%s with 1: got %v, want true", q, got)
		}
	}
	// A parameter compared with an untyped literal is text, as both are.
	if got := rowsOf(t, db, `SELECT count(*) FROM t WHERE $1 = '01'`, int64(1)); len(got) != 1 || got[0] != "0" {
		t.Errorf("$1 = '01': got %v, want 0", got)
	}
	for _, q := range []string{
		`SELECT count(*) FROM t WHERE id = 2 AND name = 2`,
		`SELECT count(*) FROM t WHERE name = 1`,
		`SELECT count(*) FROM t WHERE id = name`,
		`SELECT count(*) FROM t WHERE name::text < 2`,
		`SELECT count(*) FROM t WHERE 1 IN (SELECT name FROM t)`,
		`SELECT count(*) FROM t WHERE id = true`,
		`SELECT count(*) FROM t WHERE localtimestamp > '2024-01-01'`,
		`SELECT count(*) FROM t WHERE id = ANY (ARRAY['1', '2'])`,
		`SELECT count(*) FROM t WHERE id = ANY (ARRAY['1', NULL::text])`,
		`SELECT count(*) FROM t WHERE id = ANY (ARRAY[NULL])`,
		`SELECT count(*) FROM t WHERE id = ANY (ARRAY['1'::varchar, '2'])`,
		`SELECT count(*) FROM t WHERE 1000000000 = '1 second'::interval`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
	}
	for _, tc := range []struct {
		query string
		args  []any
	}{
		{`SELECT count(*) FROM t WHERE name = $1`, []any{int64(1)}},
		{`SELECT count(*) FROM t WHERE id = $1`, []any{"1"}},
		{`SELECT count(*) FROM t WHERE name = '1'`, nil},
	} {
		if got := rowsOf(t, db, tc.query, tc.args...); len(got) != 1 || got[0] != "1" {
			t.Errorf("%s: got %v, want 1", tc.query, got)
		}
	}
}

func TestBooleanAndTimestampColumnsStoreTheirValues(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE e (id int PRIMARY KEY, at timestamptz, ts timestamp, active bool, name text)`)
	at := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mustExec(t, db, `INSERT INTO e VALUES (1, '2024-01-01 09:00:00+09', '2024-01-01 00:00:00+09', 'true', 't')`)
	mustExec(t, db, `INSERT INTO e VALUES (2, $1, $2, true, 'true')`, at, at)
	mustExec(t, db, `INSERT INTO e VALUES (3, $1, $2, $3, 'f')`, "2023-12-31T00:00:00Z", []byte("2023-12-31"), []byte("off"))
	for _, tc := range []struct {
		query string
		args  []any
		want  []string
	}{
		{`SELECT id FROM e WHERE active ORDER BY id`, nil, []string{"1", "2"}},
		{`SELECT id FROM e WHERE NOT active ORDER BY id`, nil, []string{"3"}},
		{`SELECT id FROM e WHERE at = (SELECT at FROM e WHERE id = 2) ORDER BY id`, nil, []string{"1", "2"}},
		{`SELECT id FROM e WHERE ts = (SELECT ts FROM e WHERE id = 2) ORDER BY id`, nil, []string{"1", "2"}},
		{`SELECT id FROM e ORDER BY at DESC, id LIMIT 2`, nil, []string{"1", "2"}},
	} {
		if got := rowsOf(t, db, tc.query, tc.args...); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.query, got, tc.want)
		}
	}
	// A timestamp(p) rounds what it stores to p digits, also after
	// ALTER COLUMN TYPE.
	mustExec(t, db, `CREATE TABLE p (id int PRIMARY KEY, at timestamptz(0), ts timestamp)`)
	mustExec(t, db, `INSERT INTO p VALUES (1, $1, '2024-01-01 00:00:00.6')`, at.Add(1400*time.Millisecond))
	mustExec(t, db, `ALTER TABLE p ALTER COLUMN ts TYPE timestamp(0)`)
	var gotAt, gotTs time.Time
	if err := db.QueryRow(`SELECT at, ts FROM p`).Scan(&gotAt, &gotTs); err != nil {
		t.Fatal(err)
	}
	if !gotAt.Equal(at.Add(time.Second)) || !gotTs.Equal(at.Add(time.Second)) {
		t.Errorf("timestamp(0): got %v and %v, want both %v", gotAt, gotTs, at.Add(time.Second))
	}
	if _, err := db.Exec(`INSERT INTO e (id, active) VALUES (4, 'maybe')`); !errors.Is(err, ErrInvalidTextRepresentation) {
		t.Errorf("'maybe' to a boolean column: got %v, want invalid input", err)
	}
	for _, tc := range []struct {
		query string
		args  []any
	}{
		// Postgres reads it in the session's TimeZone, which is not modeled.
		{`INSERT INTO e (id, at) VALUES (4, 'today')`, nil},
		{`INSERT INTO e (id, ts) VALUES (4, 'Jan 1 2024')`, nil},
		{`INSERT INTO e (id, ts) VALUES (4, $1)`, []any{int64(1)}},
		// Postgres refuses the literal and reads the parameter as text.
		{`INSERT INTO e (id, active) VALUES (4, 1)`, nil},
		{`INSERT INTO e (id, active) VALUES (4, $1)`, []any{int64(1)}},
		// Text from a column has no operator with a boolean or a time.
		{`SELECT count(*) FROM e WHERE name = active`, nil},
		{`SELECT count(*) FROM e WHERE at > name`, nil},
	} {
		if _, err := db.Exec(tc.query, tc.args...); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", tc.query, err)
		}
	}
}

func TestUntypedTextComparedWithUUID(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE a (id int PRIMARY KEY, u uuid, name text)`)
	mustExec(t, db, `INSERT INTO a VALUES (1, '00000000-0000-0000-0000-000000000001', 'x')`)
	mustExec(t, db, `INSERT INTO a VALUES (2, $1, 'y')`, []byte("{00000000-0000-0000-0000-00000000000A}"))
	upper := "00000000-0000-0000-0000-00000000000A"
	for _, tc := range []struct {
		query string
		args  []any
		want  []string
	}{
		{`SELECT id FROM a WHERE u = $1`, []any{upper}, []string{"2"}},
		{`SELECT id FROM a WHERE $1 = u`, []any{[]byte("0000000000000000000000000000000a")}, []string{"2"}},
		{`SELECT id FROM a WHERE u = ANY($1)`, []any{[]string{upper}}, []string{"2"}},
		{`SELECT id FROM a WHERE u = ANY($1)`, []any{"{" + upper + "}"}, []string{"2"}},
		{`SELECT id FROM a WHERE u IN ($1, $2) ORDER BY id`, []any{upper, "00000000-0000-0000-0000-000000000001"}, []string{"1", "2"}},
		{`SELECT id FROM a WHERE u = gen_random_uuid()`, nil, nil},
	} {
		if got := rowsOf(t, db, tc.query, tc.args...); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.query, got, tc.want)
		}
	}
	// The client reads the uuid back as the text Postgres prints.
	var got any
	if err := db.QueryRow(`SELECT u FROM a WHERE id = 2`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "00000000-0000-0000-0000-00000000000a" {
		t.Errorf("uuid read back: got %#v", got)
	}
	for _, tc := range []struct {
		query string
		args  []any
	}{
		{`SELECT id FROM a WHERE u = 'nope'`, nil},
		{`SELECT id FROM a WHERE u = $1`, []any{"nope"}},
		{`SELECT id FROM a WHERE u = ANY($1)`, []any{[]string{"nope"}}},
		// Read when the statement is bound, before it reaches a row.
		{`SELECT id FROM a WHERE id < 0 AND u = $1`, []any{"nope"}},
		{`SELECT id FROM a WHERE true OR u = $1`, []any{"nope"}},
		{`SELECT id FROM a WHERE false AND id = $1`, []any{"abc"}},
		{`SELECT id FROM a WHERE id < 0 AND u = ANY($1)`, []any{[]string{"nope"}}},
	} {
		if _, err := db.Exec(tc.query, tc.args...); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%s: got %v, want invalid input", tc.query, err)
		}
	}
	for _, tc := range []struct {
		query string
		args  []any
	}{
		// Postgres has no operator comparing text or a number with a uuid.
		{`SELECT id FROM a WHERE u = name`, nil},
		{`SELECT id FROM a WHERE u > '00000000-0000-0000-0000-00000000000' || '2'`, nil},
		{`SELECT id FROM a WHERE u = 1`, nil},
		{`SELECT id FROM a WHERE u = $1`, []any{int64(1)}},
		// Postgres types a parameter by its first use, so the second
		// comparison has no operator.
		{`SELECT id FROM a WHERE id = $1 OR u = $1`, []any{"1"}},
		{`SELECT id FROM a WHERE name = $1 OR id = $1`, []any{"1"}},
		// Refused before any row, as Postgres has no operator for them.
		{`SELECT id FROM a WHERE id < 0 AND u = name`, nil},
		{`SELECT id FROM a WHERE id < 0 AND (u = 1 OR id = name)`, nil},
		{`SELECT id FROM a WHERE false AND u IN (name)`, nil},
		// A driver sends these in a form of its own.
		{`SELECT id FROM a WHERE u = $1`, []any{true}},
		{`SELECT id FROM a WHERE u = $1`, []any{time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}},
		{`INSERT INTO a (id, u) VALUES (3, 1)`, nil},
		{`INSERT INTO a (id, u) VALUES (3, $1)`, []any{int64(1)}},
	} {
		if _, err := db.Exec(tc.query, tc.args...); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", tc.query, err)
		}
	}
}

func TestTypedInputReadBeforeAnyRow(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, u uuid, n int, b bool, f float8, k text UNIQUE)`)
	for _, tc := range []struct {
		query string
		args  []any
	}{
		{`UPDATE t SET u = $1 WHERE false`, []any{"bad"}},
		{`UPDATE t SET n = 'abc' WHERE false`, nil},
		{`UPDATE t SET n = $1 WHERE false`, []any{"1.5"}},
		{`UPDATE t SET b = $1 WHERE false`, []any{"maybe"}},
		{`SELECT $1::uuid FROM t`, []any{"bad"}},
		{`SELECT 'abc'::int FROM t`, nil},
		{`INSERT INTO t (id, u) SELECT 1, $1 WHERE false`, []any{"bad"}},
		{`INSERT INTO t SELECT 1, 'bad' WHERE false`, nil},
		{`INSERT INTO t (id, k) SELECT id, k FROM t WHERE false ON CONFLICT (k) DO UPDATE SET u = 'bad'`, nil},
		{`SELECT CASE WHEN true THEN u ELSE 'bad' END FROM t`, nil},
		{`SELECT CASE WHEN id > 0 THEN u ELSE $1 END FROM t`, []any{"bad"}},
		{`SELECT coalesce(u, 'bad') FROM t`, nil},
		{`SELECT greatest(n, $1) FROM t`, []any{"abc"}},
		{`SELECT u FROM t WHERE false UNION SELECT 'bad' WHERE false`, nil},
		{`SELECT 'bad' WHERE false UNION ALL SELECT u FROM t`, nil},
		{`SELECT * FROM t WHERE false UNION SELECT 1, 'bad', 1, true, 1.5, 'k' WHERE false`, nil},
		{`VALUES (1) UNION SELECT 'bad' WHERE false`, nil},
		{`VALUES (1) UNION VALUES (2) UNION SELECT 'bad' WHERE false`, nil},
		{`SELECT (SELECT u FROM t) WHERE false UNION SELECT 'bad' WHERE false`, nil},
		{`VALUES ((SELECT u FROM t)) UNION ALL SELECT 'bad' WHERE false`, nil},
		{`SELECT (SELECT (SELECT (SELECT u FROM t))) WHERE false UNION SELECT 'bad' WHERE false`, nil},
		{`VALUES ((SELECT u FROM t UNION ALL SELECT u FROM t)) UNION ALL SELECT 'bad' WHERE false`, nil},
		{`SELECT CASE WHEN true THEN (SELECT u FROM t) ELSE $1 END`, []any{"bad"}},
		{`SELECT coalesce((SELECT u FROM t), $1)`, []any{"bad"}},
		{`VALUES ((VALUES (1))) UNION ALL SELECT 'bad' WHERE false`, nil},
		{`VALUES (1), ($1) LIMIT 0`, []any{"bad"}},
		{`SELECT (VALUES (t.u) UNION ALL SELECT 'bad' WHERE false) FROM t`, nil},
		{`SELECT coalesce((VALUES (1)), 'bad')`, nil},
		{`SELECT CASE WHEN true THEN 1 ELSE (VALUES (n) UNION SELECT $1) END FROM t`, []any{"bad"}},
		{`SELECT (SELECT u FROM t UNION ALL SELECT u FROM t) WHERE false UNION ALL SELECT 'bad' WHERE false`, nil},
		{`SELECT coalesce((SELECT u FROM t UNION ALL SELECT u FROM t), 'bad')`, nil},
	} {
		if _, err := db.Exec(tc.query, tc.args...); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%s: got %v, want invalid input", tc.query, err)
		}
	}
	// Postgres resolves a set operation pair by pair, so a nested one whose
	// queries are all untyped is text, which an integer cannot join (42804).
	for _, q := range []string{
		`SELECT 'x' UNION SELECT 'y' UNION SELECT 1`,
		`SELECT '1' WHERE false UNION SELECT NULL WHERE false UNION SELECT 1 WHERE false`,
		`SELECT k FROM t WHERE false UNION SELECT u FROM t WHERE false`,
		// A pair valueFamily does not group is refused, whether or not
		// Postgres resolves it.
		`SELECT k FROM t WHERE false UNION SELECT now()::date WHERE false`,
		`SELECT now()::date WHERE false UNION SELECT now()::timestamp WHERE false`,
		// The check knows a *'s column types by position only over one
		// declared table.
		`SELECT * FROM t JOIN t t2 ON true WHERE false UNION SELECT 1, 'bad', 1, true, 1.5, 'k', 1, 'bad', 1, true, 1.5, 'k' WHERE false`,
		`SELECT *, 'bad' FROM t WHERE false UNION SELECT *, u FROM t WHERE false`,
		// Postgres resolves no type for a number and a boolean branch.
		`SELECT CASE WHEN true THEN 1 ELSE false END FROM t`,
		`SELECT coalesce(n, b) FROM t`,
		`SELECT greatest(u, k) FROM t`,
		// A nested COALESCE of untyped arguments alone is text.
		`SELECT CASE WHEN true THEN 1 ELSE coalesce('bad', NULL) END`,
		`SELECT CASE WHEN true THEN 1 ELSE coalesce(coalesce('bad', NULL), NULL) END`,
		`SELECT coalesce('1', NULL) WHERE false UNION SELECT 1 WHERE false`,
		`VALUES ((SELECT 'bad' WHERE false)) UNION SELECT 1 WHERE false`,
		`INSERT INTO t (id, u) SELECT *, 'bad' FROM (SELECT 1) s WHERE false`,
		`(SELECT * FROM (SELECT u FROM t) a UNION ALL SELECT * FROM (SELECT u FROM t) b) UNION ALL SELECT 'bad' WHERE false`,
		// Postgres resolves a column of untyped queries alone to text,
		// which it does not assign to an integer.
		`INSERT INTO t (id, n) SELECT 1, '1' WHERE false UNION ALL SELECT 2, '2' WHERE false`,
		`INSERT INTO t (n) SELECT coalesce('1', NULL) WHERE false UNION ALL SELECT '2' WHERE false`,
		`INSERT INTO t (n) SELECT k FROM t`,
		`INSERT INTO t (n) SELECT coalesce('1', NULL)`,
		// Postgres resolves a VALUES column over its rows, whether or not
		// a LIMIT drops them.
		`VALUES (1), (false) LIMIT 0`,
		`(VALUES (1), (false) LIMIT 0) UNION ALL SELECT 2 WHERE false`,
		`(VALUES (coalesce('bad', NULL)) LIMIT 0) UNION ALL SELECT 1 WHERE false`,
		// Postgres reads prefixed and underscored integers, which detest
		// does not model.
		`SELECT '0b2'::int FROM t`,
		`SELECT '1_000'::int FROM t`,
		// A scalar subquery's bare NULL is text at its output.
		`SELECT (SELECT NULL) WHERE false UNION ALL SELECT 1 WHERE false`,
		`SELECT coalesce((SELECT NULL), 1)`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
	}
	// A cast detest refuses is refused over no row too, as it would run
	// with other semantics.
	mustExec(t, db, `CREATE DOMAIN positive AS int CHECK (VALUE > 0)`)
	if _, err := db.Exec(`SELECT 'abc'::positive FROM t`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a cast to a constrained domain over no row: got %v, want unsupported", err)
	}
	// Postgres gives a parameter one type for the statement, so one
	// written to columns of two families fails whatever is bound to it.
	// One of two number types is refused too, as Postgres reads it by the
	// first it meets and converts it to the other at each row.
	for _, q := range []string{
		`UPDATE t SET b = $1, n = $1 WHERE false`,
		`UPDATE t SET n = $1 WHERE k = $1`,
		`SELECT coalesce(u, $1), coalesce(n, $1) FROM t`,
		`UPDATE t SET id = $1 WHERE false AND $1 = 1::smallint`,
		`SELECT coalesce(n, $1), coalesce(1::bigint, $1) FROM t`,
		`SELECT $1::text, CASE WHEN true THEN 0 ELSE $1::int END`,
		`SELECT $1::int, CASE WHEN false THEN $1::smallint END`,
		`SELECT coalesce($1, NULL), CASE WHEN true THEN 0 ELSE $1::int END`,
		`INSERT INTO t (id, n) VALUES (1, $1) RETURNING CASE WHEN true THEN 0 ELSE $1::smallint END`,
		// Postgres fails a parameter VALUES writes to two types (42P08).
		`INSERT INTO t (id, k, u) VALUES (1, $1, $1)`,
		// Postgres fails a parameter it meets first under IS NULL (42P18),
		// in an order that is not the statement's.
		`UPDATE t SET n = $1 WHERE $1 IS NOT NULL AND false`,
		`SELECT $1 IS NULL, $1::int`,
		// Arithmetic types the parameter where the checks do not follow.
		`SELECT $1 + 1, CASE WHEN true THEN 0 ELSE $1::smallint END FROM t`,
	} {
		if _, err := db.Exec(q, "1"); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
	}
	if _, err := db.Exec(`SELECT n FROM t WHERE false UNION SELECT 'x' UNION SELECT 1`); !errors.Is(err, ErrInvalidTextRepresentation) {
		t.Errorf("a nested set operation over an integer: got %v, want invalid input", err)
	}
	// A parameter taken as a type paramFamily does not group, such as a
	// bytea, and as another type is refused.
	mustExec(t, db, `CREATE TABLE y (id int PRIMARY KEY, u uuid, bin bytea, d date)`)
	if _, err := db.Exec(`UPDATE y SET u = $1, bin = $1 WHERE false`, "00000000-0000-0000-0000-000000000001"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a parameter taken as a uuid and a bytea: got %v, want unsupported", err)
	}
	// A parameter a set operation types as a date is sent as a date, the
	// time's own day, as one written to a date column is.
	at := time.Date(2024, 5, 20, 8, 0, 0, 0, time.FixedZone("JST", 9*3600))
	var day time.Time
	if err := db.QueryRow(`SELECT d FROM y WHERE false UNION ALL SELECT $1`, at).Scan(&day); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2024, 5, 20, 0, 0, 0, 0, time.UTC); !day.Equal(want) {
		t.Errorf("a time bound where a set operation takes a date: got %v, want %v", day, want)
	}
	// The driver sends a float's digits as integer input, which a cast
	// would round instead.
	if _, err := db.Exec(`UPDATE t SET n = $1 WHERE false`, 1.25); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a float with a fraction for an integer column: got %v, want unsupported", err)
	}
	if _, err := db.Exec(`UPDATE t SET n = $1 WHERE false`, true); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a boolean for an integer column: got %v, want unsupported", err)
	}
	for _, v := range []any{int64(1), true} {
		if _, err := db.Exec(`UPDATE t SET u = $1 WHERE false`, v); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("a %T for a uuid column: got %v, want unsupported", v, err)
		}
	}
	// A value the cast does not convert as the driver sends it is refused,
	// as the run refuses it where a row reaches it.
	for _, tc := range []struct {
		query string
		v     any
	}{
		{`UPDATE t SET b = $1 WHERE false`, 1.25},
		{`UPDATE t SET n = $1 WHERE false`, at},
		{`SELECT $1::int FROM t`, at},
	} {
		if _, err := db.Exec(tc.query, tc.v); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s with a %T: got %v, want unsupported", tc.query, tc.v, err)
		}
	}
	if _, err := db.Exec(`UPDATE y SET d = $1 WHERE false`, int64(3)); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("an integer for a date column: got %v, want unsupported", err)
	}
	// An integer is read by its digits, as the driver sends it, for a
	// boolean domain too.
	mustExec(t, db, `CREATE DOMAIN flag AS boolean`)
	for _, q := range []string{`UPDATE t SET b = $1 WHERE false`, `SELECT $1::flag FROM t`} {
		if _, err := db.Exec(q, int64(5)); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%s with 5: got %v, want invalid input", q, err)
		}
	}
	mustExec(t, db, `CREATE TABLE w (id int PRIMARY KEY, ts timestamp, tz timestamptz)`)
	for _, q := range []string{`UPDATE w SET ts = $1 WHERE false`, `UPDATE w SET tz = $1 WHERE false`} {
		for _, v := range []any{true, int64(3)} {
			if _, err := db.Exec(q, v); !errors.As(err, new(*ErrUnsupportedSQL)) {
				t.Errorf("%s with a %T: got %v, want unsupported", q, v, err)
			}
		}
		for _, v := range []any{nil, at, "2024-05-20 10:00"} {
			if _, err := db.Exec(q, v); err != nil {
				t.Errorf("%s with %v: got %v, want no error", q, v, err)
			}
		}
	}
	if _, err := db.Exec(`UPDATE t SET u = $1 WHERE false`, nil); err != nil {
		t.Errorf("NULL for a uuid column: got %v, want no error", err)
	}
	if _, err := db.Exec(`UPDATE t SET n = $1 WHERE false`, 2.0); err != nil {
		t.Errorf("a whole float for an integer column: got %v, want no error", err)
	}
	// The width fails before the source runs, so its nextval does not
	// advance the sequence.
	mustExec(t, db, `CREATE SEQUENCE q`)
	if _, err := db.Exec(`INSERT INTO t (id) SELECT * FROM (SELECT nextval('q') AS id, 2 AS extra) s`); !errors.Is(err, ErrSyntaxError) {
		t.Errorf("a derived table of two columns into one: got %v, want a syntax error", err)
	}
	// One whose width the check does not count, as of a function, is
	// refused when its query changes state.
	if _, err := db.Exec(`INSERT INTO t (id) SELECT * FROM generate_series(nextval('q'), 1) WITH ORDINALITY`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a function source with nextval of an unknown width: got %v, want unsupported", err)
	}
	if _, err := db.Exec(`INSERT INTO t (id) SELECT 1 UNION ALL SELECT * FROM generate_series(nextval('q'), 1) WITH ORDINALITY`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a set operation of a function source with nextval: got %v, want unsupported", err)
	}
	if got := rowsOf(t, db, `SELECT nextval('q')`); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("nextval after the failed INSERT: got %v, want 1", got)
	}
	// An outer query's column in a set operation is not a width mismatch.
	if _, err := db.Exec(`SELECT (SELECT t.u WHERE false UNION ALL SELECT NULL WHERE false) FROM t`); err != nil {
		t.Errorf("an outer column in a set operation: got %v, want no error", err)
	}
	// The run expands a * over its own query's items only.
	if _, err := db.Exec(`SELECT (SELECT x.* WHERE false UNION ALL SELECT NULL WHERE false) FROM (SELECT u FROM t) x`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("an outer x.* in a set operation: got %v, want unsupported", err)
	}
	// Postgres converts an explicitly cast literal where it analyzes it,
	// before it compares a set operation's widths.
	if _, err := db.Exec(`SELECT 'bad'::int WHERE false UNION SELECT 1, 2 WHERE false`); !errors.Is(err, ErrInvalidTextRepresentation) {
		t.Errorf("a cast literal beside unequal widths: got %v, want invalid input", err)
	}
	// INSERT ... VALUES reads every row before it writes one, so a later
	// row's input fails before an earlier row's duplicate key.
	mustExec(t, db, `INSERT INTO t (id, u) VALUES (100, '00000000-0000-0000-0000-000000000001')`)
	for _, tc := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO t (id, u) VALUES (100, '00000000-0000-0000-0000-000000000001'), (101, 'bad')`, nil},
		{`INSERT INTO t (id, u) VALUES (100, '00000000-0000-0000-0000-000000000001'), (101, $1)`, []any{"bad"}},
	} {
		if _, err := db.Exec(tc.query, tc.args...); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%s: got %v, want invalid input", tc.query, err)
		}
	}
	mustExec(t, db, `DELETE FROM t WHERE id = 100`)
	// A derived table of a set operation or VALUES gives its columns'
	// types, and one whose type the check does not know is refused beside
	// untyped input.
	for _, q := range []string{
		`SELECT column1 FROM (VALUES (1)) v WHERE false UNION ALL SELECT 'bad' WHERE false`,
		`SELECT s.u FROM (SELECT u FROM t UNION ALL SELECT u FROM t) s WHERE false UNION ALL SELECT 'bad' WHERE false`,
		`WITH w AS (SELECT u FROM t UNION ALL SELECT u FROM t) SELECT u FROM w WHERE false UNION ALL SELECT 'bad' WHERE false`,
	} {
		if _, err := db.Exec(q); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%s: got %v, want invalid input", q, err)
		}
	}
	// A * over a derived table gives its items' columns with their types.
	if _, err := db.Exec(`SELECT s.u FROM (SELECT * FROM t, (SELECT 1 AS k) x) s WHERE false UNION ALL SELECT 'bad' WHERE false`); !errors.Is(err, ErrInvalidTextRepresentation) {
		t.Errorf("a column of a * beside a literal: got %v, want invalid input", err)
	}
	// Postgres fails a statement of unequal widths before it reads any
	// input by type.
	for _, q := range []string{
		`VALUES (1::smallint), ('40000', 2) LIMIT 0`,
		`SELECT 1 WHERE false UNION SELECT 'bad', 2 WHERE false`,
		`SELECT * FROM t WHERE false UNION SELECT 'bad' WHERE false`,
		`INSERT INTO t (u) SELECT 'bad', 1 WHERE false`,
		`INSERT INTO t (id, u) SELECT 1 WHERE false`,
		`INSERT INTO t (id) SELECT 1, 2`,
		`INSERT INTO t (id, u) VALUES (1, 'bad'), (2)`,
		`INSERT INTO t (id) VALUES (1, 'bad')`,
		// A * over a derived table counts the columns the table names.
		`INSERT INTO t (id) SELECT * FROM (SELECT 1 AS id, 2 AS extra) s`,
		`INSERT INTO t (id, u) SELECT * FROM (SELECT 1 AS id) s`,
	} {
		if _, err := db.Exec(q); !errors.Is(err, ErrSyntaxError) {
			t.Errorf("%s: got %v, want a syntax error", q, err)
		}
	}
	if _, err := db.Exec(`SELECT NULL UNION SELECT 1`); err != nil {
		t.Errorf("a bare NULL beside an integer: got %v, want no error", err)
	}
	if _, err := db.Exec(`VALUES (1::smallint), ('40000') LIMIT 0`); !errors.Is(err, ErrNumericValueOutOfRange) {
		t.Errorf("VALUES of a smallint and 40000: got %v, want 22003", err)
	}
	if _, err := db.Exec(`SELECT NULLIF(1::smallint, '40000') FROM t`); !errors.Is(err, ErrNumericValueOutOfRange) {
		t.Errorf("NULLIF of a smallint and 40000: got %v, want 22003", err)
	}
	if _, err := db.Exec(`UPDATE t SET n = $1 WHERE false`, "99999999999"); !errors.Is(err, ErrNumericValueOutOfRange) {
		t.Errorf("an integer out of the column's range: got %v, want 22003", err)
	}
	// Valid input of the type passes, and so does input detest does not
	// read up front, such as a fraction for a float column.
	for _, tc := range []struct {
		query string
		args  []any
	}{
		{`UPDATE t SET u = $1, n = '7', b = 'yes' WHERE false`, []any{"00000000-0000-0000-0000-000000000001"}},
		{`UPDATE t SET f = '1.5' WHERE false`, nil},
		{`SELECT coalesce(f, '2.5') FROM t`, nil},
		{`SELECT CASE WHEN true THEN k ELSE 'anything' END FROM t`, nil},
		{`INSERT INTO t (id, n) SELECT 1, $1 WHERE false`, []any{int64(3)}},
		{`SELECT 'a' UNION SELECT 'b' UNION SELECT k FROM t`, nil},
		{`SELECT CASE WHEN true THEN now()::date ELSE now() END, coalesce(n, 1.5, 2::bigint) FROM t`, nil},
		{`UPDATE t SET n = $1 WHERE id = $1`, []any{"1"}},
		{`INSERT INTO t (id, k) SELECT 1, 'x' WHERE false UNION ALL SELECT 2, 'y' WHERE false`, nil},
		{`VALUES (1) UNION SELECT 2::smallint UNION SELECT '40000' WHERE false`, nil},
		{`SELECT 1::smallint WHERE false UNION (VALUES (2), ('40000') LIMIT 0)`, nil},
		{`SELECT 1::smallint WHERE false UNION ALL VALUES ($1), (2)`, []any{int64(40000)}},
		{`SELECT $1::int, $1::integer`, []any{"1"}},
		{`SELECT n FROM t UNION SELECT id FROM t`, nil},
		// The inner pair widens to an integer, which takes 40000.
		{`SELECT 1::smallint WHERE false UNION SELECT 2 WHERE false UNION SELECT '40000' WHERE false`, nil},
		{`SELECT * FROM t UNION SELECT * FROM t`, nil},
		// The branches widen to an integer, which takes 40000.
		{`SELECT CASE WHEN true THEN 1::smallint WHEN false THEN 2 ELSE '40000' END`, nil},
		{`SELECT coalesce(1::smallint, $1, 2)`, []any{"40000"}},
		// NULLIF promotes an integer compared with a numeric, which then
		// takes 1.5.
		// smallint + integer is an integer, which takes 40000.
		{`SELECT coalesce(1::smallint + 2, '40000')`, nil},
		{`SELECT coalesce(NULLIF(1, 2.5), '1.5')`, nil},
		{`SELECT coalesce(NULLIF(n, 2.5), '1.5') FROM t`, nil},
	} {
		if _, err := db.Exec(tc.query, tc.args...); err != nil {
			t.Errorf("%s: got %v, want no error", tc.query, err)
		}
	}
}

// The branch checks resolve a scalar subquery's type before the walk
// reaches it, so each level must resolve its subqueries once: resolving
// them again at every level above took minutes at this depth.
func TestNestedSubqueryTypesResolvedOnce(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY)`)
	q := "SELECT " + strings.Repeat("coalesce((SELECT ", 16) + "id FROM t" + strings.Repeat("), 0)", 16) + " FROM t"
	mustExec(t, db, q)
}

func TestEnumInputReadBeforeAnyRow(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TYPE mood AS ENUM ('ok')`)
	mustExec(t, db, `CREATE TABLE p (id int PRIMARY KEY, m mood)`)
	for _, q := range []string{
		`UPDATE p SET m = $1 WHERE false`,
		`SELECT $1::mood FROM p`,
		`SELECT CASE WHEN true THEN m ELSE $1 END FROM p`,
		`SELECT m FROM p WHERE false UNION SELECT $1 WHERE false`,
	} {
		if _, err := db.Exec(q, "bogus"); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%s: got %v, want invalid input", q, err)
		}
		if _, err := db.Exec(q, "ok"); err != nil {
			t.Errorf("%s with a label: got %v, want no error", q, err)
		}
	}
	// A parameter takes one type for the statement, which an enum shares
	// with no other.
	if _, err := db.Exec(`SELECT CASE WHEN true THEN m ELSE $1 END, CASE WHEN true THEN id ELSE $1 END FROM p WHERE false`, "1"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a parameter taken as an enum and an integer: got %v, want unsupported", err)
	}
	if _, err := db.Exec(`INSERT INTO p (id, m) SELECT 1, 'ok' WHERE false UNION ALL SELECT 2, 'ok' WHERE false`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a set operation of untyped values written to an enum: got %v, want unsupported", err)
	}
	// An enum resolves with no other type in a set operation.
	if _, err := db.Exec(`SELECT m FROM p WHERE false UNION SELECT 1 WHERE false`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("an enum and an integer in a set operation: got %v, want unsupported", err)
	}
}

func TestArithmeticOperandInputReadBeforeAnyRow(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int, f float8)`)
	for _, q := range []string{
		`SELECT $1 + 0 FROM t WHERE false`,
		`SELECT id FROM t WHERE n = $1 + 0`,
		`UPDATE t SET n = $1 * 2 WHERE false`,
		`SELECT id FROM t WHERE n > 0 AND n < $1 + 1`,
		`SELECT id FROM t WHERE n = 0 - $1`,
		`SELECT f / $1 FROM t`,
		`SELECT coalesce(n, $1 + 0) FROM t`,
	} {
		if _, err := db.Exec(q, "bogus"); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%s: got %v, want invalid input", q, err)
		}
		if _, err := db.Exec(q, int64(2)); err != nil {
			t.Errorf("%s with an integer: got %v, want no error", q, err)
		}
	}
	if _, err := db.Exec(`SELECT 'bogus' + 0 FROM t WHERE false`); !errors.Is(err, ErrInvalidTextRepresentation) {
		t.Errorf("a string literal beside an integer: got %v, want invalid input", err)
	}
	// The integer's input does not take a fraction.
	if _, err := db.Exec(`SELECT $1 + 0 FROM t WHERE false`, "1.5"); !errors.Is(err, ErrInvalidTextRepresentation) {
		t.Errorf("a fraction beside an integer: got %v, want invalid input", err)
	}
	if _, err := db.Exec(`SELECT $1 + 0.5 FROM t WHERE false`, "1.5"); err != nil {
		t.Errorf("a fraction beside a numeric: got %v, want no error", err)
	}
	// Postgres computes with the integer it binds, 2 for 5.0 / 2, while
	// the run would compute with the float.
	mustExec(t, db, `INSERT INTO t VALUES (1, 5, 1)`)
	for _, q := range []string{`SELECT $1 / 2 FROM t`, `SELECT $1::int, $1 / 2 FROM t`} {
		if _, err := db.Exec(q, float64(5)); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s with a float: got %v, want unsupported", q, err)
		}
	}
	if _, err := db.Exec(`SELECT $1 / 2.0 FROM t`, float64(5)); err != nil {
		t.Errorf("a float beside a numeric: got %v, want no error", err)
	}
	// A domain is reduced to its base type before the operator resolves,
	// so 1::tiny + 0 is an integer and takes 40000, while 1::tiny is a
	// smallint, which does not.
	mustExec(t, db, `CREATE DOMAIN tiny AS smallint`)
	mustExec(t, db, `CREATE DOMAIN amount AS numeric`)
	for _, q := range []string{
		`SELECT $1 + (1::tiny + 0) WHERE false`,
		`SELECT $1 + s.n FROM (SELECT 1::tiny + 0 AS n) s WHERE false`,
	} {
		if _, err := db.Exec(q, "40000"); err != nil {
			t.Errorf("%s: got %v, want no error", q, err)
		}
	}
	if _, err := db.Exec(`SELECT $1 + (1::amount + 0::bigint) WHERE false`, "1.5"); err != nil {
		t.Errorf("beside a numeric domain promoted with a bigint: got %v, want no error", err)
	}
	if _, err := db.Exec(`SELECT $1 + 1::tiny WHERE false`, "40000"); !errors.Is(err, ErrNumericValueOutOfRange) {
		t.Errorf("beside a smallint domain: got %v, want out of range", err)
	}
	// generate_series over a bigint domain takes the bigint overload, so
	// a bound beside it is read as a bigint.
	mustExec(t, db, `CREATE DOMAIN big AS bigint`)
	if _, err := db.Exec(`SELECT * FROM generate_series(NULL::big + 0, $1)`, int64(3000000000)); err != nil {
		t.Errorf("generate_series beside a bigint domain: got %v, want no error", err)
	}
	// Two schemas' domains of one name and other base types are one name
	// to detest, which cannot tell which base type the input is read by.
	mustExec(t, db, `CREATE SCHEMA other`)
	mustExec(t, db, `CREATE DOMAIN other.amount AS bigint`)
	if _, err := db.Exec(`SELECT $1 + (1::amount + 0) WHERE false`, "1.5"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("beside an ambiguous domain: got %v, want unsupported", err)
	}
}

// Postgres compares rows pair by pair: an ordering by the first pair that is
// not equal, as keyset pagination relies on, and each pair as two scalars.
func TestRowComparison(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE p (a int, b int, name text, PRIMARY KEY (a, b))`)
	mustExec(t, db, `INSERT INTO p VALUES (9, 1, 'x'), (10, 1, 'y'), (2, 5, 'z'), (9, 3, NULL)`)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{`SELECT a, b FROM p WHERE (a, b) > (9, 1) ORDER BY a, b`, []string{"9,3", "10,1"}},
		{`SELECT a, b FROM p WHERE (a, b) <= (9, 1) ORDER BY a, b`, []string{"2,5", "9,1"}},
		{`SELECT a, b FROM p WHERE (a, b) = ('09', 1)`, []string{"9,1"}},
		// Numbers are equal by value, so the first pair ties and the next decides.
		{`SELECT a, b FROM p WHERE (a * 1000000, b) > (9000000.0, 2) ORDER BY a, b`, []string{"9,3", "10,1"}},
		{`SELECT a, b FROM p WHERE a * 1000000 = 9000000.0 ORDER BY b`, []string{"9,1", "9,3"}},
		{`SELECT a, b FROM p WHERE (a, b) <> (9, 1) ORDER BY a, b`, []string{"2,5", "9,3", "10,1"}},
		{`SELECT a, b FROM p WHERE (a, b) IN ((2, 5), ('10', 1)) ORDER BY a`, []string{"2,5", "10,1"}},
		// A NULL pair makes = unknown unless another pair differs.
		{`SELECT a, b FROM p WHERE (a, name) = (9, NULL)`, nil},
		{`SELECT a, b FROM p WHERE NOT ((a, name) = (10, NULL)) ORDER BY a, b`, []string{"2,5", "9,1", "9,3"}},
	} {
		if got := rowsOf(t, db, tc.query); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.query, got, tc.want)
		}
	}
	// Each subquery row is compared in three-valued logic, so a NULL pair
	// makes NOT IN unknown, while a subquery with no rows is false for NULL.
	for _, tc := range []struct {
		query string
		want  string
	}{
		{`SELECT count(*) FROM p WHERE (a, name) NOT IN (SELECT 9, NULL)`, "2"},
		{`SELECT count(*) FROM p WHERE a NOT IN (SELECT NULL::int)`, "0"},
		{`SELECT count(*) FROM p WHERE NULL::int NOT IN (SELECT a FROM p WHERE a < 0)`, "4"},
		// HAVING compares a row pair by pair too, not as the text of the row.
		{`SELECT count(*) FROM p HAVING (count(*), 1) > (3, 1)`, "4"},
		{`SELECT count(*) FROM p HAVING (count(*), 1) < (10, 1)`, "4"},
	} {
		if got := rowsOf(t, db, tc.query); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: got %v, want %s", tc.query, got, tc.want)
		}
	}
	for _, q := range []string{
		`SELECT count(*) FROM p WHERE 1 IN ((1, 2))`,
		`SELECT count(*) FROM p WHERE CASE (a, b) WHEN (9, 1) THEN true ELSE false END`,
		`SELECT count(*) FROM p WHERE NULLIF((a, b), (9, 1)) IS NULL`,
		`SELECT count(*) FROM p WHERE a IN (SELECT a, b FROM p)`,
		`SELECT count(*) FROM p WHERE (name, a) = (1, 9)`,
		`SELECT count(*) FROM p WHERE (a, b) = (1, 2, 3)`,
		`SELECT count(*) FROM p WHERE (SELECT a, b FROM p LIMIT 1) = (a, b)`,
		`SELECT a FROM p ORDER BY (name, a)`,
		`SELECT (name, a) AS pair FROM p ORDER BY pair`,
		`SELECT (name, a) FROM p ORDER BY 1`,
		`SELECT a, row_number() OVER (ORDER BY (a, b)) FROM p`,
		`SELECT count(*) FROM p WHERE (a, b) = (SELECT a, b, name FROM p LIMIT 1)`,
		`SELECT count(*) FROM p WHERE (a, b) = (SELECT * FROM p LIMIT 1)`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
	}
}

// A row compared with a subquery of another width is refused before either
// side runs, as Postgres refuses it when it analyzes the statement.
func TestRowSubqueryWidthRefusedBeforeEvaluation(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE SEQUENCE sq`)
	if _, err := db.Exec(`SELECT 1 WHERE (nextval('sq'), 1) = (SELECT 1, 2, 3)`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Fatalf("got %v", err)
	}
	if got := rowsOf(t, db, `SELECT nextval('sq')`); len(got) != 1 || got[0] != "1" {
		t.Errorf("sequence advanced before the refusal: %v", got)
	}
}

// Integers above 2^53, such as snowflake IDs, are ordered exactly, as a float
// would round adjacent ones to one value.
func TestLargeIntegersCompareExactly(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int8 PRIMARY KEY)`)
	mustExec(t, db, `INSERT INTO t VALUES ($1), ($2)`, int64(1<<53), int64(1<<53+1))
	for _, tc := range []struct {
		query string
		args  []any
	}{
		{`SELECT id FROM t WHERE id > $1`, []any{int64(1 << 53)}},
		{`SELECT id FROM t ORDER BY id DESC LIMIT 1`, nil},
		// An integer and a float compare exactly, even above 2^53.
		{`SELECT id FROM t WHERE id > $1`, []any{float64(1 << 53)}},
	} {
		if got := rowsOf(t, db, tc.query, tc.args...); !reflect.DeepEqual(got, []string{"9007199254740993"}) {
			t.Errorf("%s: got %v", tc.query, got)
		}
	}
}

// A cast to a number reads text as Postgres's input function does and rounds
// a fraction, so cast values compare as numbers.
func TestCastToNumber(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	for _, tc := range []struct {
		query string
		args  []any
		want  string
	}{
		{`SELECT $1::int > $2::int`, []any{"10", "9"}, "true"},
		{`SELECT ' 01 '::int = 1`, nil, "true"},
		{`SELECT '1.50'::numeric = 1.5`, nil, "true"},
		{`SELECT 1.6::int`, nil, "2"},
		{`SELECT (-1.4)::smallint`, nil, "-1"},
		{`SELECT (1.5 * 2) / 2`, nil, "1.5"},
		{`SELECT true::int + false::int`, nil, "1"},
		{`SELECT floor(1.5) / 2`, nil, "0.5"},
		{`SELECT floor(3) / 2`, nil, "1.5"},
		{`SELECT round(3) / 2`, nil, "1.5"},
		{`SELECT abs(-3) / 2`, nil, "1"},
		{`SELECT -(1.5 * 2) / 2`, nil, "-1.5"},
	} {
		if got := rowsOf(t, db, tc.query, tc.args...); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: got %v, want %s", tc.query, got, tc.want)
		}
	}
	for _, q := range []string{`SELECT 'abc'::int`, `SELECT 'not_a_number'::int`} {
		if _, err := db.Exec(q); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%s: got %v, want invalid input syntax", q, err)
		}
	}
	var b any
	if err := db.QueryRow(`SELECT $1::bytea`, []byte("ab")).Scan(&b); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.([]byte); !ok {
		t.Errorf("$1::bytea: got %T, want []byte", b)
	}
	for _, q := range []string{`SELECT '40000'::smallint`, `SELECT 3000000000::int`} {
		if _, err := db.Exec(q); !errors.Is(err, ErrNumericValueOutOfRange) {
			t.Errorf("%s: got %v, want out of range", q, err)
		}
	}
	mustExec(t, db, `CREATE TABLE bin (id int PRIMARY KEY, b bytea)`)
	mustExec(t, db, `INSERT INTO bin VALUES (1, $1)`, []byte("10"))
	for _, q := range []string{`SELECT b::int FROM bin`, `SELECT b::text FROM bin`} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
	}
	if got := rowsOf(t, db, `SELECT $1::int`, []byte("10")); !reflect.DeepEqual(got, []string{"10"}) {
		t.Errorf("$1::int with bytes: got %v", got)
	}
	for _, q := range []string{`SELECT $1::numeric`, `SELECT $1::float8`} {
		if _, err := db.Exec(q, math.NaN()); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s with NaN: got %v, want unsupported", q, err)
		}
	}
	// A numeric rounds half away from zero and a float half to even, told
	// apart by the expression's form; a parameter's is unknown.
	if got := rowsOf(t, db, `SELECT 2.5::int, (-2.5)::int, 2.5::float8::int, (0.5 * 5)::int, round(2.5), round(2.5::float8)`); !reflect.DeepEqual(got, []string{"3,-3,2,3,3,2"}) {
		t.Errorf("casts and round of halves: got %v", got)
	}
	for _, q := range []string{`SELECT $1::int`, `SELECT round($1)`} {
		if _, err := db.Exec(q, 2.5); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s with 2.5: got %v, want unsupported", q, err)
		}
	}
	for _, q := range []string{`SELECT 'NaN'::float8`, `SELECT '0.12345678901234567890'::numeric`, `SELECT 9007199254740993::numeric`,
		`SELECT CURRENT_TIMESTAMP::int`, `SELECT CURRENT_TIMESTAMP::numeric`, `SELECT true::float8`,
		`SELECT true::bigint`, `SELECT true::smallint`} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
	}
}

// Equal numbers have one key, so a unique index or a row lock on a value
// written once as an integer and once as a float is one entry.
func TestNumberKeysByValue(t *testing.T) {
	for _, tc := range []struct{ a, b any }{
		{int64(1000000), float64(1e6)},
		{int64(0), math.Copysign(0, -1)},
		{int64(9007199254740994), float64(9007199254740994)},
		{int64(math.MinInt64), float64(math.MinInt64)},
	} {
		if ka, kb := keyString(tc.a), keyString(tc.b); ka != kb {
			t.Errorf("keyString(%v) = %q, keyString(%v) = %q", tc.a, ka, tc.b, kb)
		}
	}
	if keyString(1.5) == keyString(int64(1)) || keyString(1.5) != "1.5" {
		t.Errorf("keyString(1.5) = %q", keyString(1.5))
	}
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	if got := rowsOf(t, db, `SELECT '-0.0'::numeric`); !reflect.DeepEqual(got, []string{"0"}) {
		t.Errorf("'-0.0'::numeric: got %v", got)
	}
}

// Values Postgres converts by a type detest does not keep are refused, and
// the ones the statement or the schema tells are converted as Postgres does.
func TestValueFormsByType(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, ratio float8, name text, v varchar(3), c char(3), price numeric(4,1), b bytea, active bool)`)
	mustExec(t, db, `INSERT INTO t (id, ratio, name, b, active) VALUES (1, 0.5, 'a', $1, true)`, []byte("10"))
	for _, q := range []string{
		`SELECT 1 || 2`, `SELECT 'a' || 1.5`, `SELECT 1.5::text`, `SELECT ratio::text FROM t`,
		`SELECT coalesce(name, 1) FROM t`, `SELECT CASE WHEN id = 1 THEN name ELSE 2 END FROM t`, `SELECT greatest(name, 1) FROM t`,
		`SELECT id FROM t UNION SELECT '1'`, `SELECT id FROM t INTERSECT SELECT '1'`,
		`SELECT round(ratio) FROM t`, `INSERT INTO t (id, c) VALUES (2, 'ab')`, `INSERT INTO t (id, c) VALUES (2, 'a  ')`,
		`SELECT true UNION SELECT 1`, `SELECT id FROM t UNION SELECT NULL UNION SELECT now()`,
		`SELECT true || false`, `SELECT 1 || true`, `SELECT CASE WHEN true THEN '2'::text ELSE 1 END`,
		`SELECT '' || now()`, `SELECT now()::text`, `SELECT CASE WHEN true THEN 1 ELSE 'x'::text END`,
		`ALTER TABLE t ALTER COLUMN v TYPE int USING length(v)`,
		`SELECT b || 'x' FROM t`, `SELECT id FROM t WHERE name = 'a '`, `SELECT id FROM t WHERE 'a ' IN (name)`,
		`SELECT 1::bigint::bool`, `SELECT id FROM t WHERE (id, name) > (0, 1)`, `SELECT id FROM t WHERE (id, name) = (0, 1)`,
		`CREATE TABLE n (id int PRIMARY KEY, v numeric(2, -3))`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
	}
	for _, tc := range []struct {
		q    string
		want error
	}{
		{`INSERT INTO t (id, v) VALUES (2, 'abcd')`, ErrStringDataRightTruncation},
		{`INSERT INTO t (id, c) VALUES (2, 'abcd')`, ErrStringDataRightTruncation},
		{`INSERT INTO t (id, v) VALUES (2, 1234)`, ErrStringDataRightTruncation},
		{`INSERT INTO t (id, price) VALUES (2, 999.95)`, ErrNumericValueOutOfRange},
		{`SELECT 'o'::bool`, ErrInvalidTextRepresentation},
		{`SELECT CASE WHEN false THEN 1 ELSE '1.5' END`, ErrInvalidTextRepresentation},
		{`SELECT CASE WHEN true THEN 1 ELSE 'x' END`, ErrInvalidTextRepresentation},
	} {
		if _, err := db.Exec(tc.q); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.q, err, tc.want)
		}
	}
	mustExec(t, db, `INSERT INTO t (id, v, c, price) VALUES (2, 'abc ', 'abc  ', 1.05)`)
	got := rowsOf(t, db, `SELECT v, c, price = 1.1, 'yes'::bool, 2::bool, CASE WHEN id = 2 THEN 1 ELSE '2' END = 1 FROM t WHERE id = 2`)
	if want := []string{"abc,abc,true,true,true,true"}; !reflect.DeepEqual(got, want) {
		t.Errorf("converted values: got %v, want %v", got, want)
	}
	if got := rowsOf(t, db, `SELECT (CASE WHEN true THEN 1 ELSE random() END) / 2 = 0.5, $1::bool, $2::bool, 'a' || $3`, int64(1), int64(0), []byte("b")); !reflect.DeepEqual(got, []string{"true,true,false,ab"}) {
		t.Errorf("random branch, boolean parameters and bytes concatenated: got %v", got)
	}
	// A column's type shows in its value, so a float there types the
	// integer of another branch.
	if got := rowsOf(t, db, `SELECT (CASE WHEN true THEN 1 ELSE ratio END) / 2 = 0.5, COALESCE(1, ratio) / 2 = 0.5, (CASE WHEN true THEN 1 ELSE id END) / 2 FROM t WHERE id = 1`); !reflect.DeepEqual(got, []string{"true,true,0"}) {
		t.Errorf("branches typed by a column's value: got %v", got)
	}
	if _, err := db.Exec(`SELECT $1::bool`, int64(2)); !errors.Is(err, ErrInvalidTextRepresentation) {
		t.Errorf("$1::bool with 2: got %v, want invalid input", err)
	}
	if got := rowsOf(t, db, `SELECT count(*) FROM t WHERE active = $1`, int64(1)); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("active = $1 with 1: got %v", got)
	}
	if _, err := db.Exec(`SELECT count(*) FROM t WHERE active = $1`, int64(2)); !errors.Is(err, ErrInvalidTextRepresentation) {
		t.Errorf("active = $1 with 2: got %v, want invalid input", err)
	}
	// ALTER COLUMN TYPE applies the new limits to the rows and the writes
	// after it.
	mustExec(t, db, `ALTER TABLE t ALTER COLUMN price TYPE numeric(4,2)`)
	mustExec(t, db, `ALTER TABLE t ALTER COLUMN v TYPE varchar(4)`)
	mustExec(t, db, `INSERT INTO t (id, price, v) VALUES (3, 1.005, 'abcd')`)
	if got := rowsOf(t, db, `SELECT price = 1.01 FROM t WHERE id = 3`); !reflect.DeepEqual(got, []string{"true"}) {
		t.Errorf("after ALTER TYPE: got %v", got)
	}
	if _, err := db.Exec(`INSERT INTO t (id, v) VALUES (4, 'abcde')`); !errors.Is(err, ErrStringDataRightTruncation) {
		t.Errorf("varchar(4) after ALTER TYPE: got %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE t ALTER COLUMN v TYPE varchar(3)`); !errors.Is(err, ErrStringDataRightTruncation) {
		t.Errorf("ALTER TYPE over a longer row: got %v", err)
	}
}

func TestDuplicateColumnAliasIsUnsupported(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	if _, err := db.Query(`SELECT * FROM (SELECT 1, 2) s(a, a)`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("got %v", err)
	}
}

func TestWholeRowValueIsUnsupported(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY)`)
	if _, err := db.Query(`SELECT t FROM t`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("got %v", err)
	}
}

func TestOutputNameAndPositionFormsAreUnsupported(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, a int, b int)`)
	for _, q := range []string{
		`SELECT a AS x, b AS x FROM t ORDER BY x`,
		`SELECT a AS x, b AS x FROM t GROUP BY x`,
		`SELECT DISTINCT ON (x) a AS x, b AS x FROM t`,
		`SELECT *, count(*) FROM t GROUP BY 1, 2, 3`,
		`SELECT s.a FROM (SELECT 1 AS a, 2 AS a) s`,
		`SELECT a FROM (SELECT 1 AS a, 2 AS a) s`,
		`SELECT k FROM t AS x(k)`,
		`WITH w AS (SELECT id FROM t) SELECT k FROM w AS x(k)`,
		`WITH c(x) AS (SELECT id FROM t) SELECT x FROM c`,
		`SELECT x.id FROM t AS x JOIN t AS x ON true`,
		`SELECT public.t.* FROM public.t`,
		`SELECT *, id + 1 AS id FROM t ORDER BY id`,
		`SELECT a FROM generate_series(1, 2) AS s(a, a)`,
		`SELECT s.a FROM (SELECT 1 AS a, 2 AS a, 3 AS a) s`,
		`SELECT a AS x, b AS x, id AS x FROM t ORDER BY x`,
		`SELECT a FROM (SELECT 1 AS a, 2 AS a) s, generate_series(1, 1) g`,
		`UPDATE public.t AS x SET a = 1 RETURNING public.t.*`,
		`UPDATE t SET a = 1 FROM t AS u WHERE u.id = t.id RETURNING u.*`,
	} {
		if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v", q, err)
		}
	}
	mustExec(t, db, `SELECT * FROM t ORDER BY 2`)
}

func TestColumnCheckWithoutSchema(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	if _, err := db.Query(`SELECT typo FROM (SELECT 1 AS a) s`); !errors.Is(err, ErrUndefinedColumn) {
		t.Errorf("a derived table's columns are known without a schema: %v", err)
	}
}

func TestUndeclaredSequenceIsUndefined(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY)`)
	for _, q := range []string{`SELECT nextval('missing')`, `SELECT setval('missing', 3)`} {
		if _, err := db.Exec(q); !errors.Is(err, ErrUndefinedTable) {
			t.Errorf("%s: %v", q, err)
		}
	}
}

func TestInsertColumnChecks(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, a int)`)
	if _, err := db.Exec(`INSERT INTO t (a, a) VALUES (1, 2)`); !errors.Is(err, ErrDuplicateColumn) {
		t.Errorf("duplicate INSERT target: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO missing VALUES (typo)`); !errors.Is(err, ErrUndefinedTable) {
		t.Errorf("missing table first: %v", err)
	}
	if _, err := db.Exec(`WITH c AS (SELECT 1 AS x) INSERT INTO t SELECT 5, x FROM c RETURNING (SELECT x FROM c)`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("CTE read by RETURNING: %v", err)
	}
	mustExec(t, db, `WITH c AS (SELECT 1 AS x) INSERT INTO t SELECT 6, x FROM c RETURNING id`)
}

func TestCorrelatedCTEInSubqueryUnsupported(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE u (id int PRIMARY KEY)`)
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, v int)`)
	for _, q := range []string{
		`SELECT (WITH c AS (SELECT u.id AS v) SELECT (SELECT v FROM c)) FROM u`,
		`SELECT (WITH c AS (SELECT u.id AS v), d AS (SELECT v FROM c) SELECT (SELECT v FROM d)) FROM u`,
		`SELECT (WITH c AS (SELECT u.id AS v) SELECT (WITH c AS (SELECT v FROM c) SELECT v FROM c)) FROM u`,
		`SELECT (WITH unused AS (SELECT u.id) SELECT v FROM t WHERE id = 1) FROM u`,
		`SELECT (WITH a AS (SELECT u.id), unused AS (SELECT * FROM a) SELECT v FROM t WHERE id = 1) FROM u`,
		`SELECT id FROM u WHERE EXISTS (WITH c AS (SELECT 1 FROM t WHERE t.id = u.id) SELECT 1 FROM c)`,
		`UPDATE u SET id = id WHERE id = (WITH c AS (SELECT u.id AS v) SELECT v FROM c)`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) || !strings.Contains(err.Error(), "reading a column of a query around") {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL for the CTE", q, err)
		}
	}
	for _, q := range []string{
		`SELECT (WITH c AS (SELECT 5 AS v) SELECT (SELECT v FROM c) + u.id) FROM u`,
		`SELECT (WITH c AS (SELECT v FROM t WHERE id = 1) SELECT v FROM c) FROM u`,
		`WITH c AS (SELECT id FROM u) SELECT (SELECT count(*) FROM c WHERE c.id = u.id) FROM u`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}

func TestUnknownColumnOwnerUnsupported(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE u (id int PRIMARY KEY)`)
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, v int, n int)`)
	mustExec(t, db, `CREATE TABLE v (n int)`)
	mustExec(t, db, `CREATE VIEW vv AS SELECT * FROM v`)
	mustExec(t, db, `INSERT INTO u VALUES (1), (2)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 10, 1)`)
	mustExec(t, db, `INSERT INTO v VALUES (7)`)
	for _, q := range []string{
		`SELECT (SELECT id FROM vv) FROM u`,
		`SELECT (SELECT t.v FROM t WHERE t.id = 1 AND EXISTS (SELECT 1 FROM vv WHERE n IS NOT NULL)) FROM u`,
		`SELECT (WITH c AS (SELECT id FROM vv) SELECT id FROM c) FROM u`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) || !strings.Contains(err.Error(), "may have") && !strings.Contains(err.Error(), "around the one declaring it") {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL for the column's owner", q, err)
		}
	}
	for _, q := range []string{
		`SELECT (SELECT n FROM vv) FROM u`,
		`SELECT (SELECT t.v FROM t WHERE t.id = 1 AND EXISTS (SELECT 1 FROM vv WHERE vv.n IS NOT NULL)) FROM u`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}
