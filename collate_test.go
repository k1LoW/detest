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
	// A * keeps no collation detest can tell, so the table loads and
	// ordering its text is refused.
	mustExec(t, db, `CREATE TABLE w AS SELECT * FROM t WHERE name COLLATE "C" > ''`)
	if _, err := db.Exec(`SELECT id FROM w ORDER BY name`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("ordering a CREATE TABLE AS column of a *: got %v, want unsupported", err)
	}
	if got := rowsOf(t, db, `SELECT id FROM w ORDER BY id`); !reflect.DeepEqual(got, []string{"1", "2", "3"}) {
		t.Errorf("ordering a non-text CREATE TABLE AS column of a *: got %v", got)
	}
	// bytea orders byte by byte, under any collation.
	mustExec(t, db, `CREATE TABLE b (id int PRIMARY KEY, payload bytea)`)
	mustExec(t, db, `INSERT INTO b VALUES (1, $1), (2, $2), (3, $3), (4, $4)`, []byte("b"), []byte("B"), []byte{9}, []byte{10})
	if got := rowsOf(t, db, `SELECT id FROM b ORDER BY payload`); !reflect.DeepEqual(got, []string{"3", "4", "2", "1"}) {
		t.Errorf("ORDER BY a bytea column: got %v", got)
	}
	// a over B by bytes, B over a under the case-folding collation.
	mustExec(t, db, `INSERT INTO b VALUES (5, $1)`, []byte("a"))
	if got := rowsOf(t, db, `SELECT greatest(x.payload, y.payload) = x.payload FROM b x, b y WHERE x.id = 5 AND y.id = 2`); !reflect.DeepEqual(got, []string{"true"}) {
		t.Errorf("greatest of bytea: got %v, want a, the greater by bytes", got)
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
	// Columns that cannot be text need no collation, whatever the query's
	// shape, on a schema that declares collations.
	mustExec(t, db, `CREATE TABLE decl (s text COLLATE "C")`)
	mustExec(t, db, `CREATE TABLE ints AS SELECT 1 AS n UNION SELECT 2`)
	mustExec(t, db, `CREATE TABLE vals AS VALUES (1, true), (2, false)`)
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
	// IF NOT EXISTS skips a relation of any kind, a view and a unique index
	// among them.
	mustExec(t, db, `CREATE UNIQUE INDEX uix ON t (id)`)
	mustExec(t, db, `CREATE TABLE IF NOT EXISTS uix AS SELECT id FROM t`)
	if _, ok := store.defs[store.resolve("uix")]; ok {
		t.Error("CREATE TABLE IF NOT EXISTS ... AS made a table of a unique index's name")
	}
	mustExec(t, db, `CREATE TABLE IF NOT EXISTS t_pkey AS SELECT id FROM t`)
	if _, ok := store.defs[store.resolve("t_pkey")]; ok {
		t.Error("CREATE TABLE IF NOT EXISTS ... AS made a table of a primary key index's name")
	}
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

// A cast to name brings name's C collation into a statement where nothing
// else sets one: a derived column of it, which detest does not carry, is
// refused, and a CREATE TABLE AS column of it keeps C.
func TestNameCastCollation(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.Collation(foldCase{})))
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'b'), (2, 'B'), (3, 'a')`)
	if _, err := db.Exec(`SELECT x.n FROM (SELECT name::name AS n FROM t) x ORDER BY x.n`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("ordering a derived column cast to name: got %v, want unsupported", err)
	}
	mustExec(t, db, `CREATE TABLE nm AS SELECT id, name::name AS n FROM t`)
	if got := rowsOf(t, db, `SELECT id FROM nm ORDER BY n`); !reflect.DeepEqual(got, []string{"2", "3", "1"}) {
		t.Errorf("a CREATE TABLE AS column cast to name: got %v, want C order", got)
	}
}

// Text written or compared as []byte, as database/sql may pass a string,
// orders by the collation as text does, while bytea keeps byte order.
func TestBytesAsText(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.Collation(foldCase{})))
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text, payload bytea)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, $1, $1), (2, $2, $2)`, []byte("a"), []byte("B"))
	// A bytea literal, which detest may hold as a string, and a CREATE
	// TABLE AS of it keep byte order.
	mustExec(t, db, `CREATE TABLE lit (id int PRIMARY KEY, payload bytea)`)
	mustExec(t, db, `INSERT INTO lit VALUES (1, 'a'), (2, 'B')`)
	mustExec(t, db, `CREATE TABLE copied AS SELECT id, payload FROM lit`)
	mustExec(t, db, `CREATE TABLE mixed (id int PRIMARY KEY, payload bytea)`)
	mustExec(t, db, `INSERT INTO mixed VALUES (1, 'a'), (2, 'B')`)
	mustExec(t, db, `INSERT INTO mixed VALUES (3, $1)`, []byte("z"))
	var raw []byte
	if err := db.QueryRow(`SELECT payload FROM lit WHERE id = 1`).Scan(&raw); err != nil || string(raw) != "a" {
		t.Errorf("a bytea literal read back: got %q, %v", raw, err)
	}
	// A CREATE TABLE AS column of bytea is bytea, by the query's type or,
	// for a *, by its values, so a later bytea literal written to it is
	// read as bytea input.
	mustExec(t, db, `CREATE TABLE copied2 AS SELECT payload FROM lit`)
	mustExec(t, db, `CREATE TABLE starred AS SELECT * FROM lit`)
	for _, tbl := range []string{"copied2", "starred"} {
		mustExec(t, db, `INSERT INTO `+tbl+` (payload) VALUES ('\x41')`)
		var back []byte
		if err := db.QueryRow(`SELECT payload FROM ` + tbl + ` WHERE payload = 'A'::bytea`).Scan(&back); err != nil || string(back) != "A" {
			t.Errorf("bytea literal written to the CREATE TABLE AS table %s: got %q, %v", tbl, back, err)
		}
	}
	// Without rows, a * and a set operation still tell a bytea column by
	// the query's types, and a column neither they nor its values tell
	// takes no text.
	mustExec(t, db, `CREATE TABLE empty1 AS SELECT * FROM lit WHERE false`)
	mustExec(t, db, `CREATE TABLE empty2 AS SELECT NULL AS payload UNION SELECT payload FROM lit WHERE false`)
	mustExec(t, db, `CREATE TABLE empty3 AS SELECT l.* FROM lit l WHERE false`)
	for _, tbl := range []string{"empty1", "empty2", "empty3"} {
		mustExec(t, db, `INSERT INTO `+tbl+` (payload) VALUES ('\x41')`)
		if got := rowsOf(t, db, `SELECT count(*) FROM `+tbl+` WHERE payload = 'A'::bytea`); !reflect.DeepEqual(got, []string{"1"}) {
			t.Errorf("bytea literal written to the empty CREATE TABLE AS table %s: got %v", tbl, got)
		}
	}
	mustExec(t, db, `CREATE TABLE untold AS SELECT * FROM (SELECT payload FROM lit UNION SELECT payload FROM lit) s WHERE false`)
	if _, err := db.Exec(`INSERT INTO untold VALUES ('\x41')`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("text written to a CREATE TABLE AS column of untold type: got %v, want unsupported", err)
	}
	mustExec(t, db, `INSERT INTO untold VALUES ($1), (NULL)`, []byte("A"))
	if got := rowsOf(t, db, `SELECT count(*) FROM untold WHERE payload IS NULL`); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("a CREATE TABLE AS column of untold type read: got %v", got)
	}
	// Two schemas' domains of one name with different base types leave a
	// column of the name untyped, so a write to it is refused.
	mustExec(t, db, `CREATE SCHEMA a`)
	mustExec(t, db, `CREATE SCHEMA b`)
	mustExec(t, db, `CREATE DOMAIN a.twice AS bytea`)
	mustExec(t, db, `CREATE DOMAIN b.twice AS text`)
	mustExec(t, db, `CREATE TABLE amb (id int PRIMARY KEY, v a.twice)`)
	if _, err := db.Exec(`INSERT INTO amb VALUES (1, 'x')`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a write to a column of a domain two schemas declare: got %v, want unsupported", err)
	}
	if _, err := db.Exec(`SELECT '\x41'::a.twice`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a cast to a domain two schemas declare: got %v, want unsupported", err)
	}
	// A drop naming another schema's domain of the name leaves this one.
	mustExec(t, db, `CREATE DOMAIN blob AS bytea`)
	mustExec(t, db, `DROP DOMAIN IF EXISTS b.blob`)
	mustExec(t, db, `CREATE TABLE kept (id int PRIMARY KEY, v blob)`)
	mustExec(t, db, `INSERT INTO kept VALUES (1, '\x41')`)
	if got := rowsOf(t, db, `SELECT id FROM kept WHERE v = 'A'::bytea`); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("a bytea domain after a drop of another schema's: got %v", got)
	}
	// Two schemas' domains of one name and base type stay typed until
	// both are dropped.
	mustExec(t, db, `CREATE DOMAIN a.blob2 AS bytea`)
	mustExec(t, db, `CREATE DOMAIN b.blob2 AS bytea`)
	mustExec(t, db, `DROP DOMAIN b.blob2`)
	mustExec(t, db, `CREATE TABLE kept2 (id int PRIMARY KEY, v a.blob2)`)
	mustExec(t, db, `INSERT INTO kept2 VALUES (1, '\x41')`)
	if got := rowsOf(t, db, `SELECT id FROM kept2 WHERE v = 'A'::bytea`); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("a bytea domain after a drop of another schema's of the same base: got %v", got)
	}
	mustExec(t, db, `CREATE DOMAIN bytes AS bytea`)
	mustExec(t, db, `CREATE TABLE dom (id int PRIMARY KEY, payload bytes)`)
	mustExec(t, db, `INSERT INTO dom VALUES (1, 'a'), (2, 'B')`)
	for _, tc := range []struct {
		q    string
		args []any
		want []string
	}{
		{`SELECT id FROM t ORDER BY name`, nil, []string{"1", "2"}},
		{`SELECT id FROM t ORDER BY payload`, nil, []string{"2", "1"}},
		{`SELECT id FROM t WHERE name < $1`, []any{[]byte("B")}, []string{"1"}},
		{`SELECT min(name) FROM t`, nil, []string{"a"}},
		// A bytea literal or cast orders by bytes too, though detest may
		// hold it as a string.
		{`SELECT greatest('a'::bytea, 'B'::bytea) = 'a'::bytea`, nil, []string{"true"}},
		{`SELECT 'B'::bytea < 'a'::bytea`, nil, []string{"true"}},
		{`SELECT id FROM lit ORDER BY payload`, nil, []string{"2", "1"}},
		{`SELECT id FROM copied ORDER BY payload`, nil, []string{"2", "1"}},
		{`SELECT id FROM dom ORDER BY payload`, nil, []string{"2", "1"}},
		// A literal and a []byte parameter of one value are one value, and
		// rows written either way order by bytes together.
		{`SELECT id FROM lit WHERE payload = $1`, []any{[]byte("B")}, []string{"2"}},
		{`SELECT id FROM lit WHERE payload = '\x42'`, nil, []string{"2"}},
		{`SELECT id FROM mixed ORDER BY payload`, nil, []string{"2", "1", "3"}},
		{`SELECT id FROM t WHERE name < $1 COLLATE "default"`, []any{[]byte("B")}, []string{"1"}},
		{`SELECT min(name COLLATE "default") < $1 COLLATE "default" FROM t`, []any{[]byte("B")}, []string{"true"}},
	} {
		if got := rowsOf(t, db, tc.q, tc.args...); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.q, got, tc.want)
		}
	}
}

// A collation ends at a result that has none: length's integer drops the C
// of its argument, so its text orders by the database's collation.
func TestCollationEndsAtNonText(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.Collation(foldCase{})))
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'b'), (2, 'B'), (3, 'a')`)
	if got := rowsOf(t, db, `SELECT id FROM t ORDER BY length(name COLLATE "C")::text || name`); !reflect.DeepEqual(got, []string{"3", "2", "1"}) {
		t.Errorf("text made from length of a C operand: got %v, want the database's order", got)
	}
}

// A collation declared with a nil comparator is not declared.
func TestNilNamedCollation(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.Collations(map[string]Collation{"fold": nil})))
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text COLLATE "fold")`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'b'), (2, 'B')`)
	if _, err := db.Exec(`SELECT id FROM t ORDER BY name`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("ordering by a collation declared nil: got %v, want unsupported", err)
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
	// A CHECK without COLLATE orders by its column's collation.
	mustExec(t, db, `CREATE TABLE c (id int PRIMARY KEY, name text COLLATE "fold" CHECK (name < 'm'))`)
	mustExec(t, db, `INSERT INTO c VALUES (1, 'B')`)
	if _, err := db.Exec(`INSERT INTO c VALUES (2, 'Z')`); !errors.Is(err, ErrCheckViolation) {
		t.Errorf("a CHECK of a column of fold: got %v, want a check violation", err)
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

// lower, upper and ILIKE fold case by the ctype of their collation, which a
// Collation does not tell, so text under a declared collation other than C
// and POSIX, or of a subquery where a column declares one, is refused. The
// database's and C's are checked against Postgres in internal/difftest.
func TestCaseFoldingCollations(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.Collations(map[string]Collation{"en_US.utf8": foldCase{}})))
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text COLLATE "en_US.utf8", code text COLLATE "C", plain text)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'Äb', 'Äb', 'Äb')`)
	for _, q := range []string{
		`SELECT lower(name) FROM t`,
		`SELECT upper(plain COLLATE "en_US.utf8") FROM t`,
		`SELECT id FROM t WHERE name ILIKE 'äb'`,
		`SELECT id FROM t WHERE plain NOT ILIKE 'x' COLLATE "en_US.utf8"`,
		`SELECT lower(s.code) FROM (SELECT code FROM t) s`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want unsupported", q, err)
		}
	}
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{`SELECT lower(code) FROM t`, []string{"Äb"}},
		{`SELECT lower(plain) FROM t`, []string{"äb"}},
		{`SELECT lower(name) IS NULL FROM t WHERE false`, nil},
		{`SELECT id FROM t WHERE code ILIKE 'ÄB'`, []string{"1"}},
		{`SELECT count(*) FROM t WHERE code ILIKE 'äb'`, []string{"0"}},
	} {
		if got := rowsOf(t, db, tc.q); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.q, got, tc.want)
		}
	}
}
