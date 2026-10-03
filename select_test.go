package detest

import (
	"database/sql"
	"errors"
	"fmt"
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
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v", q, err)
		}
	}
	var n int64
	if err := db.QueryRow(`SELECT count(*) + 1 FROM t`).Scan(&n); err != nil || n != 2 {
		t.Errorf("count(*) + 1: %d, %v", n, err)
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
