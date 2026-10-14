package detest

import (
	"fmt"
	"maps"
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
func (db *DB) applyEnum(ch sqlir.SchemaChange) error {
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
	case ch.RenameLabel != [2]string{}:
		if table := db.enumKeyedRows(name); table != "" {
			// A row is stored under its key's text, which the rename
			// would leave at the old label.
			return unsupported(fmt.Sprintf("ALTER TYPE %s RENAME VALUE while %s, whose primary key is of it, holds rows", name, relname(table)), "")
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

// enumKeyedRows is a table that holds rows and has a column of the enum
// name in its primary key, or empty when none does.
func (db *DB) enumKeyedRows(name string) string {
	for _, table := range slices.Sorted(maps.Keys(db.defs)) {
		def := db.defs[table]
		if len(db.committed[table]) > 0 && slices.ContainsFunc(def.pk, func(c string) bool { return def.types[c] == name }) {
			return table
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
