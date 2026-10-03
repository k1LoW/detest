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
		`CREATE TABLE t (a int, b int GENERATED ALWAYS AS (a * 2) STORED)`,
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
		changes := st.(*sqlir.SchemaStmt).Changes
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
	st, err := parser{}.Parse(`SELECT 1 WHERE n = ANY ((ARRAY['01', '02'])::int[])`)
	if err != nil {
		t.Fatal(err)
	}
	in, ok := st.(*sqlir.SelectStmt).Where.(*sqlir.InExpr)
	if !ok || len(in.List) != 2 {
		t.Fatalf("got %#v", st.(*sqlir.SelectStmt).Where)
	}
	for _, e := range in.List {
		if c, ok := e.(*sqlir.Cast); !ok || c.Type != "int4" {
			t.Errorf("element %#v, want a cast to int4", e)
		}
	}
}
