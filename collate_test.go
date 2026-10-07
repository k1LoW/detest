package detest

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
	"github.com/k1LoW/detest/internal/sqlir"
)

// foldCase orders text ignoring case, and tells apart nothing else, so that
// a test can tell its order from byte order and see detest break its ties.
type foldCase struct{}

func (foldCase) Compare(a, b string) int {
	return strings.Compare(strings.ToLower(a), strings.ToLower(b))
}

// reverse orders text against byte order.
type reverse struct{}

func (reverse) Compare(a, b string) int { return strings.Compare(b, a) }

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

// A collation qualified by a schema other than pg_catalog is declared with
// each part quoted, as two schemas may have collations of the same name and
// an unqualified collation may be named app.fold itself.
func TestSchemaQualifiedCollations(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.Collations(map[string]Collation{`"app"."fold"`: foldCase{}, "app.fold": reverse{}})))
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text COLLATE app.fold, code text)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'b', 'b'), (2, 'B', 'B'), (3, 'a', 'a')`)
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{`SELECT id FROM t ORDER BY name`, []string{"3", "2", "1"}},
		{`SELECT id FROM t ORDER BY code COLLATE app.fold`, []string{"3", "2", "1"}},
		// The unqualified collation named app.fold is another one.
		{`SELECT id FROM t ORDER BY code COLLATE "app.fold"`, []string{"1", "3", "2"}},
	} {
		if got := rowsOf(t, db, tc.q); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.q, got, tc.want)
		}
	}
	for _, q := range []string{
		`SELECT id FROM t ORDER BY code COLLATE fold`,
		`SELECT id FROM t ORDER BY code COLLATE "app"."other.fold"`,
		`SELECT id FROM t ORDER BY code COLLATE other.fold`,
		`SELECT id FROM t ORDER BY code COLLATE app."C"`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
	}
}

// A COLLATE inside a subquery is not carried to its column, so ordering the
// column's text is refused, and the aggregates that do not order their
// argument do not ask for its collation.
func TestDerivedTextCollation(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.Collation(foldCase{})))
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'b'), (2, 'B')`)
	if _, err := db.Exec(`SELECT s.name FROM (SELECT name COLLATE "C" AS name FROM t) s ORDER BY s.name`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a COLLATE inside a subquery: got %v, want unsupported", err)
	}
	for _, q := range []string{
		`SELECT count(s.name), count(DISTINCT s.name) FROM (SELECT name COLLATE "C" AS name FROM t) s`,
		`SELECT count(s.name) OVER () FROM (SELECT name COLLATE "C" AS name FROM t) s`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}

// The collation of an expression comes from what gives its result: not from
// a window's PARTITION BY, nor through a boolean, and CREATE TABLE AS keeps
// the collation of each column it creates.
func TestCollationOfResults(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.Collation(foldCase{})))
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'b'), (2, 'B'), (3, 'a')`)
	mustExec(t, db, `CREATE TABLE u AS SELECT id, name COLLATE "C" AS name, name AS plain FROM t`)
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{`SELECT first_value(name) OVER (PARTITION BY name COLLATE "C") FROM t ORDER BY 1`, []string{"a", "B", "b"}},
		{`SELECT id FROM t ORDER BY concat((name COLLATE "C") IS NULL, name)`, []string{"3", "2", "1"}},
		{`SELECT id FROM u ORDER BY name`, []string{"2", "3", "1"}},
		{`SELECT id FROM u ORDER BY plain`, []string{"3", "2", "1"}},
	} {
		if got := rowsOf(t, db, tc.q); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.q, got, tc.want)
		}
	}
	if _, err := db.Exec(`CREATE TABLE w AS SELECT * FROM t WHERE name COLLATE "C" > ''`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("CREATE TABLE AS of a * with a COLLATE: got %v, want unsupported", err)
	}
	// A string argument that database/sql passes as []byte is text to
	// greatest and least, as to the other functions.
	if got := rowsOf(t, db, `SELECT greatest($1::text, $2::text)`, []byte("a"), []byte("B")); !reflect.DeepEqual(got, []string{"B"}) {
		t.Errorf("greatest of []byte arguments: got %v", got)
	}
}

// A table CREATE TABLE IF NOT EXISTS ... AS finds is left as it is, a
// number keeps no collation, and each statement of a script sees the
// collations the ones before it declared.
func TestCollationsAcrossStatements(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New(postgres.Collation(foldCase{})))
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'b'), (2, 'B'), (3, 'a')`)
	mustExec(t, db, `CREATE TABLE u AS SELECT id, name FROM t`)
	mustExec(t, db, `CREATE TABLE IF NOT EXISTS u AS SELECT id, name COLLATE "C" AS name FROM t`)
	mustExec(t, db, `CREATE TABLE n AS SELECT id, name, length(name COLLATE "C") AS n FROM t`)
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{`SELECT id FROM u ORDER BY name`, []string{"3", "2", "1"}},
	} {
		if got := rowsOf(t, db, tc.q); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.q, got, tc.want)
		}
	}
	if c, ok := store.defs[store.resolve("n")].collations["n"]; ok {
		t.Errorf("the integer column n of CREATE TABLE AS keeps collation %q", c)
	}
	_, err := db.Exec(`SELECT id FROM t ORDER BY name; CREATE TABLE d (name text COLLATE "C"); INSERT INTO d VALUES ('b'), ('B'); SELECT s.name FROM (SELECT name FROM d) s ORDER BY s.name`)
	if !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a script ordering a subquery's text after declaring a collation: got %v, want unsupported", err)
	}
	if !isValue(&sqlir.Collate{X: &sqlir.Param{Index: 1}, Name: "C"}) {
		t.Error("a parameter under COLLATE is not a value to sharedtx")
	}
}

// detest does not follow a domain's collation, so text of a domain that
// declares one, or is based on one that does, is not ordered, whether a
// column is of it or a cast makes text of it. A domain without one orders
// as text does. CREATE TABLE IF NOT EXISTS ... AS on any relation that
// exists does nothing.
func TestDomainCollations(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New(postgres.Collation(foldCase{})))
	mustExec(t, db, `CREATE DOMAIN tag AS text COLLATE "C"`)
	mustExec(t, db, `CREATE DOMAIN nested AS tag`)
	mustExec(t, db, `CREATE DOMAIN old AS text COLLATE "C"`)
	mustExec(t, db, `ALTER DOMAIN old RENAME TO renamed`)
	mustExec(t, db, `CREATE DOMAIN plain AS text`)
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text, a tag, n nested, r renamed, p plain)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'b', 'b', 'b', 'b', 'b'), (2, 'B', 'B', 'B', 'B', 'B'), (3, 'a', 'a', 'a', 'a', 'a')`)
	mustExec(t, db, `CREATE TABLE c AS SELECT id, name::tag AS name FROM t`)
	mustExec(t, db, `CREATE TABLE IF NOT EXISTS c AS SELECT id + 100 AS id, name FROM t`)
	for _, q := range []string{
		`SELECT id FROM t ORDER BY a`,
		`SELECT id FROM t ORDER BY n`,
		`SELECT id FROM t ORDER BY r`,
		`SELECT id FROM t ORDER BY name::tag`,
		`SELECT id FROM t WHERE a < 'b'`,
		`SELECT id FROM c ORDER BY name`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
	}
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{`SELECT id FROM t ORDER BY p`, []string{"3", "2", "1"}},
		{`SELECT id FROM t WHERE a = 'B'`, []string{"2"}},
		{`SELECT count(*) FROM c`, []string{"3"}},
	} {
		if got := rowsOf(t, db, tc.q); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.q, got, tc.want)
		}
	}
	// IF NOT EXISTS skips a relation of any kind, a view among them.
	mustExec(t, db, `CREATE VIEW vw AS SELECT id FROM t`)
	mustExec(t, db, `CREATE TABLE IF NOT EXISTS vw AS SELECT id, name FROM t`)
	if _, ok := store.defs[store.resolve("vw")]; ok {
		t.Error("CREATE TABLE IF NOT EXISTS ... AS made a table beside the view")
	}
}

// On a database of the C collation, a domain is the only thing that sets a
// collation, and a cast to it is still refused rather than ordered as C.
func TestDomainOnlyCollation(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.Collations(map[string]Collation{"fold": foldCase{}})))
	mustExec(t, db, `CREATE DOMAIN tag AS text COLLATE "fold"`)
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'b'), (2, 'B'), (3, 'a')`)
	if _, err := db.Exec(`SELECT id FROM t ORDER BY name::tag`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("ordering text cast to a collated domain: got %v, want unsupported", err)
	}
}

// A COLLATE a table keeps in a CHECK is evaluated on a later write, which
// orders by it rather than by bytes on a database of the C collation.
func TestPersistedCollate(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.Collations(map[string]Collation{"fold": foldCase{}})))
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text CHECK (name COLLATE "fold" < 'm'))`)
	// Under fold, 'B' sorts before 'm'; by bytes it does too, but 'Z' does
	// only by bytes, so fold refuses it where C would take it.
	mustExec(t, db, `INSERT INTO t VALUES (1, 'B')`)
	if _, err := db.Exec(`INSERT INTO t VALUES (2, 'Z')`); !errors.Is(err, ErrCheckViolation) {
		t.Errorf("a CHECK ordered by fold: got %v, want a check violation", err)
	}
}

// The built-in name type orders by C whatever the database's collation, as
// a column, through a cast and as a domain's base; and a quoted domain name
// holding a dot keeps its mark through a rename.
func TestNameTypeCollation(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.Collation(foldCase{})))
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n name, s text)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'b', 'b'), (2, 'B', 'B'), (3, 'a', 'a')`)
	mustExec(t, db, `CREATE TABLE u (id int PRIMARY KEY, n text)`)
	mustExec(t, db, `ALTER TABLE u ALTER COLUMN n TYPE name`)
	mustExec(t, db, `INSERT INTO u VALUES (1, 'b'), (2, 'B'), (3, 'a')`)
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{`SELECT id FROM t ORDER BY n`, []string{"2", "3", "1"}},
		{`SELECT id FROM t ORDER BY s::name`, []string{"2", "3", "1"}},
		{`SELECT id FROM t ORDER BY s`, []string{"3", "2", "1"}},
		{`SELECT id FROM u ORDER BY n`, []string{"2", "3", "1"}},
	} {
		if got := rowsOf(t, db, tc.q); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.q, got, tc.want)
		}
	}
	mustExec(t, db, `CREATE DOMAIN ident AS name`)
	mustExec(t, db, `CREATE DOMAIN "a.b" AS text COLLATE "C"`)
	mustExec(t, db, `ALTER DOMAIN "a.b" RENAME TO c`)
	for _, q := range []string{`SELECT id FROM t ORDER BY s::ident`, `SELECT id FROM t ORDER BY s::c`} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
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
