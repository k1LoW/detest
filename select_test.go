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

	"github.com/k1LoW/detest/postgres"
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
		`SELECT count(*) FROM t WHERE now() > '2024-01-01'`,
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
	mustExec(t, db, `INSERT INTO e VALUES (2, $1, $1, true, 'true')`, at)
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
		{`INSERT INTO e (id, at) VALUES (4, '2024-01-01 00:00:00')`, nil},
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
	} {
		if got := rowsOf(t, db, tc.query); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: got %v, want %s", tc.query, got, tc.want)
		}
	}
	for _, q := range []string{
		`SELECT count(*) FROM p WHERE 1 IN ((1, 2))`,
		`SELECT count(*) FROM p HAVING (count(*), 1) > (3, 1)`,
		`SELECT count(*) FROM p WHERE CASE (a, b) WHEN (9, 1) THEN true ELSE false END`,
		`SELECT count(*) FROM p WHERE NULLIF((a, b), (9, 1)) IS NULL`,
		`SELECT count(*) FROM p WHERE a IN (SELECT a, b FROM p)`,
		`SELECT count(*) FROM p WHERE (name, a) = (1, 9)`,
		`SELECT count(*) FROM p WHERE (a, b) = (1, 2, 3)`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
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
