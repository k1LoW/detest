package detest

import (
	"database/sql"
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
