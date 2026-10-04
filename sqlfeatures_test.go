package detest

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/k1LoW/detest/mysql"
	"github.com/k1LoW/detest/postgres"
)

// The statements below run outside any process (no run, no yields), which
// exercises the executor directly through database/sql.
func TestSQLExecutorFeatures(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	exec := func(q string, args ...any) sql.Result {
		t.Helper()
		res, err := db.Exec(q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return res
	}
	queryInts := func(q string, args ...any) []int64 {
		t.Helper()
		rows, err := db.Query(q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("%s: scan: %v", q, err)
			}
			out = append(out, v)
		}
		return out
	}
	queryStrings := func(q string, args ...any) []string {
		t.Helper()
		rows, err := db.Query(q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("%s: scan: %v", q, err)
			}
			out = append(out, v)
		}
		return out
	}

	exec(`INSERT INTO "customers" ("id","name") VALUES ($1,$2)`, "w1", "alpha")
	exec(`INSERT INTO "customers" ("id","name") VALUES ($1,$2)`, "w2", "beta")
	exec(`INSERT INTO "orders" ("id","customer_id","status") VALUES ($1,$2,$3)`, "e1", "w1", int64(2))
	exec(`INSERT INTO "orders" ("id","customer_id","status") VALUES ($1,$2,$3)`, "e2", "w1", int64(3))
	exec(`INSERT INTO "orders" ("id","customer_id","status") VALUES ($1,$2,$3)`, "e3", "w2", int64(2))

	// JOIN with aliases and qualified columns.
	if got := queryStrings(`SELECT w.name FROM "orders" e JOIN "customers" w ON w.id = e.customer_id WHERE e.status = $1 ORDER BY w.name`, int64(2)); len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("join: %v", got)
	}
	// LEFT JOIN keeps rows without a match.
	exec(`INSERT INTO "customers" ("id","name") VALUES ($1,$2)`, "w3", "gamma")
	if got := queryStrings(`SELECT w.name FROM "customers" w LEFT JOIN "orders" e ON e.customer_id = w.id WHERE e.id IS NULL`); len(got) != 1 || got[0] != "gamma" {
		t.Fatalf("left join: %v", got)
	}
	// GROUP BY with HAVING and COUNT.
	if got := queryStrings(`SELECT customer_id FROM "orders" GROUP BY customer_id HAVING COUNT(*) > 1`); len(got) != 1 || got[0] != "w1" {
		t.Fatalf("having: %v", got)
	}
	// CTE feeding the main query, and a scalar subquery.
	if got := queryInts(`WITH running AS (SELECT customer_id FROM "orders" WHERE status = $1) SELECT COUNT(*) FROM running`, int64(2)); len(got) != 1 || got[0] != 2 {
		t.Fatalf("cte: %v", got)
	}
	if got := queryInts(`SELECT (SELECT COUNT(*) FROM "orders" WHERE status = $1) + (SELECT COUNT(*) FROM "customers")`, int64(3)); got[0] != 4 {
		t.Fatalf("scalar subqueries: %v", got)
	}
	// ON CONFLICT DO UPDATE with EXCLUDED and arithmetic, as the reconcile
	// attempt counter does.
	exec(`INSERT INTO "attempts" AS a ("key","attempts","last_at") VALUES ($1,$2,$3) ON CONFLICT ("key") DO UPDATE SET attempts = a.attempts + 1, last_at = EXCLUDED.last_at`, "k", int64(1), time.Unix(100, 0))
	exec(`INSERT INTO "attempts" AS a ("key","attempts","last_at") VALUES ($1,$2,$3) ON CONFLICT ("key") DO UPDATE SET attempts = a.attempts + 1, last_at = EXCLUDED.last_at WHERE a.attempts < $4`, "k", int64(1), time.Unix(200, 0), int64(5))
	if got := queryInts(`SELECT attempts FROM "attempts" WHERE "key" = $1`, "k"); got[0] != 2 {
		t.Fatalf("on conflict do update: %v", got)
	}
	// The conditional update is skipped when WHERE is false.
	exec(`INSERT INTO "attempts" AS a ("key","attempts","last_at") VALUES ($1,$2,$3) ON CONFLICT ("key") DO UPDATE SET attempts = a.attempts + 1 WHERE a.attempts < $4`, "k", int64(1), time.Unix(300, 0), int64(2))
	if got := queryInts(`SELECT attempts FROM "attempts" WHERE "key" = $1`, "k"); got[0] != 2 {
		t.Fatalf("on conflict do update where: %v", got)
	}
	// Interval arithmetic with make_interval and LEAST/power, as in backoff SQL.
	if got := queryInts(`SELECT COUNT(*) FROM "attempts" a WHERE a.last_at <= $1::timestamptz - make_interval(secs => $2::double precision * power(2, LEAST(a.attempts - 1, 6)))`, time.Unix(1000, 0), 5.0); got[0] != 1 {
		t.Fatalf("interval arithmetic: %v", got)
	}
	if got := queryInts(`SELECT COUNT(*) FROM "attempts" a WHERE a.last_at <= $1::timestamptz - make_interval(secs => $2::double precision)`, time.Unix(205, 0), 10.0); got[0] != 0 {
		t.Fatalf("interval arithmetic (too recent): %v", got)
	}
	// UPDATE ... FROM and DELETE ... USING.
	res := exec(`UPDATE "orders" SET status = $1 FROM "customers" w WHERE w.id = "orders".customer_id AND w.name = $2`, int64(9), "alpha")
	if n, _ := res.RowsAffected(); n != 2 {
		t.Fatalf("update from affected %d", n)
	}
	res = exec(`DELETE FROM "orders" USING "customers" w WHERE w.id = "orders".customer_id AND w.name = $1`, "beta")
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("delete using affected %d", n)
	}
	if got := queryInts(`SELECT COUNT(*) FROM "orders"`); got[0] != 2 {
		t.Fatalf("after delete: %v", got)
	}
	// CASE and DISTINCT.
	if got := queryStrings(`SELECT DISTINCT CASE WHEN status = $1 THEN 'done' ELSE 'other' END FROM "orders"`, int64(9)); len(got) != 1 || got[0] != "done" {
		t.Fatalf("case/distinct: %v", got)
	}
	// RETURNING on UPDATE.
	if got := queryStrings(`UPDATE "customers" SET name = $1 WHERE id = $2 RETURNING name`, "ALPHA", "w1"); len(got) != 1 || got[0] != "ALPHA" {
		t.Fatalf("returning: %v", got)
	}
	// The MySQL dialect exists as an interface but has no frontend yet.
	if err := CheckSQL(mysql.New(), "SELECT 1"); err == nil {
		t.Fatal("mysql frontend should report not implemented")
	}
}

// An expression detest cannot evaluate where the application observes its
// value is refused, so that no placeholder is written in place of the value
// and no sort key is silently dropped.
func TestUnevaluableExpressionIsUnsupported(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, v text)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'a')`)
	for _, q := range []string{
		`INSERT INTO t VALUES (2, now() * 2)`,
		`UPDATE t SET v = now() * 2 WHERE id = 1`,
		`INSERT INTO t VALUES (1, 'b') ON CONFLICT (id) DO UPDATE SET v = now() * 2`,
		`SELECT id FROM t ORDER BY now() * 2`,
		`SELECT id, now() * 2 FROM t`,
		`INSERT INTO t VALUES (2, 'b') RETURNING now() * 2`,
		`SELECT id FROM t WHERE now() * 2 IS NULL`,
		`SELECT id FROM t WHERE id = 1 AND now() * 2 IS NULL FOR UPDATE`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v", q, err)
		}
	}
	var n int64
	var v string
	if err := db.QueryRow(`SELECT count(*), min(v) FROM t`).Scan(&n, &v); err != nil || n != 1 || v != "a" {
		t.Fatalf("got %d rows, v=%q, err=%v; want the table untouched", n, v, err)
	}
}

// A CHECK detest cannot convert loads with the schema, and a write to its
// table is refused rather than checked against nothing. A default detest
// cannot convert loads as well, and a write that leaves the column out
// stores the Unknown marker, so the INSERTs a dump's tables take still run.
func TestUnconvertedSchemaExpressions(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE c (id int PRIMARY KEY, v text, CONSTRAINT c_v_check CHECK (v <> CURRENT_USER))`)
	if _, err := db.Exec(`INSERT INTO c VALUES (1, 'a')`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("insert into a table with an unconverted CHECK: got %v", err)
	}
	mustExec(t, db, `CREATE TABLE d (id int PRIMARY KEY, tags text[] DEFAULT ARRAY[]::text[])`)
	mustExec(t, db, `INSERT INTO d (id) VALUES (1)`)
	var tags string
	if err := db.QueryRow(`SELECT tags FROM d WHERE id = 1`).Scan(&tags); err != nil || tags != Unknown {
		t.Errorf("default detest cannot convert: got %q, %v; want the Unknown marker", tags, err)
	}
}
