package postgres_test

import (
	"testing"

	"github.com/k1LoW/detest/internal/difftest"
)

// The array cases compare x = ANY (a) and x <> ALL (a) over an array given
// as a value with Postgres. Steps take no parameters, so the array is the
// text pq.Array sends, written as a literal; a Go slice bound to a parameter
// reaches the same comparison and is covered by the unit tests.
var (
	arraySchema = []string{`CREATE TABLE a (id bigint PRIMARY KEY, name text, n int, u uuid)`}
	arraySeed   = []string{`INSERT INTO a VALUES
		(1, 'a', 10, '00000000-0000-0000-0000-000000000001'),
		(2, 'b', NULL, '00000000-0000-0000-0000-00000000000a'),
		(3, 'a b', 30, NULL)`}
)

var arrayCases = []difftest.Case{
	{
		Name:   "= ANY and <> ALL over array text",
		Schema: arraySchema, Seed: arraySeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id FROM a WHERE id = ANY('{1,3,9}') ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE id <> ALL('{1,3}') ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE id != ALL('{1}') ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE NOT (id = ANY('{1}')) ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE name = ANY('{"a b", b}') ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE n = ANY('{10,30}') ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE id = ANY('{01}') ORDER BY id`),
			difftest.Q(0, `UPDATE a SET n = n + 1 WHERE id = ANY('{1,2,9}') RETURNING id`),
			difftest.S(0, `DELETE FROM a WHERE id = ANY('{9}')`),
		},
	},
	{
		Name:   "the empty array and NULLs in = ANY and <> ALL",
		Schema: arraySchema, Seed: arraySeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT count(*) FROM a WHERE id = ANY('{}')`),
			difftest.Q(0, `SELECT count(*) FROM a WHERE n <> ALL('{}')`),
			difftest.Q(0, `SELECT count(*) FROM a WHERE n = ANY(' { } ')`),
			difftest.Q(0, `SELECT id FROM a WHERE (n = ANY('{10}')) IS NULL ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE (id = ANY('{1,NULL}')) IS NULL ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE (id <> ALL('{1,NULL}')) IS NULL ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE (id <> ALL('{2,null}')) IS NOT NULL ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE (name = ANY('{"NULL",null}')) IS NULL ORDER BY id`),
		},
	},
	{
		Name:   "quoting and space in array text",
		Schema: arraySchema, Seed: arraySeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id FROM a WHERE id = ANY('{ 1 , 2 }') ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE id = ANY(' {3} ') ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE name = ANY('{ a b }') ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE name = ANY('{a\ b}') ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE name = ANY('{"a" , "b" }') ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE name = ANY('{"a\"b", "", a}') ORDER BY id`),
		},
	},
	{
		Name:   "array text cast to an array type",
		Schema: arraySchema, Seed: arraySeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id FROM a WHERE id = ANY('{1,3}'::bigint[]) ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE n = ANY('{10,30}'::int[]) ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE n = ANY('{10}'::bigint[]) ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE id <> ALL('{2}'::int8[]) ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE name = ANY('{a,b}'::text[]) ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE name = ANY('{b}'::varchar[]) ORDER BY id`),
			difftest.Q(0, `SELECT id FROM a WHERE u = ANY('{00000000-0000-0000-0000-000000000001}'::uuid[]) ORDER BY id`),
		},
	},
	{
		Name:   "a malformed array fails the statement before it reads a row",
		Schema: append(append([]string{}, arraySchema...), `CREATE TABLE e (id bigint PRIMARY KEY)`),
		Seed:   arraySeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id FROM e WHERE id = ANY('1,2')`),
			difftest.Q(0, `SELECT id FROM e WHERE id = ANY('{1,2')`),
			difftest.Q(0, `SELECT id FROM e WHERE id = ANY('{1,,2}')`),
			difftest.Q(0, `SELECT id FROM e WHERE id = ANY('{1,}')`),
			difftest.Q(0, `SELECT id FROM e WHERE id = ANY('{,}')`),
			difftest.Q(0, `SELECT id FROM e WHERE id = ANY('{1,"2"x}')`),
			difftest.Q(0, `SELECT id FROM e WHERE id = ANY('{a"b"}')`),
			difftest.Q(0, `SELECT id FROM e WHERE id = ANY('{}x')`),
			difftest.Q(0, `SELECT id FROM e WHERE id = ANY('{"1"')`),
			difftest.Q(0, `SELECT id FROM e WHERE id = ANY('{"1')`),
			difftest.Q(0, `SELECT id FROM e WHERE id = ANY('{abc}'::bigint[])`),
			difftest.S(0, `UPDATE e SET id = 1 WHERE id = ANY('{1')`),
			difftest.Q(0, `SELECT id FROM a WHERE id = ANY('{1,abc}')`),
		},
	},
	{
		Name:   "update over = ANY waits and rechecks",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = n - 1 WHERE sku = ANY('{apple,pear}') AND n > 0`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "select for update over <> ALL with skip locked",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(1, `BEGIN`),
			difftest.Q(0, `SELECT sku FROM stock WHERE sku = ANY('{apple}') FOR UPDATE`),
			difftest.Q(1, `SELECT sku FROM stock WHERE sku <> ALL('{}') ORDER BY sku FOR UPDATE SKIP LOCKED`),
			difftest.Q(1, `SELECT sku FROM stock WHERE sku = ANY('{apple,pear}') ORDER BY sku FOR UPDATE NOWAIT`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `ROLLBACK`),
		},
	},
}

func TestDiffArrays(t *testing.T) {
	for _, c := range arrayCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			difftest.Run(t, backend{}, c)
		})
	}
}
