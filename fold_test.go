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
		`SELECT v FROM fv WHERE false`,
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
	// A constant detest cannot evaluate may fail in Postgres's folding.
	q := `SELECT CASE WHEN n > 0 THEN interval '1 day' / 0 END FROM t`
	if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
	}
}
