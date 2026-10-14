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
			difftest.Q(0, `SELECT coalesce(u.n, 0) FROM (SELECT NULL AS n UNION ALL (SELECT NULL UNION ALL SELECT n FROM t)) u ORDER BY 1`),
			difftest.Q(0, `SELECT coalesce(u.name, 'x') FROM (SELECT 'b' AS name UNION ALL SELECT name FROM t) u ORDER BY 1`),
			difftest.Q(0, `SELECT coalesce(g, 0) FROM generate_series(1, 2) AS g ORDER BY 1`),
			difftest.Q(0, `SELECT coalesce(k, 0) FROM generate_series(1, 2) AS g(k) ORDER BY 1`),
			difftest.Q(0, `SELECT * FROM generate_series(1, 2) g ORDER BY 1 DESC`),
			difftest.Q(0, `SELECT s.nope FROM (SELECT * FROM t) s`),
		},
	},
	{
		Name:   "a number branch beside a NULL column of another number type, or beside text",
		Schema: []string{`CREATE TABLE t (id int PRIMARY KEY, n int, m numeric)`},
		Seed:   []string{`INSERT INTO t VALUES (1, NULL, NULL), (2, 3, 1)`},
		Conns:  1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT (coalesce(m, 1) / 2 = 0.5)::text FROM t ORDER BY id`),
			difftest.Q(0, `SELECT (coalesce(s.m, 1) / 2 = 0.5)::text FROM (SELECT * FROM t) s ORDER BY s.id`),
			difftest.Q(0, `SELECT (CASE WHEN id = 1 THEN 1 ELSE m END / 2 = 0.5)::text FROM t ORDER BY id`),
			difftest.Q(0, `SELECT (coalesce(n, m) / 2 * 2 = 3)::text FROM t WHERE id = 2`),
			difftest.Q(0, `SELECT coalesce(n, '5') / 2 FROM t ORDER BY id`),
			difftest.Q(0, `SELECT CASE WHEN id = 1 THEN '7' ELSE n END FROM t ORDER BY id`),
			difftest.Q(0, `SELECT coalesce(n, 'bogus') FROM t WHERE id = 2`),
			difftest.Q(0, `SELECT coalesce(n, 'bogus') FROM t WHERE false`),
			difftest.Q(0, `SELECT CASE WHEN false THEN 'bogus' ELSE n END FROM t`),
			difftest.Q(0, `SELECT coalesce(s.n, 'bogus') FROM (SELECT * FROM t) s`),
			difftest.Q(0, `SELECT (nullif(1, s.m) / 2 = 0.5)::text FROM (SELECT * FROM t) s ORDER BY s.id`),
			difftest.Q(0, `SELECT (nullif(2, n) / 2)::text FROM t ORDER BY id`),
			difftest.Q(0, `SELECT (coalesce(coalesce(1, NULL::numeric), '0.5') = 1)::text`),
			difftest.Q(0, `SELECT (coalesce(coalesce(NULL, NULL::numeric), '0.5') = 0.5)::text`),
			difftest.Q(0, `SELECT (lag(s.m, 1, 1) OVER (ORDER BY s.id) / 2 = 0.5)::text FROM (SELECT * FROM t) s ORDER BY s.id`),
			difftest.Q(0, `SELECT (lead(m, 1, 1) OVER (ORDER BY id) / 2 = 0.5)::text FROM t ORDER BY id`),
			difftest.Q(0, `SELECT lag(n, 1, '4') OVER (ORDER BY id) / 2 FROM t ORDER BY id`),
			difftest.Q(0, `SELECT lag(n, 1, 'bogus') OVER (ORDER BY id) FROM t WHERE false`),
			difftest.Q(0, `SELECT (coalesce(nullif(1, s.m), '0.5') = 1)::text FROM (SELECT * FROM t) s ORDER BY s.id`),
			difftest.Q(0, `SELECT (CASE WHEN true THEN 1 WHEN false THEN '0.5' ELSE s.m END = 1)::text FROM (SELECT * FROM t) s ORDER BY s.id`),
			difftest.Q(0, `SELECT (CASE WHEN id = 1 THEN '0.5' ELSE m END = 0.5)::text FROM t ORDER BY id`),
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
