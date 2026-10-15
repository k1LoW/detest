package postgres_test

import (
	"testing"

	"github.com/k1LoW/detest/internal/difftest"
)

var (
	enumSchema = []string{
		`CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy')`,
		`CREATE TABLE p (id int PRIMARY KEY, m mood, name text)`,
	}
	enumSeed = []string{`INSERT INTO p VALUES (1, 'happy', 'a'), (2, 'sad', 'b'), (3, 'ok', 'c'), (4, NULL, 'd')`}
)

var enumCases = []difftest.Case{
	{
		// Postgres reads a literal by the enum it takes from an assignment,
		// a cast or the other branches when it plans the statement, so a
		// write or a branch no row reaches fails too.
		Name:   "enum input typed by assignment, cast or branch over no rows",
		Schema: enumSchema, Seed: enumSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.S(0, `UPDATE p SET m = 'bogus' WHERE false`),
			difftest.Q(0, `SELECT 'bogus'::mood FROM p WHERE false`),
			difftest.Q(0, `SELECT CASE WHEN true THEN m ELSE 'bogus' END FROM p WHERE false`),
			difftest.Q(0, `SELECT coalesce(m, 'bogus') FROM p WHERE false`),
			difftest.Q(0, `SELECT m FROM p WHERE false UNION SELECT 'bogus' WHERE false`),
			difftest.S(0, `INSERT INTO p (id, m) SELECT 9, 'bogus' WHERE false`),
			difftest.S(0, `UPDATE p SET m = 'ok' WHERE false`),
			difftest.Q(0, `SELECT count(*) FROM (SELECT coalesce(m, 'sad') FROM p WHERE false) s`),
		},
	},
	{
		Name:   "enum values order by declaration",
		Schema: enumSchema, Seed: enumSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id FROM p ORDER BY m, id`),
			difftest.Q(0, `SELECT id FROM p ORDER BY m DESC NULLS LAST, id`),
			difftest.Q(0, `SELECT id FROM p WHERE m < 'happy' ORDER BY id`),
			difftest.Q(0, `SELECT id FROM p WHERE m BETWEEN 'sad' AND 'ok' ORDER BY id`),
			difftest.Q(0, `SELECT min(m)::text, max(m)::text FROM p`),
			difftest.Q(0, `SELECT greatest(m, 'ok')::text FROM p WHERE id = 2`),
			difftest.Q(0, `SELECT m::text AS label, count(*) FROM p GROUP BY m ORDER BY p.m`),
			difftest.Q(0, `SELECT id FROM p ORDER BY m::text, id`),
			difftest.Q(0, `SELECT id FROM p WHERE m IN ('sad', 'ok') ORDER BY id`),
			difftest.Q(0, `SELECT id FROM p ORDER BY row_number() OVER (ORDER BY m DESC), id LIMIT 2`),
		},
	},
	{
		Name:   "enum input refuses a value that is no label",
		Schema: enumSchema, Seed: enumSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.S(0, `INSERT INTO p VALUES (5, 'bogus', 'e')`),
			difftest.Q(0, `SELECT id FROM p WHERE m = 'bogus'`),
			difftest.S(0, `UPDATE p SET m = 'Happy' WHERE id = 1`),
			difftest.Q(0, `SELECT 'x'::mood::text`),
			difftest.Q(0, `SELECT id FROM p WHERE m = 'ok'`),
			difftest.Q(0, `SELECT coalesce(m, 'sad')::text FROM p WHERE id = 4`),
			difftest.Q(0, `SELECT m::text AS label FROM (SELECT m FROM p WHERE id < 3 UNION SELECT 'ok') u ORDER BY u.m`),
			difftest.Q(0, `SELECT m FROM p WHERE false UNION SELECT 'bogus'`),
			difftest.Q(0, `SELECT id FROM p WHERE m = 'ok'::public.mood`),
			difftest.Q(0, `SELECT 'bogus'::public.mood::text`),
		},
	},
	{
		Name: "enum values compare with each other and through keys",
		Schema: append(append([]string{}, enumSchema...),
			`CREATE TABLE allowed (m mood PRIMARY KEY)`,
			`CREATE TABLE t (id int PRIMARY KEY, m mood REFERENCES allowed (m), CHECK (m <> 'sad'))`,
		),
		Seed:  append(append([]string{}, enumSeed...), `INSERT INTO allowed VALUES ('ok'), ('happy')`),
		Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT a.id, b.id FROM p a JOIN p b ON a.m < b.m ORDER BY a.id, b.id`),
			difftest.S(0, `INSERT INTO t VALUES (1, 'ok')`),
			difftest.S(0, `INSERT INTO t VALUES (2, 'sad')`),
			difftest.S(0, `INSERT INTO allowed VALUES ('sad')`),
			difftest.S(0, `INSERT INTO t VALUES (3, 'sad')`),
			difftest.Q(0, `SELECT t.id FROM t JOIN p ON p.m = t.m ORDER BY t.id`),
		},
	},
	{
		Name: "enum labels added before and renamed",
		Schema: append(append([]string{}, enumSchema...),
			`ALTER TYPE mood ADD VALUE 'meh' BEFORE 'ok'`,
			`ALTER TYPE mood ADD VALUE IF NOT EXISTS 'ecstatic'`,
			`ALTER TYPE mood RENAME VALUE 'sad' TO 'blue'`,
		),
		Seed:  []string{`INSERT INTO p VALUES (1, 'happy', 'a'), (2, 'blue', 'b'), (3, 'ok', 'c'), (4, 'meh', 'd'), (5, 'ecstatic', 'e')`},
		Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id, m::text AS label FROM p ORDER BY p.m`),
			difftest.S(0, `INSERT INTO p VALUES (6, 'sad', 'f')`),
		},
	},
}

func TestDiffEnum(t *testing.T) {
	for _, c := range enumCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			difftest.Run(t, backend{}, c)
		})
	}
}
