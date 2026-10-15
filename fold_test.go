package detest

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// Postgres folds an expression of constants when it plans the statement,
// so one that fails does so before any row is read.
func TestFoldConstantsBeforeAnyRow(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 0)`)
	mustExec(t, db, `CREATE VIEW fv AS SELECT CASE WHEN n > 0 THEN 1 / 0 END AS v FROM t`)
	for _, q := range []string{
		`SELECT CASE WHEN n = 0 THEN 1 ELSE 1 / 0 END FROM t`,
		`SELECT n + 1 / 0 FROM t WHERE false`,
		`SELECT coalesce(n, 1 / 0) FROM t`,
		`SELECT sum(1 / 0) FROM t WHERE false`,
		`SELECT n + (SELECT 1 / 0) FROM t WHERE false`,
		`SELECT 1 FROM t WHERE 1 / 0 = 1 AND false`,
		`SELECT coalesce(n, extract(year FROM now()), 1 / 0) FROM t`,
		`SELECT n FROM t LIMIT 1 / 0`,
	} {
		if _, err := db.Query(q); err == nil || !strings.Contains(err.Error(), "division by zero") {
			t.Errorf("%s: got %v, want division by zero", q, err)
		}
	}
	// A branch Postgres drops because of a constant before it is not
	// folded, nor is a CTE no query reads.
	for _, tt := range []struct {
		q    string
		want []string
	}{
		{`SELECT coalesce(n, -1, 1 / 0) FROM t`, []string{"0"}},
		{`SELECT CASE WHEN false THEN 1 / 0 ELSE n END FROM t`, []string{"0"}},
		{`SELECT CASE WHEN true THEN n ELSE 1 / 0 END FROM t`, []string{"0"}},
		{`SELECT CASE WHEN 1 = 1 THEN 0 ELSE 1 / 0 END`, []string{"0"}},
		{`SELECT CASE 1 WHEN 2 THEN 1 / 0 ELSE 0 END`, []string{"0"}},
		{`SELECT 1 FROM t WHERE false AND 1 / 0 = 1`, nil},
		{`SELECT 1 / (CASE WHEN n > 0 THEN 0 END) FROM t WHERE false`, nil},
		{`WITH c AS (SELECT 1 / 0 AS v) SELECT n FROM t`, []string{"0"}},
	} {
		if got := rowsOf(t, db, tt.q); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
	// A constant detest cannot evaluate may fail in Postgres's folding, and
	// so may one in a branch Postgres keeps or drops depending on whether
	// it folds a constant whose volatility detest does not model.
	for _, q := range []string{
		`SELECT CASE WHEN n > 0 THEN interval '1 day' / 0 END FROM t`,
		`SELECT CASE WHEN timestamptz '2024-01-01 00:00:00+00' + interval '1 day' = timestamptz '2024-01-02 00:00:00+00' THEN 0 ELSE 1 / 0 END`,
		`SELECT CASE WHEN timestamp '2024-01-01'::date = date '2024-01-01' THEN 1 ELSE 1 / 0 END`,
		`SELECT CASE WHEN (1, 2) = (1, 2) THEN 0 ELSE 1 / 0 END`,
		`SELECT coalesce(extract(year FROM timestamp '2024-01-01'), 1 / 0) FROM t WHERE false`,
		`SELECT date_trunc('fortnight', timestamp '2024-01-01') FROM t WHERE false`,
		`SELECT CASE WHEN 'true' THEN 1 / 0 ELSE 0 END FROM t WHERE false`,
		`SELECT 'false' AND 1 / 0 = 1 FROM t WHERE false`,
		`SELECT CASE WHEN 'a' || 1 = 'a1' THEN 0 ELSE 1 / 0 END`,
		// Postgres removes the outputs of a table in FROM, a view or a CTE
		// that the query around it does not read, before it folds them.
		`SELECT v FROM fv WHERE false`,
		`SELECT 1 FROM fv WHERE false`,
		`SELECT 1 FROM (SELECT 1 / 0 AS unused FROM t WHERE false) q`,
		`WITH c AS (SELECT 1 / 0 AS v FROM t WHERE false) SELECT 1 FROM c, c c2`,
		`WITH a AS (SELECT * FROM fv), fv AS (SELECT 1 AS v) SELECT v FROM a`,
		// The evaluators read a string literal under AND, OR or NOT as NULL
		// (#118), where Postgres reads it as a boolean.
		`SELECT CASE WHEN ('true' AND true) THEN 0 ELSE 1 / 0 END FROM t WHERE false`,
		`SELECT CASE WHEN NOT 'false' THEN 0 ELSE 1 / 0 END FROM t WHERE false`,
		// A function detest does not list may or may not be strict.
		`SELECT CASE WHEN md5(NULL || n) IS NULL THEN 1 / 0 END FROM t`,
	} {
		if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	// What detest does not model is refused only where it changes the
	// outcome.
	for _, tt := range []struct {
		q    string
		want []string
	}{
		{`SELECT CASE WHEN timestamp '2024-01-01' < timestamp '2024-01-02' THEN n ELSE 1 END FROM t`, []string{"0"}},
		{`SELECT (a, b) = (1, 2) FROM (SELECT 1 AS a, 2 AS b) q`, []string{"true"}},
		{`SELECT CASE WHEN n > 0 THEN 1 WHEN timestamp '2024-01-01' < timestamp '2024-01-02' THEN 2 END + 1 FROM t`, []string{"3"}},
	} {
		if got := rowsOf(t, db, tt.q); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
}

// Postgres does not plan the query of a CREATE TABLE AS that creates no
// rows or no table.
func TestFoldConstantsCreateTableAsUnplanned(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int)`)
	mustExec(t, db, `CREATE TABLE src (x int)`)
	mustExec(t, db, `CREATE TABLE IF NOT EXISTS t AS SELECT 1 / 0 AS id`)
	mustExec(t, db, `CREATE TABLE dst AS SELECT 1 / 0 AS v FROM src WITH NO DATA`)
	if _, err := db.Exec(`CREATE TABLE dst2 AS SELECT 1 / 0 AS v FROM src`); err == nil || !strings.Contains(err.Error(), "division by zero") {
		t.Errorf("got %v, want division by zero", err)
	}
}
