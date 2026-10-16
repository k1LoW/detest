package detest

import (
	"errors"
	"slices"
	"testing"

	"github.com/k1LoW/detest/db/mysql"
	"github.com/k1LoW/detest/db/postgres"
)

// Postgres sorts the rows of a set operation only by an output column, named
// or by position, and fails any other key whatever rows the statement reads.
func TestSetOpOrderExpressionRefused(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int)`)
	for _, q := range []string{
		`SELECT 1 UNION SELECT 2 ORDER BY abs(1)`,
		`SELECT n FROM t UNION SELECT n FROM t ORDER BY random()`,
		`SELECT n FROM t UNION SELECT n FROM t ORDER BY n + 1`,
		`SELECT n FROM t UNION SELECT n FROM t ORDER BY 1 + 0`,
		`SELECT n FROM t UNION SELECT n FROM t ORDER BY -n`,
		`SELECT n FROM t UNION SELECT n FROM t ORDER BY n::bigint`,
		`SELECT n FROM t UNION SELECT n FROM t ORDER BY (SELECT 1)`,
		`SELECT n FROM t UNION SELECT n FROM t ORDER BY n IS NULL`,
		`SELECT n FROM t UNION SELECT n FROM t ORDER BY 1, abs(1)`,
		`SELECT n FROM t INTERSECT SELECT n FROM t ORDER BY n + 1`,
		`SELECT n FROM t EXCEPT ALL SELECT n FROM t ORDER BY n + 1`,
		`SELECT 1 FROM t WHERE EXISTS (SELECT 1 UNION SELECT 2 ORDER BY abs(1))`,
		`SELECT (SELECT 1 UNION SELECT 2 ORDER BY abs(1) LIMIT 1) FROM t`,
		`SELECT * FROM (SELECT n FROM t UNION SELECT 2 ORDER BY n + 1) x`,
		`WITH c AS (SELECT 1 UNION SELECT 2 ORDER BY abs(1)) SELECT 1`,
		`(SELECT n FROM t UNION SELECT 2 ORDER BY n + 1) UNION SELECT 3`,
		`INSERT INTO t SELECT 1, 2 UNION SELECT 3, 4 ORDER BY abs(1)`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	if _, err := db.Exec(`SELECT n FROM t UNION SELECT n FROM t ORDER BY t.n`); !errors.Is(err, ErrUndefinedTable) {
		t.Errorf("qualified name: got %v, want ErrUndefinedTable", err)
	}
	for _, q := range []string{
		`SELECT n FROM t UNION SELECT n FROM t ORDER BY n`,
		`SELECT n FROM t UNION SELECT n FROM t ORDER BY (n)`,
		`SELECT n FROM t UNION SELECT n FROM t ORDER BY 1 DESC`,
		`SELECT n AS m FROM t UNION SELECT n FROM t ORDER BY m DESC NULLS LAST`,
		`(SELECT n FROM t ORDER BY n + 1 LIMIT 1) UNION SELECT n FROM t`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}

// MySQL sorts the rows of a set operation by an expression over its outputs.
func TestSetOpOrderExpressionMySQL(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysql.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 1), (2, 2), (3, 3)`)
	rows, err := db.Query(`SELECT n FROM t UNION SELECT n FROM t ORDER BY -n`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []int{3, 2, 1}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
