package detest

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// foldCase orders text ignoring case, and tells apart nothing else, so that
// a test can tell its order from byte order and see detest break its ties.
type foldCase struct{}

func (foldCase) Compare(a, b string) int {
	return strings.Compare(strings.ToLower(a), strings.ToLower(b))
}

// The database's collation orders the text that declares none, in every
// place that orders it, and equality stays byte equality.
func TestCollationOrdersText(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.Collation(foldCase{})))
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text NOT NULL)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'b'), (2, 'B'), (3, 'a'), (4, 'C')`)
	for _, tc := range []struct {
		q    string
		want []string
	}{
		// B and b tie under the collation and take byte order.
		{`SELECT id FROM t ORDER BY name`, []string{"3", "2", "1", "4"}},
		{`SELECT id FROM t ORDER BY name DESC`, []string{"4", "1", "2", "3"}},
		// C and c tie under the collation, and C sorts first by its bytes.
		{`SELECT id FROM t WHERE name < 'c' ORDER BY id`, []string{"1", "2", "3", "4"}},
		{`SELECT min(name), max(name) FROM t`, []string{"a,C"}},
		{`SELECT greatest('a', 'B'), least('b', 'C')`, []string{"B,b"}},
		{`SELECT id, row_number() OVER (ORDER BY name) FROM t ORDER BY id`, []string{"1,3", "2,2", "3,1", "4,4"}},
		{`SELECT id FROM t WHERE (name, id) > ('b', 0) ORDER BY id`, []string{"1", "4"}},
		{`SELECT id FROM t WHERE name = 'b'`, []string{"1"}},
		// COLLATE "C" orders by bytes whatever the database's collation.
		{`SELECT id FROM t ORDER BY name COLLATE "C"`, []string{"2", "4", "3", "1"}},
		{`SELECT id FROM t ORDER BY name COLLATE pg_catalog."POSIX"`, []string{"2", "4", "3", "1"}},
		{`SELECT id FROM t WHERE name COLLATE "C" < 'a' ORDER BY id`, []string{"2", "4"}},
	} {
		if got := rowsOf(t, db, tc.q); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.q, got, tc.want)
		}
	}
}

// A collation a column declares orders it, and the ones a column or COLLATE
// names must be declared to order text by.
func TestNamedCollations(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.Collations(map[string]Collation{"en_US.utf8": foldCase{}})))
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text COLLATE "en_US.utf8", code text, other text COLLATE "de_DE.utf8")`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'b', 'b', 'b'), (2, 'B', 'B', 'B'), (3, 'a', 'a', 'a')`)
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{`SELECT id FROM t ORDER BY name`, []string{"3", "2", "1"}},
		{`SELECT id FROM t ORDER BY code`, []string{"2", "3", "1"}},
		{`SELECT id FROM t ORDER BY code COLLATE "en_US.utf8"`, []string{"3", "2", "1"}},
		{`SELECT id FROM t ORDER BY name COLLATE "C"`, []string{"2", "3", "1"}},
		// The column's collation outranks the database's in a comparison.
		{`SELECT id FROM t WHERE name < 'C' ORDER BY id`, []string{"1", "2", "3"}},
		{`SELECT id FROM t WHERE code < 'C' ORDER BY id`, []string{"2"}},
		{`SELECT name AS n FROM t ORDER BY n`, []string{"a", "B", "b"}},
		{`SELECT name FROM t ORDER BY 1`, []string{"a", "B", "b"}},
		{`SELECT DISTINCT ON (name) name FROM t ORDER BY name`, []string{"a", "B", "b"}},
	} {
		if got := rowsOf(t, db, tc.q); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.q, got, tc.want)
		}
	}
	for _, q := range []string{
		`SELECT id FROM t ORDER BY other`,
		`SELECT id FROM t ORDER BY code COLLATE "sv_SE.utf8"`,
		`SELECT id FROM t WHERE name COLLATE "C" < code COLLATE "en_US.utf8"`,
		`SELECT id FROM t WHERE name < other`,
		`SELECT s.name FROM (SELECT name FROM t) s ORDER BY s.name`,
		`SELECT name FROM t UNION SELECT code FROM t ORDER BY 1`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
	}
	// The column of a subquery orders as it did when it is not text.
	if got := rowsOf(t, db, `SELECT s.id FROM (SELECT id FROM t) s ORDER BY s.id DESC`); !reflect.DeepEqual(got, []string{"3", "2", "1"}) {
		t.Errorf("a subquery's integer column: got %v", got)
	}
	// Reading or matching the column of an undeclared collation does not
	// order it.
	if got := rowsOf(t, db, `SELECT id FROM t WHERE other = 'B'`); !reflect.DeepEqual(got, []string{"2"}) {
		t.Errorf("equality on a column of an undeclared collation: got %v", got)
	}
}

func TestCheckSQLCollate(t *testing.T) {
	if err := CheckSQL(postgres.New(), `SELECT name FROM t ORDER BY name COLLATE "C"`); err != nil {
		t.Errorf("COLLATE \"C\": got %v", err)
	}
	if err := CheckSQL(postgres.New(), `SELECT name FROM t ORDER BY name COLLATE "en_US.utf8"`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("an undeclared collation: got %v, want unsupported", err)
	}
}
