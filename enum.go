package detest

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// enumType is a Postgres enum: its labels in the order the type sorts them.
// A value holds its label by pointer, so a label renamed or one added
// before it changes the values already stored, as Postgres stores a label's
// identity rather than its text.
type enumType struct {
	name      string
	qualified string // the schema-qualified name the type was created as
	labels    []*enumLabel
}

type enumLabel struct{ name string }

// enumValue is a value of an enum. It is not a string, so that untyped text
// compared with it is read by the enum's input, and text from a text
// expression is told apart, as Postgres has no operator comparing text with
// an enum. label is nil in the sample typeSample gives.
type enumValue struct {
	typ   *enumType
	label *enumLabel
}

func (v enumValue) String() string { return v.label.name }

func (t *enumType) lookup(s string) *enumLabel {
	i := slices.IndexFunc(t.labels, func(l *enumLabel) bool { return l.name == s })
	if i < 0 {
		return nil
	}
	return t.labels[i]
}

// compareEnums orders two values of one enum by the position of their
// labels; values of two enums do not compare.
func compareEnums(a, b enumValue) (int, bool) {
	if a.typ != b.typ {
		return 0, false
	}
	return slices.Index(a.typ.labels, a.label) - slices.Index(a.typ.labels, b.label), true
}

// enumOf is the enum typ names, nil when it names none.
func (db *DB) enumOf(typ string) *enumType {
	if db.kind.InnoDB() {
		return nil
	}
	return db.enums[typ]
}

// enumInput reads v as a value of t, as Postgres's enum input reads text:
// the label of that name, case and spaces included, or 22P02, naming the
// table and the column written to, if any.
func (db *DB) enumInput(t *enumType, v any, table, col string) (any, error) {
	switch v := derefValue(v).(type) {
	case nil:
		return nil, nil
	case enumValue:
		if v.typ == t {
			return v, nil
		}
		return nil, unsupported(fmt.Sprintf("a value of enum %s written as enum %s", v.typ.name, t.name), "")
	case []byte:
		return db.enumInput(t, string(v), table, col)
	case string:
		if l := t.lookup(v); l != nil {
			return enumValue{t, l}, nil
		}
		return nil, db.kind.Error(sqlir.InvalidTextRepresentation, fmt.Sprintf("invalid input value for enum %s: %q", t.name, v), table, col, "")
	}
	return nil, unsupported(fmt.Sprintf("a %T read as enum %s", v, t.name), "")
}

// applyEnum runs CREATE TYPE ... AS ENUM, ALTER TYPE and DROP TYPE of an
// enum. A statement Postgres fails here fails the schema, whose error no
// process sees, so it carries no SQLSTATE. Enums are known by their bare name, as a column's type is, so a
// second enum of a name in another schema is refused rather than taken for
// the first.
func (db *DB) applyEnum(ch sqlir.SchemaChange, tx *Tx) error {
	name := ch.Columns[0].Name
	t := db.enums[name]
	switch {
	case ch.Create:
		if t != nil {
			return unsupported(fmt.Sprintf("a second enum named %q", name), "")
		}
		if _, ok := db.domainBases[name]; ok {
			return unsupported(fmt.Sprintf("an enum named %q, as a domain is", name), "")
		}
		if db.otherTypes[name] {
			// The mark stays once another type had the name, as detest
			// cannot tell which schema a DROP of it reached.
			return unsupported(fmt.Sprintf("an enum named %q, as another type is or was", name), "")
		}
		for _, rel := range slices.Concat(slices.Collect(maps.Keys(db.defs)), slices.Collect(maps.Keys(db.views)), slices.Collect(maps.Keys(db.matviews))) {
			if relname(rel) == name {
				return unsupported(fmt.Sprintf("an enum named %q, as a relation is, whose row type has the name", name), "")
			}
		}
		t = &enumType{name: name, qualified: db.qualifyDomain(ch.Table, name)}
		for _, l := range ch.Labels {
			if t.lookup(l) != nil {
				return fmt.Errorf("detest: enum label %q used more than once", l)
			}
			t.labels = append(t.labels, &enumLabel{l})
		}
		if db.enums == nil {
			db.enums = map[string]*enumType{}
		}
		db.enums[name] = t
		return nil
	case t == nil || !db.namesEnum(t, ch.Table, name):
		// Another kind of type, which detest does not keep, or an enum of
		// the name in another schema than the one detest knows, which
		// Postgres does not find: DROP TYPE IF EXISTS of it does nothing,
		// and any other statement on it fails the schema.
		return nil
	case ch.Drop:
		if ch.Cascade {
			// Postgres drops the columns of the type with it, which detest
			// does not record.
			return unsupported(fmt.Sprintf("DROP TYPE %s CASCADE", name), "")
		}
		if db.enumUsed(name) {
			return unsupported(fmt.Sprintf("DROP TYPE %s, which a column has", name), "")
		}
		delete(db.enums, name)
		return nil
	case ch.NewSchema != "":
		t.qualified = ch.NewSchema + "." + name
		return nil
	case ch.RenameLabel != [2]string{}:
		if db.keepsText(ch.RenameLabel[0]) {
			// An expression the schema keeps holds the label as text, as
			// it was written, which the rename would leave at the old one.
			return unsupported(fmt.Sprintf("ALTER TYPE %s RENAME VALUE %q, which an expression of the schema names", name, ch.RenameLabel[0]), "")
		}
		if table := db.enumRows(t, tx); table != "" {
			// A row may be stored under its values' text, by its key or by
			// all its values in a table without one, which the rename
			// would leave at the old label.
			return unsupported(fmt.Sprintf("ALTER TYPE %s RENAME VALUE while %s holds a value of it", name, relname(table)), "")
		}
		l := t.lookup(ch.RenameLabel[0])
		if l == nil {
			return fmt.Errorf("detest: %q is not an existing enum label", ch.RenameLabel[0])
		}
		if t.lookup(ch.RenameLabel[1]) != nil {
			return fmt.Errorf("detest: enum label %q already exists", ch.RenameLabel[1])
		}
		l.name = ch.RenameLabel[1]
		return nil
	}
	if t.lookup(ch.AddLabel) != nil {
		if ch.IfNotExists {
			return nil
		}
		return fmt.Errorf("detest: enum label %q already exists", ch.AddLabel)
	}
	at := len(t.labels)
	if ch.Neighbor != "" {
		n := t.lookup(ch.Neighbor)
		if n == nil {
			return fmt.Errorf("detest: %q is not an existing enum label", ch.Neighbor)
		}
		at = slices.Index(t.labels, n)
		if ch.AfterNeighbor {
			at++
		}
	}
	t.labels = slices.Insert(t.labels, at, &enumLabel{ch.AddLabel})
	return nil
}

// namesEnum reports whether table, the name a DROP or an ALTER gives a type
// named bare, is t: the qualified name t was created as, or a bare name
// Postgres finds t by, its schema being on the search path.
func (db *DB) namesEnum(t *enumType, table, bare string) bool {
	if table != bare && strings.HasSuffix(table, "."+bare) {
		return table == t.qualified
	}
	return slices.Contains(db.searchPath(), strings.TrimSuffix(t.qualified, "."+bare))
}

// enumRows is a table with a row, committed or written by tx, that holds a
// value of t, or empty when none does.
func (db *DB) enumRows(t *enumType, tx *Tx) string {
	holds := func(r Row) bool {
		for _, v := range r {
			if e, ok := v.(enumValue); ok && e.typ == t {
				return true
			}
		}
		return false
	}
	for _, table := range slices.Sorted(maps.Keys(db.committed)) {
		for _, r := range db.committed[table] {
			if holds(r) {
				return table
			}
		}
	}
	if tx != nil {
		for lk, r := range tx.writes {
			if holds(r) {
				return lk.table
			}
		}
	}
	return ""
}

// enumUsed reports whether a column of a table or a domain has the enum
// name as its type.
func (db *DB) enumUsed(name string) bool {
	for _, def := range db.defs {
		for _, typ := range def.types {
			if typ == name {
				return true
			}
		}
	}
	for _, base := range db.domainBases {
		if base == name {
			return true
		}
	}
	return false
}

// keepsText reports whether an expression the schema keeps, a default, a
// CHECK, a generated column, an index or a view's query, holds the string
// constant s.
func (db *DB) keepsText(s string) bool {
	for _, v := range db.views {
		if hasText(v.View, s) {
			return true
		}
	}
	if hasText(db.matviews, s) {
		return true
	}
	for _, def := range db.defs {
		if hasText(def, s) {
			return true
		}
	}
	return false
}

// hasText reports whether n holds a string constant s.
func hasText(n any, s string) bool {
	constType := reflect.TypeFor[*sqlir.Const]()
	found := false
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		if found || !v.IsValid() {
			return
		}
		switch v.Kind() {
		case reflect.Pointer:
			if v.IsNil() {
				return
			}
			if v.Type() == constType {
				if c := v.Elem().FieldByName("Value"); !c.IsNil() && c.Elem().Kind() == reflect.String && c.Elem().String() == s {
					found = true
				}
				return
			}
			walk(v.Elem())
		case reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Struct:
			for _, f := range v.Fields() {
				walk(f)
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i))
			}
		case reflect.Map:
			for it := v.MapRange(); it.Next(); {
				walk(it.Value())
			}
		}
	}
	walk(reflect.ValueOf(n))
	return found
}
