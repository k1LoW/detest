package detest

import (
	"errors"
	"testing"

	"github.com/k1LoW/detest/db/mysql"
	"github.com/k1LoW/detest/db/postgres"
)

// An aggregate or a window function in a clause that allows none fails on
// the server whatever rows the statement reads, so it is refused before any
// row, over an empty table as over one with rows.
func TestMisplacedAggregateRefused(t *testing.T) {
	both := []string{
		`SELECT 1 FROM t WHERE max(n) > 0`,
		`SELECT 1 FROM t WHERE count(*) > 0`,
		`UPDATE t SET n = max(n)`,
		`UPDATE t SET n = 1 WHERE max(n) > 0`,
		`DELETE FROM t WHERE max(n) > 0`,
		`SELECT 1 FROM t JOIN u ON max(t.n) = 1`,
		`SELECT 1 FROM t GROUP BY max(n)`,
		`SELECT max(n) FROM t GROUP BY 1`,
		`SELECT max(n) AS m FROM t GROUP BY m`,
		`INSERT INTO t VALUES (1, max(1))`,
		`SELECT 1 FROM t WHERE row_number() OVER () > 0`,
		`SELECT 1 FROM t WHERE sum(n) OVER () > 0`,
		`UPDATE t SET n = row_number() OVER ()`,
		`SELECT 1 FROM t GROUP BY row_number() OVER ()`,
		`SELECT row_number() OVER () FROM t GROUP BY 1`,
		`SELECT 1 FROM t JOIN u ON row_number() OVER () = 1`,
		`SELECT n FROM t GROUP BY n HAVING row_number() OVER () > 0`,
		`SELECT 1 FROM t WHERE EXISTS (SELECT 1 FROM u WHERE max(u.n) > 0)`,
		`SELECT (SELECT 1 FROM u WHERE max(u.n) > 0) FROM t`,
		`SELECT n FROM t UNION SELECT n FROM u ORDER BY row_number() OVER ()`,
		`SELECT 1 FROM t WHERE EXISTS (SELECT 1 UNION SELECT 2 ORDER BY row_number() OVER ())`,
		`SELECT 1 FROM t WHERE EXISTS (SELECT 1 UNION SELECT 2 ORDER BY max(1))`,
	}
	byKind := map[string][]string{
		"postgres": {
			`SELECT 1 FROM t LIMIT max(1)`,
			`SELECT 1 FROM t OFFSET max(1)`,
			`SELECT 1 FROM t LIMIT row_number() OVER ()`,
			`VALUES (max(1))`,
			`DELETE FROM t RETURNING max(n)`,
			`UPDATE t SET n = 1 RETURNING row_number() OVER ()`,
			`INSERT INTO t VALUES (1, 1) RETURNING max(n)`,
			`INSERT INTO t VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET n = max(excluded.n)`,
			`INSERT INTO t VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET n = 1 WHERE max(t.n) > 0`,
			`SELECT * FROM t, generate_series(1, max(1))`,
			`SELECT row_number() OVER (PARTITION BY row_number() OVER ()) FROM t`,
		},
		"mysql": {
			`INSERT INTO t VALUES (1, 1) ON DUPLICATE KEY UPDATE n = max(n)`,
			`UPDATE t SET n = 1 ORDER BY max(n)`,
			`DELETE FROM t ORDER BY max(n)`,
			`DELETE FROM t ORDER BY row_number() OVER ()`,
		},
	}
	accepted := []string{
		`SELECT max(n) FROM t HAVING max(n) > 0`,
		`SELECT n FROM t GROUP BY n ORDER BY max(id)`,
		`SELECT max(n) AS n FROM t GROUP BY id`,
		`SELECT 1 FROM t WHERE (SELECT max(u.n) FROM u) > 0`,
		`SELECT n, rank() OVER (PARTITION BY max(id)) FROM t GROUP BY n`,
		`UPDATE t SET n = (SELECT max(u.n) FROM u)`,
		`INSERT INTO t SELECT 1, max(n) FROM u`,
	}
	for _, tt := range []struct {
		name string
		srv  Server
	}{
		{"postgres", postgres.New()},
		{"mysql", mysql.New()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newSim(t)
			db, _ := s.DB("app", tt.srv)
			mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int)`)
			mustExec(t, db, `CREATE TABLE u (id int PRIMARY KEY, n int)`)
			for _, q := range append(both, byKind[tt.name]...) {
				if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
					t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
				}
			}
			for _, q := range accepted {
				if _, err := db.Exec(q); err != nil {
					t.Errorf("%s: %v", q, err)
				}
			}
		})
	}
}
