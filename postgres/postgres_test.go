package postgres

import (
	"errors"
	"testing"

	"github.com/k1LoW/detest/internal/sqlir"
)

// SQL whose meaning detest would change by converting it must fail as
// unsupported, so a test never passes against a query that behaves
// differently on a real server.
func TestUnsupportedRatherThanApproximated(t *testing.T) {
	for _, q := range []string{
		`SELECT CURRENT_DATE`,
		`SELECT CURRENT_USER`,
		`SELECT * FROM t WHERE created_by = SESSION_USER`,
		`ALTER TABLE t ALTER COLUMN b DROP EXPRESSION`,
		`CREATE TABLE t (a int, b int GENERATED ALWAYS AS (nextval('s')) STORED)`,
		`CREATE TABLE t (a int, b timestamptz GENERATED ALWAYS AS (now()) STORED)`,
		`CREATE TABLE t (a int, b int GENERATED ALWAYS AS ((SELECT 1)) STORED)`,
		`CREATE TABLE t (a text, b text GENERATED ALWAYS AS (string_agg(a, ',')) STORED)`,
		`CREATE TABLE t (a int, b int GENERATED ALWAYS AS (sum(a ORDER BY a)) STORED)`,
		`CREATE TABLE t (a int, b int GENERATED ALWAYS AS (count(*) FILTER (WHERE a > 0)) STORED)`,
		`CREATE TABLE t (a int, b int DEFAULT 1 GENERATED ALWAYS AS (a * 2) STORED)`,
		`CREATE TABLE t (a int, b int GENERATED ALWAYS AS IDENTITY GENERATED ALWAYS AS (a * 2) STORED)`,
		`ALTER TABLE t ALTER COLUMN b SET EXPRESSION AS (a * 3)`,
		`SELECT * FROM a NATURAL JOIN b`,
		`SELECT * FROM a NATURAL LEFT JOIN b`,
		`UPDATE t SET tags[1] = 'x'`,
		`UPDATE t SET addr.city = 'x'`,
		`INSERT INTO t (tags[1]) VALUES ('x')`,
		`INSERT INTO t (id, tags) VALUES (1, '{}') ON CONFLICT (id) DO UPDATE SET tags[1] = 'x'`,
		`SELECT count(*) FILTER (WHERE done) FROM jobs`,
		`SELECT string_agg(name, ',' ORDER BY name) FROM t`,
		`SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY x) FROM t`,
		`SELECT * FROM t WHERE a > ANY (ARRAY[1, 2])`,
		`SELECT * FROM t WHERE a = ALL (ARRAY[1, 2])`,
		`SELECT * FROM t WHERE a = ANY ($1)`,
		`SELECT * FROM t WHERE a = ANY (ARRAY[]::int[])`,
		`SELECT * FROM t WHERE a <> ALL (ARRAY[]::int[])`,
		`SELECT * FROM t WHERE a = ANY (ARRAY[1, 1/0])`,
		`SELECT * FROM t WHERE a = ANY (ARRAY[b, c])`,
		`SELECT * FROM t WHERE a = ANY ((ARRAY['01', '02'])::int[])`,
		`SELECT * FROM t WHERE a = ANY ((ARRAY['a '])::bpchar[])`,
		`SELECT * FROM t WHERE a = ANY ((ARRAY['ab'])::varchar(1)[])`,
		`SELECT * FROM t WHERE a = ANY (ARRAY['ab'::varchar(1)])`,
		`SELECT * FROM t WHERE a = ANY (ARRAY['1'::int])`,
		`SELECT * FROM t WHERE a = ANY (ARRAY['a']::text)`,
		`SELECT * FROM t WHERE a = ANY ((ARRAY[1.20])::text[])`,
		`SELECT * FROM t WHERE a = ANY ((ARRAY['a'])::app.text[])`,
		`SELECT * FROM t WHERE a = ANY ((ARRAY['a'])::"TEXT"[])`,
		`SELECT * FROM t WHERE a = ANY (ARRAY[1.20::text])`,
		`SELECT * FROM t WHERE a = ANY (ARRAY['{a}'::text[]])`,
		`SELECT 1 UNION SELECT 2 FOR UPDATE`,
		`(SELECT 1 FROM t FOR UPDATE) UNION SELECT 2`,
		`VALUES (1) FOR UPDATE`,
	} {
		_, err := parser{}.Parse(q)
		if !errors.As(err, new(*sqlir.ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
	}
}

func TestStillSupported(t *testing.T) {
	for _, q := range []string{
		`SELECT CURRENT_TIMESTAMP`,
		`SELECT LOCALTIMESTAMP(3)`,
		`SELECT * FROM a CROSS JOIN b`,
		`SELECT * FROM a JOIN b ON a.id = b.a_id`,
		`UPDATE t SET name = 'x'`,
		`SELECT count(DISTINCT a) FROM t`,
		`CREATE TABLE t (a int, b int GENERATED ALWAYS AS (a * 2) STORED)`,
		`CREATE TABLE t (a text, b text GENERATED ALWAYS AS (lower(coalesce(a, ''))) STORED)`,
		`SELECT * FROM t WHERE a = ANY (ARRAY[$1, -1, 'x'::text])`,
	} {
		if _, err := (parser{}).Parse(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}

// pg_dump writes CHECK (status IN ('a', 'b')) as = ANY over an array, which
// must still become a check and not be dropped as one detest cannot evaluate.
func TestCheckInDumpForm(t *testing.T) {
	for _, tc := range []struct {
		query string
		not   bool
	}{
		{`CREATE TABLE t (status character varying, CONSTRAINT t_status_check CHECK (((status)::text = ANY ((ARRAY['a'::character varying, 'b'::character varying])::text[]))))`, false},
		{`CREATE TABLE t (status text, CONSTRAINT t_status_check CHECK ((status <> ALL (ARRAY['x'::text, 'y'::text]))))`, true},
	} {
		st, err := parser{}.Parse(tc.query)
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		schema, ok := st.(*sqlir.SchemaStmt)
		if !ok {
			t.Fatalf("%s: got %T", tc.query, st)
		}
		changes := schema.Changes
		if len(changes) != 1 || len(changes[0].Checks) != 1 {
			t.Fatalf("%s: checks %#v", tc.query, changes)
		}
		in, ok := changes[0].Checks[0].Expr.(*sqlir.InExpr)
		if !ok || len(in.List) != 2 || in.Not != tc.not {
			t.Errorf("%s: got %#v", tc.query, changes[0].Checks[0].Expr)
		}
	}
}

// A cast of the array casts each element, so it stays on the elements.
func TestAnyKeepsArrayCast(t *testing.T) {
	st, err := parser{}.Parse(`SELECT 1 WHERE n = ANY ((ARRAY['1', '2'])::text[])`)
	if err != nil {
		t.Fatal(err)
	}
	sel, ok := st.(*sqlir.SelectStmt)
	if !ok {
		t.Fatalf("got %T", st)
	}
	in, ok := sel.Where.(*sqlir.InExpr)
	if !ok || len(in.List) != 2 {
		t.Fatalf("got %#v", sel.Where)
	}
	for _, e := range in.List {
		if c, ok := e.(*sqlir.Cast); !ok || c.Type != "text" {
			t.Errorf("element %#v, want a cast to text", e)
		}
	}
}
