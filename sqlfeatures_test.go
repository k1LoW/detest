package detest

import (
	"database/sql"
	"errors"
	"fmt"
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
	if err := CheckSQL(mysql.New(), "SELECT 1"); err != nil {
		t.Fatalf("mysql: %v", err)
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
	// The table's own key, constraints and generated columns read the
	// column, and would be decided from the marker, so the write is refused.
	for _, ddl := range []string{
		`CREATE TABLE e (id int PRIMARY KEY, d text DEFAULT CURRENT_DATE CHECK (d <> 'x'))`,
		`CREATE TABLE e (id int PRIMARY KEY, d text DEFAULT CURRENT_DATE UNIQUE)`,
		`CREATE TABLE e (id int PRIMARY KEY, d text DEFAULT CURRENT_DATE, u text GENERATED ALWAYS AS (upper(d)) STORED)`,
		`CREATE TABLE e (id int PRIMARY KEY, d text NOT NULL DEFAULT CURRENT_DATE)`,
		`CREATE TABLE e (id int PRIMARY KEY, d uuid DEFAULT CURRENT_USER::uuid)`,
		`CREATE TABLE e (id int PRIMARY KEY, d smallint DEFAULT length(CURRENT_USER))`,
		`CREATE TABLE e (id int PRIMARY KEY, d int DEFAULT length(CURRENT_USER) REFERENCES c (id))`,
	} {
		mustExec(t, db, ddl)
		if _, err := db.Exec(`INSERT INTO e (id) VALUES (1)`); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: insert leaving out a default that a key, constraint or generated column reads: got %v", ddl, err)
		}
		mustExec(t, db, `DROP TABLE e`)
	}
	// SET DEFAULT in a referential action always writes a foreign key
	// column, so a default detest cannot compute is refused there, and the
	// child keeps its row.
	mustExec(t, db, `CREATE TABLE p (id int PRIMARY KEY)`)
	mustExec(t, db, `CREATE TABLE k (id int PRIMARY KEY, pid int DEFAULT length(CURRENT_USER) REFERENCES p (id) ON DELETE SET DEFAULT)`)
	mustExec(t, db, `INSERT INTO p VALUES (1)`)
	mustExec(t, db, `INSERT INTO k VALUES (1, 1)`)
	if _, err := db.Exec(`DELETE FROM p WHERE id = 1`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("ON DELETE SET DEFAULT with a default detest cannot compute: got %v", err)
	}
	var pid, parents int64
	if err := db.QueryRow(`SELECT pid, (SELECT count(*) FROM p) FROM k WHERE id = 1`).Scan(&pid, &parents); err != nil || pid != 1 || parents != 1 {
		t.Errorf("after the refused delete: pid=%d parents=%d err=%v; want both unchanged", pid, parents, err)
	}
}

// A column rename reaches every reference of an expression or partial
// unique index, not only a bare column, so the index still constrains the
// renamed column and still counts as reading its default.
func TestRenameColumnInUniqueExpression(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE u (id int PRIMARY KEY, d text, live bool)`)
	mustExec(t, db, `CREATE UNIQUE INDEX u_lower_d ON u (lower(d)) WHERE live`)
	mustExec(t, db, `ALTER TABLE u RENAME COLUMN d TO x`)
	mustExec(t, db, `ALTER TABLE u RENAME COLUMN live TO active`)
	mustExec(t, db, `INSERT INTO u VALUES (1, 'A', true)`)
	if _, err := db.Exec(`INSERT INTO u VALUES (2, 'a', true)`); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("duplicate under the renamed expression index: got %v", err)
	}
	mustExec(t, db, `INSERT INTO u VALUES (3, 'a', false)`)

	mustExec(t, db, `CREATE TABLE v (id int PRIMARY KEY, d text DEFAULT CURRENT_DATE)`)
	mustExec(t, db, `CREATE UNIQUE INDEX v_lower_d ON v (lower(d))`)
	mustExec(t, db, `ALTER TABLE v RENAME COLUMN d TO x`)
	if _, err := db.Exec(`INSERT INTO v (id) VALUES (1)`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("default read by a renamed expression index: got %v", err)
	}
}

// A rename in one database leaves the unique index of another database
// built from the same statements alone, as the parsed statements they
// share are not rewritten.
func TestRenameColumnLeavesOtherDatabases(t *testing.T) {
	s := newSim(t)
	a, _ := s.DB("a", postgres.New())
	b, _ := s.DB("b", postgres.New())
	for _, db := range []*sql.DB{a, b} {
		mustExec(t, db, `CREATE TABLE shared_u (id int PRIMARY KEY, d text)`)
		mustExec(t, db, `CREATE UNIQUE INDEX shared_u_d ON shared_u (d)`)
		mustExec(t, db, `CREATE UNIQUE INDEX shared_u_lower_d ON shared_u (lower(d))`)
	}
	mustExec(t, a, `ALTER TABLE shared_u RENAME COLUMN d TO x`)
	mustExec(t, b, `INSERT INTO shared_u VALUES (1, 'A')`)
	for _, q := range []string{`INSERT INTO shared_u VALUES (2, 'A')`, `INSERT INTO shared_u VALUES (3, 'a')`} {
		if _, err := b.Exec(q); !errors.Is(err, ErrUniqueViolation) {
			t.Errorf("%s in the database that renamed nothing: got %v", q, err)
		}
	}
}

// SET col = DEFAULT writes the column's default, in UPDATE and in ON
// CONFLICT DO UPDATE, and refuses one detest cannot compute that the
// table's schema reads, as an INSERT that leaves the column out does.
func TestSetDefault(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE sd (id int PRIMARY KEY, n int DEFAULT 7, m int, d text DEFAULT CURRENT_DATE UNIQUE)`)
	mustExec(t, db, `INSERT INTO sd VALUES (1, 1, 1, 'x')`)
	mustExec(t, db, `UPDATE sd SET n = DEFAULT, m = DEFAULT WHERE id = 1`)
	var n int64
	var m sql.NullInt64
	if err := db.QueryRow(`SELECT n, m FROM sd WHERE id = 1`).Scan(&n, &m); err != nil || n != 7 || m.Valid {
		t.Errorf("UPDATE SET DEFAULT: n=%d m=%v err=%v; want 7 and NULL", n, m, err)
	}
	mustExec(t, db, `UPDATE sd SET n = 1 WHERE id = 1`)
	mustExec(t, db, `INSERT INTO sd VALUES (1, 2, 2, 'y') ON CONFLICT (id) DO UPDATE SET n = DEFAULT`)
	if err := db.QueryRow(`SELECT n FROM sd WHERE id = 1`).Scan(&n); err != nil || n != 7 {
		t.Errorf("ON CONFLICT DO UPDATE SET DEFAULT: n=%d err=%v; want 7", n, err)
	}
	for _, q := range []string{
		`UPDATE sd SET d = DEFAULT WHERE id = 1`,
		`INSERT INTO sd VALUES (1, 2, 2, 'y') ON CONFLICT (id) DO UPDATE SET d = DEFAULT`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v", q, err)
		}
	}
}

// A default that is the literal Unknown string is a default like any other,
// not one detest failed to convert.
func TestLiteralUnknownDefault(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE lu (id int PRIMARY KEY, d text DEFAULT '<unknown expression>' UNIQUE)`)
	mustExec(t, db, `INSERT INTO lu (id) VALUES (1)`)
	if _, err := db.Exec(`INSERT INTO lu (id) VALUES (2)`); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("second row with the literal default: got %v", err)
	}
}

// A table without a primary key tells its rows apart by their values, so a
// second equal row, whether written or from a default detest stood in for,
// is refused rather than reported as a duplicate key.
func TestEqualRowsWithoutPrimaryKey(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE nk (n int, d bigint DEFAULT txid_current())`)
	mustExec(t, db, `INSERT INTO nk (n) VALUES (1)`)
	mustExec(t, db, `INSERT INTO nk VALUES (2, 5)`)
	for _, q := range []string{`INSERT INTO nk (n) VALUES (1)`, `INSERT INTO nk VALUES (2, 5)`} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v", q, err)
		}
	}
	// A unique constraint that rejects the equal row still does, as on the
	// server.
	mustExec(t, db, `CREATE TABLE nku (a int UNIQUE)`)
	mustExec(t, db, `INSERT INTO nku VALUES (1)`)
	if _, err := db.Exec(`INSERT INTO nku VALUES (1)`); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("equal row under a unique constraint: got %v", err)
	}
	// An equal row another transaction is writing is refused without
	// waiting on it, as Postgres has no key to wait on.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO nk VALUES (3, 3)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO nk VALUES (3, 3)`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("equal row another transaction is writing: got %v", err)
	}
}

// An equal row a unique index takes waits for the transaction writing the
// other one, as Postgres waits on the index entry, and then fails as a
// unique violation once that transaction commits.
// In a table without a primary key whose rows detest keys by an id column,
// a row another transaction is writing with the same id but other unique
// values is refused rather than waited on, as Postgres would not wait.
func TestEqualIDWithoutPrimaryKeyDoesNotWait(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE nki (id int, u int UNIQUE)`)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO nki VALUES (1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO nki VALUES (1, 2)`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("same id, other unique value, another transaction writing: got %v", err)
	}
}

func TestEqualRowUnderUniqueWaits(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE nkw (a int UNIQUE)`)
		var errs []error
		s.Seed(func() { errs = nil })
		insert := func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`INSERT INTO nkw VALUES (1)`); err != nil {
				errs = append(errs, err)
				return nil
			}
			return tx.Commit()
		}
		s.Manual("x", 1, insert)
		s.Manual("y", 1, insert)
		s.AtQuiescence(func(*State) error {
			if len(errs) != 1 {
				return fmt.Errorf("%d inserts failed, want 1: %v", len(errs), errs)
			}
			if !errors.Is(errs[0], ErrUniqueViolation) {
				return errs[0]
			}
			return nil
		})
	})
}
