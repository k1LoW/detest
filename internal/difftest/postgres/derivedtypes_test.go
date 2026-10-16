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
			difftest.Q(0, `SELECT coalesce(lag(s.n, NULL, 0) OVER (ORDER BY s.id), -1) FROM (SELECT * FROM t) s ORDER BY s.id`),
			difftest.Q(0, `SELECT coalesce(lead(s.n, NULL, 0) OVER (ORDER BY s.id), -1) FROM (SELECT * FROM t) s ORDER BY s.id`),
			difftest.Q(0, `SELECT s.nope FROM (SELECT * FROM t) s`),
		},
	},
	{
		Name: "a number branch beside a NULL column of another number type, or beside text",
		Schema: []string{
			`CREATE TABLE t (id int PRIMARY KEY, n int, m numeric)`,
			`CREATE TABLE k (id int PRIMARY KEY, m numeric, CHECK ((CASE WHEN true THEN 1 WHEN false THEN '0.5' ELSE m END) / 2 = 0.5))`,
		},
		Seed:  []string{`INSERT INTO t VALUES (1, NULL, NULL), (2, 3, 1)`},
		Conns: 1,
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
			difftest.S(0, `INSERT INTO k VALUES (1, NULL)`),
			difftest.Q(0, `SELECT coalesce(nullif('1', s.n), 'bogus') FROM (SELECT * FROM t) s WHERE false`),
			difftest.Q(0, `SELECT (coalesce(nullif('1', s.m), '0.5') = 0.5)::text FROM (SELECT * FROM t) s ORDER BY s.id`),
			difftest.Q(0, `SELECT (coalesce(nullif(NULL, m), 1) / 2 = 0.5)::text FROM t ORDER BY id`),
		},
	},
	{
		Name:   "a literal beside a bytea column of a *",
		Schema: []string{`CREATE TABLE b (id int PRIMARY KEY, payload bytea)`},
		Seed:   []string{`INSERT INTO b VALUES (1, '\x42'), (2, NULL)`},
		Conns:  1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT (lag(s.payload, 1, '\x41') OVER (ORDER BY s.id) = '\x41'::bytea)::text FROM (SELECT * FROM b) s ORDER BY s.id`),
			difftest.Q(0, `SELECT (coalesce(s.payload, '\x41') = '\x41'::bytea)::text FROM (SELECT * FROM b) s ORDER BY s.id`),
			difftest.Q(0, `SELECT lag(payload, 1, '\xZZ') OVER (ORDER BY id) FROM b WHERE false`),
			difftest.Q(0, `SELECT coalesce(s.payload, '\xZZ') FROM (SELECT * FROM b) s WHERE false`),
		},
	},
	{
		Name:   "a literal beside a date column of a *",
		Schema: []string{`CREATE TABLE d (id int PRIMARY KEY, d date)`},
		Seed:   []string{`INSERT INTO d VALUES (1, '2025-01-01'), (2, NULL)`},
		Conns:  1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT (coalesce(s.d, '2025-01-02') = '2025-01-02'::date)::text FROM (SELECT * FROM d) s ORDER BY s.id`),
			difftest.Q(0, `SELECT (CASE WHEN s.id = 2 THEN '2025-01-02' ELSE s.d END = '2025-01-01'::date)::text FROM (SELECT * FROM d) s ORDER BY s.id`),
		},
	},
	{
		Name: "a literal beside a scalar subquery among the branches",
		Schema: []string{
			`CREATE TABLE a (id int PRIMARY KEY, n text, m numeric)`,
			`CREATE TABLE b (n numeric)`,
		},
		Seed:  []string{`INSERT INTO a VALUES (1, NULL, NULL)`, `INSERT INTO b VALUES (NULL)`},
		Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT coalesce((SELECT n FROM b), s.m, 'bogus') FROM (SELECT * FROM a) s`),
			difftest.Q(0, `SELECT coalesce(s.v, 'bogus') FROM (SELECT a.*, (SELECT n FROM b) AS v FROM a) s`),
			difftest.Q(0, `SELECT coalesce(u.v, 'bogus') FROM (SELECT v FROM (SELECT (SELECT n FROM b) AS v FROM a) s UNION ALL SELECT m FROM a) u`),
		},
	},
	{
		Name:   "a query output of string literals or NULLs is text to the query around it",
		Schema: []string{`CREATE TABLE t (id int PRIMARY KEY)`},
		Conns:  1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT x.v || 'a', (x.v = '1')::text, length(x.v) FROM (SELECT '1' AS v) x`),
			difftest.Q(0, `SELECT coalesce(x.v, 'z'), (x.v IS NULL)::text FROM (SELECT NULL AS v) x`),
			difftest.Q(0, `WITH c AS (SELECT '1' AS v) SELECT v || 'b' FROM c`),
			difftest.Q(0, `SELECT x.v || 'c' FROM (SELECT max('1') AS v) x`),
			difftest.Q(0, `SELECT x.v::int + 1 FROM (SELECT '1' AS v) x`),
			difftest.Q(0, `SELECT upper((SELECT 'a')), ((SELECT '1') = '1')::text, (SELECT max('1')) || 'x'`),
			difftest.Q(0, `SELECT x.v || 'd' FROM t, LATERAL (SELECT '1' AS v) x`),
			difftest.Q(0, `SELECT x.a || 'e', x.b || 'f', x.c || 'g' FROM (SELECT first_value('1'::text) OVER () AS a, lead('1', 1, '2') OVER () AS b, first_value(coalesce('1', NULL)) OVER () AS c) x`),
		},
	},
	{
		Name:   "an expression of string literals or NULLs alone compared as text",
		Schema: []string{`CREATE TABLE t (id int PRIMARY KEY, n int, s text, v varchar(10))`},
		Seed:   []string{`INSERT INTO t VALUES (1, 1, '1', '1'), (2, 2, 'x', NULL)`},
		Conns:  1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id, s = coalesce('1', NULL), v = CASE WHEN true THEN '1' END FROM t ORDER BY id`),
			difftest.Q(0, `SELECT id, s IN (coalesce('1', NULL), 'x'), CASE s WHEN max('1') OVER () THEN 1 ELSE 0 END FROM t ORDER BY id`),
			difftest.Q(0, `SELECT id FROM t WHERE '1' = coalesce('1', NULL) AND n = coalesce(1, NULL) ORDER BY id`),
			difftest.Q(0, `SELECT id FROM t WHERE n = coalesce('1', NULL)::int ORDER BY id`),
		},
	},
	{
		Name: "a subquery whose select list is a * over a derived table, a CTE or a view",
		Schema: []string{
			`CREATE TABLE t (id int PRIMARY KEY, s text, n int)`,
			`CREATE VIEW nv AS SELECT n FROM t`,
		},
		Seed:  []string{`INSERT INTO t VALUES (1, '1', 1)`},
		Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT (1 = (SELECT * FROM (SELECT 1 AS v) x))::text`),
			difftest.Q(0, `SELECT (1 = (SELECT x.* FROM (SELECT n FROM t) x))::text`),
			difftest.Q(0, `SELECT coalesce((SELECT * FROM (SELECT '1' AS v) x), 'z')`),
			difftest.Q(0, `SELECT ((SELECT * FROM (VALUES ('1')) v) = '1')::text`),
			difftest.Q(0, `WITH c AS (SELECT n FROM t) SELECT (1 = (SELECT * FROM c))::text`),
			difftest.Q(0, `SELECT (1 = (SELECT * FROM nv))::text`),
			difftest.Q(0, `SELECT (1 IN (SELECT * FROM (SELECT n FROM t) x))::text`),
			difftest.Q(0, `SELECT ((1, '1') IN (SELECT x.* FROM (SELECT n, s FROM t) x JOIN t ON true))::text`),
			difftest.Q(0, `SELECT (1 = (SELECT * FROM (SELECT * FROM (SELECT n FROM t) x) y))::text`),
			difftest.Q(0, `WITH c AS (SELECT * FROM (VALUES (1)) a UNION ALL SELECT * FROM (VALUES (2)) b) SELECT (1 IN (SELECT * FROM c))::text`),
		},
	},
	{
		Name: "a set operation whose first query is a * over a derived table, a CTE or a view",
		Schema: []string{
			`CREATE TABLE t (id int PRIMARY KEY, s text, n int)`,
			`CREATE VIEW sv(w) AS SELECT * FROM (SELECT s FROM t) y`,
		},
		Seed:  []string{`INSERT INTO t VALUES (1, 'a', 1)`},
		Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT x.v || 'z' FROM (SELECT * FROM (SELECT s AS v FROM t) y UNION ALL SELECT 'b'::text) x ORDER BY 1`),
			difftest.Q(0, `SELECT x.v || 'z' FROM (SELECT y.* FROM (SELECT s AS v FROM t) y JOIN t ON true UNION ALL SELECT 'b'::text) x ORDER BY 1`),
			difftest.Q(0, `SELECT x.w || 'z' FROM (SELECT * FROM (SELECT s AS v FROM t) y(w) UNION ALL SELECT 'b'::text) x ORDER BY 1`),
			difftest.Q(0, `WITH c AS (SELECT n FROM t) SELECT x.n + 1 FROM (SELECT * FROM c UNION ALL SELECT 2) x ORDER BY 1`),
			difftest.Q(0, `WITH u AS (SELECT * FROM (SELECT s AS v FROM t) y UNION ALL SELECT 'b'::text) SELECT v || 'z' FROM u ORDER BY 1`),
			difftest.Q(0, `SELECT x.w || 'z' FROM (SELECT * FROM sv UNION ALL SELECT 'b'::text) x ORDER BY 1`),
			difftest.Q(0, `SELECT a FROM (SELECT * FROM (SELECT n AS v FROM t) y) z(a)`),
			difftest.Q(0, `SELECT v FROM (SELECT * FROM (SELECT n AS v FROM t) y) z(a)`),
		},
	},
	{
		Name:  "a column of VALUES in FROM that Postgres resolves to text",
		Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT (coalesce(v.column1, 'z') = '01')::text FROM (VALUES ('1')) v`),
			difftest.Q(0, `SELECT (coalesce((SELECT column1 FROM (VALUES ('1')) v), 'z') = '01')::text`),
			difftest.Q(0, `SELECT (coalesce((SELECT * FROM (VALUES ('1')) v), 'z') = '01')::text`),
			difftest.Q(0, `WITH v AS (VALUES ('1'), (NULL)) SELECT (column1 = '01')::text FROM v ORDER BY 1`),
			difftest.Q(0, `SELECT (x = '01')::text FROM (VALUES ('1'), ('a'::text)) v(x) ORDER BY 1`),
		},
	},
	{
		Name:   "a string literal in a column of VALUES that Postgres resolves to another type",
		Schema: []string{`CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy')`},
		Conns:  1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT (column1 = '01')::text FROM (VALUES ('1'), (2)) v ORDER BY 1`),
			difftest.Q(0, `SELECT 1 FROM (VALUES ('1'), (2)) v WHERE column1 = '01'`),
			difftest.Q(0, `SELECT column1 + 1 FROM (VALUES ('1'), (2)) v ORDER BY 1`),
			difftest.Q(0, `SELECT (column1 = '01')::text FROM (VALUES ('1'), (2.5)) v ORDER BY 1`),
			difftest.Q(0, `SELECT (column1 = '1')::text FROM (VALUES ('01'), (2), (NULL)) v ORDER BY 1`),
			difftest.Q(0, `SELECT count(*) FROM (VALUES ('1', 'a'), (2, 'b')) v WHERE column1 IN ('01', '02')`),
			difftest.Q(0, `WITH v AS (VALUES ('1'), (2)) SELECT count(*) FROM v WHERE column1 = '01'`),
			difftest.Q(0, `SELECT (column1 < date '2024-01-02')::text FROM (VALUES ('2024-01-01'), (date '2024-01-03')) v ORDER BY 1`),
			difftest.Q(0, `SELECT (column1 < 'ok')::text FROM (VALUES ('sad'), ('happy'::mood)) v ORDER BY 1`),
			difftest.Q(0, `SELECT column1 FROM (VALUES ('x'), (2)) v`),
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
