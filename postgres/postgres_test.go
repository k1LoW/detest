package postgres

import (
	"testing"

	"github.com/k1LoW/detest/internal/sqlir"
)

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
