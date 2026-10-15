package postgres_test

import (
	"testing"

	"github.com/k1LoW/detest/internal/difftest"
)

var foldCases = []difftest.Case{
	{
		Name:   "an expression of constants that fails, folded when the statement is planned",
		Schema: []string{`CREATE TABLE t (id int PRIMARY KEY, n int)`},
		Seed:   []string{`INSERT INTO t VALUES (1, 0)`},
		Conns:  1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT CASE WHEN n = 0 THEN 1 ELSE 1 / 0 END FROM t`),
			difftest.Q(0, `SELECT n + 1 / 0 FROM t WHERE false`),
			difftest.Q(0, `SELECT coalesce(n, 1 / 0) FROM t`),
			difftest.Q(0, `SELECT coalesce(n, -1, 1 / 0) FROM t`),
			difftest.Q(0, `SELECT CASE WHEN false THEN 1 / 0 ELSE n END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN true THEN n ELSE 1 / 0 END FROM t`),
			difftest.Q(0, `SELECT 1 FROM t WHERE false AND 1 / 0 = 1`),
			difftest.Q(0, `SELECT 1 FROM t WHERE 1 / 0 = 1 AND false`),
			difftest.Q(0, `SELECT sum(1 / 0) FROM t WHERE false`),
			difftest.Q(0, `SELECT CASE 1 WHEN 2 THEN 1 / 0 ELSE 0 END`),
			difftest.Q(0, `SELECT coalesce(n, extract(year FROM now()), 1 / 0) FROM t`),
			difftest.Q(0, `SELECT n + (SELECT 1 / 0) FROM t WHERE false`),
			difftest.Q(0, `SELECT abs(-1) / 0 FROM t WHERE false`),
			difftest.Q(0, `SELECT 1 / (CASE WHEN n > 0 THEN 0 END) FROM t WHERE false`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 1 % 0 END FROM t`),
			difftest.S(0, `UPDATE t SET n = 1 / 0 WHERE false`),
			difftest.Q(0, `SELECT 1 FROM t WHERE n = 1 OR 1 / 0 = 1`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 'a' || 1 / 0 END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN 1 = 1 THEN 0 ELSE 1 / 0 END`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN nullif(1, 1 / 0) END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 'x'::int + 1 END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 1 / 0 END FROM t WHERE false`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 1.0 / 0 END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 1 / 0 > 1 END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 1 / 0 IS NULL END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 1 IN (1 / 0) END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN -(1 / 0) END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN (1 / 0)::text END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN greatest(1, 1 / 0) END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN lower('A' || (1 / 0)::text) END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN date_trunc('day', now()) + interval '1 day' * (1 / 0) END FROM t`),
			difftest.Q(0, `SELECT n FROM t ORDER BY 1 / 0 LIMIT 0`),
			difftest.Q(0, `SELECT n FROM t LIMIT 1 / 0`),
			difftest.Q(0, `WITH c AS (SELECT 1 / 0 AS v) SELECT n FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN coalesce(1, 1 / 0) END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN coalesce(NULL, 1 / 0) END FROM t`),
			difftest.Q(0, `SELECT CASE n WHEN 1 THEN 1 / 0 END FROM t`),
		},
	},
}

func TestDiffFold(t *testing.T) {
	for _, c := range foldCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			difftest.Run(t, backend{}, c)
		})
	}
}
