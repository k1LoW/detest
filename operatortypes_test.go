package detest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// Postgres has no arithmetic over a boolean, a uuid or text, and fails the
// statement when it plans it, whatever rows it reads.
func TestArithmeticOperandTypesBeforeAnyRow(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int, b boolean, u uuid, s text)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 0, true, NULL, 'x')`)
	for _, q := range []string{
		`SELECT coalesce(n, n + true) FROM t`,
		`SELECT CASE WHEN n = 0 THEN 1 ELSE n + true END FROM t`,
		`SELECT n + true FROM t WHERE false`,
		`SELECT n * b FROM t WHERE false`,
		`SELECT u - u FROM t WHERE false`,
		`SELECT s + 1 FROM t WHERE false`,
		`SELECT -b FROM t WHERE false`,
		`SELECT n + s::text FROM t WHERE false`,
	} {
		if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	// Numbers, and a literal that takes the other operand's type, run.
	for _, tt := range []struct {
		q    string
		want []string
	}{
		{`SELECT n + 1 FROM t`, []string{"1"}},
		{`SELECT -n FROM t`, []string{"0"}},
		{`SELECT coalesce(n, n + 1.5) FROM t`, []string{"0"}},
	} {
		if got := rowsOf(t, db, tt.q); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
}
