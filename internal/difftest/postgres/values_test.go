package postgres_test

import (
	"testing"

	"github.com/k1LoW/detest/internal/difftest"
)

// The values cases compare how detest converts and compares values with
// Postgres: untyped literals, values written to typed columns, row
// comparisons, IN with NULLs, large integers, numeric arithmetic and casts.
// A query returns ids, counts and booleans rather than numerics, whose digits
// as written detest does not keep (README).
var (
	valueSchema = []string{`CREATE TABLE t (id int PRIMARY KEY, ratio float8, amount numeric, name text)`}
	valueSeed   = []string{
		`INSERT INTO t VALUES ('1', '0.5', '99.00', 'a')`,
		`INSERT INTO t VALUES (2, 1.5, 100.50, '2')`,
		`INSERT INTO t VALUES (9, 2, 7, NULL)`,
		`INSERT INTO t VALUES (10, NULL, 1.5, 'x')`,
	}
)

var valueCases = []difftest.Case{
	{
		Name:   "untyped literal compared with a number",
		Schema: valueSchema, Seed: valueSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id FROM t WHERE id = '01'`),
			difftest.Q(0, `SELECT id FROM t WHERE id IN ('01', '2') ORDER BY id`),
			difftest.Q(0, `SELECT id FROM t WHERE id < '10' ORDER BY id`),
			difftest.Q(0, `SELECT id FROM t WHERE id = ANY (ARRAY[9, '10']) ORDER BY id`),
			difftest.Q(0, `SELECT id FROM t WHERE name = ANY (ARRAY['2', 'x']) ORDER BY id`),
			difftest.Q(0, `SELECT id FROM t WHERE name = ANY (ARRAY['2', NULL::text]) ORDER BY id`),
			difftest.Q(0, `SELECT id FROM t WHERE name = ANY (ARRAY[NULL]) ORDER BY id`),
			difftest.Q(0, `SELECT CASE id WHEN '09' THEN 'nine' ELSE 'other' END FROM t ORDER BY id`),
			difftest.Q(0, `SELECT id FROM t WHERE NULLIF(id, '01') IS NULL`),
			difftest.Q(0, `SELECT count(*) FROM t HAVING count(*) = '04'`),
			difftest.Q(0, `SELECT id FROM t WHERE '01' IN (SELECT id FROM t) ORDER BY id`),
			difftest.Q(0, `SELECT id FROM t WHERE name = '2'`),
			difftest.Q(0, `SELECT count(*) FROM t WHERE id = 'abc'`),
		},
	},
	{
		Name:   "values written to typed columns",
		Schema: valueSchema, Seed: valueSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id FROM t WHERE amount < 100 ORDER BY id`),
			difftest.Q(0, `SELECT id FROM t ORDER BY amount, id`),
			difftest.Q(0, `SELECT id FROM t WHERE ratio > 1 ORDER BY id`),
			difftest.S(0, `INSERT INTO t (id) VALUES ('01')`),
			difftest.S(0, `INSERT INTO t (id) VALUES ('abc')`),
			difftest.S(0, `INSERT INTO t (id) VALUES ('1.5')`),
			difftest.S(0, `INSERT INTO t (id, amount) VALUES (5, 'abc')`),
			difftest.S(0, `INSERT INTO t (id) VALUES ('9223372036854775808')`),
			difftest.S(0, `INSERT INTO t (id, ratio) VALUES (7, '1e400')`),
			difftest.S(0, `INSERT INTO t (id, ratio, amount) VALUES (5, ' 1e3 ', '.5')`),
			difftest.Q(0, `SELECT ratio = 1000, amount = 0.5 FROM t WHERE id = 5`),
			difftest.S(0, `INSERT INTO t (id, name) VALUES (6, 2)`),
			difftest.Q(0, `SELECT id FROM t WHERE name = '02'`),
			difftest.S(0, `UPDATE t SET amount = ' 7 ' WHERE id = 2`),
			difftest.Q(0, `SELECT id FROM t WHERE amount = 7 ORDER BY id`),
		},
	},
	{
		Name:   "numeric and float arithmetic",
		Schema: valueSchema, Seed: valueSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id, amount / 2 = 0.75 FROM t WHERE id = 10`),
			difftest.Q(0, `SELECT (amount * 2) / 2 = 1.5 FROM t WHERE id = 10`),
			difftest.Q(0, `SELECT ratio / 4 = 0.5 FROM t WHERE id = 9`),
			difftest.Q(0, `SELECT sum(amount) > 208 FROM t`),
			difftest.Q(0, `SELECT floor(3) / 2 = 1.5, abs(-3) / 2`),
			difftest.Q(0, `SELECT (1.5 * 2) / 2 = 1.5, -(1.5 * 2) / 2 = -1.5`),
			difftest.Q(0, `SELECT id FROM t WHERE id * 1000000 = 9000000.0`),
			difftest.Q(0, `SELECT id FROM t ORDER BY CASE WHEN id = 2 THEN 'inf'::float8 - 'inf'::float8 ELSE ratio END DESC NULLS LAST, id LIMIT 2`),
			difftest.Q(0, `SELECT count(*) FROM (SELECT DISTINCT CASE WHEN id = 1 THEN 1000000 ELSE 1000000.0 END FROM t) s`),
			difftest.Q(0, `SELECT count(*) FROM (SELECT 1 FROM t GROUP BY CASE WHEN id = 1 THEN 1000000 ELSE 1000000.0 END) s`),
			difftest.Q(0, `SELECT count(*) FROM (SELECT 1000000 UNION SELECT 1000000.0) s`),
			difftest.Q(0, `SELECT count(DISTINCT CASE WHEN id = 1 THEN 1000000 ELSE 1000000.0 END) FROM t`),
			difftest.Q(0, `SELECT (CASE WHEN id > 0 THEN 1 ELSE 1.5 END) / 2 = 0.5 FROM t WHERE id = 1`),
			difftest.Q(0, `SELECT sum(CASE WHEN id > 0 THEN 1 ELSE 1.5 END) / 2 = 0.5 FROM t WHERE id = 1`),
			difftest.Q(0, `SELECT COALESCE(NULL, 1, 1.5) / 2 = 0.5, greatest(1, 0.5) / 2 = 0.5`),
			difftest.Q(0, `SELECT (CASE WHEN true THEN 1 ELSE -1.5 END) / 2 = 0.5, (CASE WHEN true THEN 1 ELSE 2 * 0.5 END) / 2 = 0.5`),
			difftest.Q(0, `SELECT (CASE WHEN true THEN 1 ELSE CASE WHEN false THEN 2.5 END END) / 2 = 0.5, COALESCE(1, round(2.5)) / 2 = 0.5`),
			difftest.Q(0, `SELECT NULLIF(1, 2.5) / 2 = 0.5`),
			difftest.Q(0, `SELECT count(*) FROM t HAVING NULLIF(count(*), 2.5) / 8 = 0.5`),
		},
	},
	{
		Name:   "casts to numbers",
		Schema: valueSchema, Seed: valueSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT '10'::int > '9'::int, ' 01 '::int = 1, '1.50'::numeric = 1.5`),
			difftest.Q(0, `SELECT 1.6::int, (-1.4)::smallint, true::int`),
			difftest.Q(0, `SELECT 'abc'::int`),
			difftest.Q(0, `SELECT 'not_a_number'::int`),
			difftest.Q(0, `SELECT '1_000_'::int`),
			difftest.Q(0, `SELECT '40000'::smallint`),
			difftest.Q(0, `SELECT 3000000000::int`),
			difftest.Q(0, `SELECT '9223372036854775808'::bigint`),
			difftest.Q(0, `SELECT '1e400'::float8`),
			difftest.Q(0, `SELECT '1e3'::numeric = 1000, 'inf'::float8 > 1e308`),
		},
	},
	{
		Name:   "untyped literal compared with a boolean",
		Schema: []string{`CREATE TABLE e (id int PRIMARY KEY, active bool)`},
		Seed:   []string{`INSERT INTO e VALUES (1, true), (2, false)`},
		Conns:  1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id FROM e WHERE active = 't' ORDER BY id`),
			difftest.Q(0, `SELECT id FROM e WHERE active = ' OFF ' ORDER BY id`),
			difftest.Q(0, `SELECT true = 'yes', true = ' TRUE ', true = 'on', true = '1', false = 'of', false = 'n'`),
			difftest.Q(0, `SELECT true = ANY (ARRAY['yes', false])`),
			difftest.Q(0, `SELECT true = 'o'`),
			difftest.Q(0, `SELECT true = 'maybe'`),
		},
	},
	{
		Name: "row comparison",
		Schema: []string{
			`CREATE TABLE p (a int, b int, name text, PRIMARY KEY (a, b))`,
		},
		Seed:  []string{`INSERT INTO p VALUES (9, 1, 'x'), (10, 1, 'y'), (2, 5, 'z'), (9, 3, NULL)`},
		Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT a, b FROM p WHERE (a, b) > (9, 1) ORDER BY a, b`),
			difftest.Q(0, `SELECT a, b FROM p WHERE (a, b) <= (9, 1) ORDER BY a, b`),
			difftest.Q(0, `SELECT a, b FROM p WHERE (a, b) = ('09', 1)`),
			difftest.Q(0, `SELECT a, b FROM p WHERE (a, b) <> (9, 1) ORDER BY a, b`),
			difftest.Q(0, `SELECT a, b FROM p WHERE (a, b) IN ((2, 5), ('10', 1)) ORDER BY a`),
			difftest.Q(0, `SELECT a, b FROM p WHERE (a, name) = (9, NULL)`),
			difftest.Q(0, `SELECT a, b FROM p WHERE NOT ((a, name) = (10, NULL)) ORDER BY a, b`),
			difftest.Q(0, `SELECT a, b FROM p WHERE (a * 1000000, b) > (9000000.0, 2) ORDER BY a, b`),
		},
	},
	{
		Name: "IN and NOT IN over a subquery with NULLs",
		Schema: []string{
			`CREATE TABLE p (a int, b int, name text, PRIMARY KEY (a, b))`,
		},
		Seed:  []string{`INSERT INTO p VALUES (9, 1, 'x'), (10, 1, 'y'), (2, 5, 'z'), (9, 3, NULL)`},
		Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT count(*) FROM p WHERE (a, name) NOT IN (SELECT 9, NULL)`),
			difftest.Q(0, `SELECT count(*) FROM p WHERE a NOT IN (SELECT NULL::int)`),
			difftest.Q(0, `SELECT count(*) FROM p WHERE NULL::int NOT IN (SELECT a FROM p WHERE a < 0)`),
			difftest.Q(0, `SELECT count(*) FROM p WHERE a IN (SELECT a FROM p WHERE name IS NULL)`),
		},
	},
	{
		Name:   "bigint keys above 2^53",
		Schema: []string{`CREATE TABLE s (id int8 PRIMARY KEY)`},
		Seed:   []string{`INSERT INTO s VALUES (9007199254740992), (9007199254740993)`},
		Conns:  1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id FROM s WHERE id > 9007199254740992`),
			difftest.Q(0, `SELECT id FROM s ORDER BY id DESC LIMIT 1`),
			difftest.Q(0, `SELECT id FROM s WHERE id > 9007199254740992.0`),
			difftest.S(0, `INSERT INTO s VALUES ('9007199254740993')`),
		},
	},
	{
		Name:   "numeric keys written in different forms",
		Schema: []string{`CREATE TABLE u (n numeric PRIMARY KEY)`},
		Seed:   []string{`INSERT INTO u VALUES (1000000)`},
		Conns:  1,
		Steps: []difftest.Step{
			difftest.S(0, `INSERT INTO u VALUES ('1000000')`),
			difftest.S(0, `INSERT INTO u VALUES (1000000.0)`),
			difftest.S(0, `INSERT INTO u VALUES (1e6)`),
			difftest.S(0, `INSERT INTO u VALUES ('-0')`),
			difftest.S(0, `INSERT INTO u VALUES (0)`),
		},
	},
}

func TestDiffValues(t *testing.T) {
	for _, c := range valueCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			difftest.Run(t, backend{}, c)
		})
	}
}
