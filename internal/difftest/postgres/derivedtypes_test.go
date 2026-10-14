package postgres_test

import (
	"testing"

	"github.com/k1LoW/detest/internal/difftest"
)

var derivedTypeCases = []difftest.Case{
	{
		Name: "a literal beside a column of a *, a set operation or generate_series",
		Schema: []string{
			`CREATE TABLE t (id int PRIMARY KEY, name text, n int)`,
			`CREATE VIEW tv AS SELECT id, name FROM t UNION ALL SELECT 3, NULL`,
		},
		Seed:  []string{`INSERT INTO t VALUES (1, 'a', 5), (2, NULL, NULL)`},
		Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT coalesce(s.name, 'x') FROM (SELECT * FROM t) s ORDER BY s.id`),
			difftest.Q(0, `SELECT coalesce(s.name, 'x') FROM (SELECT t.* FROM t) s ORDER BY s.id`),
			difftest.Q(0, `WITH c AS (SELECT * FROM t) SELECT coalesce(c.name, 'x') FROM c ORDER BY c.id`),
			difftest.Q(0, `SELECT coalesce(name, 'x') FROM tv ORDER BY id`),
			difftest.Q(0, `SELECT CASE WHEN s.id = 1 THEN s.name ELSE 'x' END FROM (SELECT * FROM t) s ORDER BY s.id`),
			difftest.Q(0, `SELECT greatest(s.n, 1) FROM (SELECT * FROM t) s ORDER BY s.id`),
			difftest.Q(0, `SELECT coalesce(u.name, 'x') FROM (SELECT name FROM t UNION SELECT name FROM t) u ORDER BY 1`),
			difftest.Q(0, `SELECT coalesce(u.name, 'x') FROM (SELECT name FROM t UNION ALL SELECT NULL) u ORDER BY 1`),
			difftest.Q(0, `SELECT coalesce(g, 0) FROM generate_series(1, 2) AS g ORDER BY 1`),
			difftest.Q(0, `SELECT coalesce(k, 0) FROM generate_series(1, 2) AS g(k) ORDER BY 1`),
			difftest.Q(0, `SELECT * FROM generate_series(1, 2) g ORDER BY 1 DESC`),
			difftest.Q(0, `SELECT s.nope FROM (SELECT * FROM t) s`),
		},
	},
}

func TestDiffDerivedTypes(t *testing.T) {
	for _, c := range derivedTypeCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			difftest.Run(t, backend{}, c)
		})
	}
}
