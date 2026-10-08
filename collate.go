package detest

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// colNote is the collation a column reference's column declares, empty for
// the database's, with known false for the column of a derived item.
type colNote struct {
	name  string
	known bool
}

// noteCollation records the collation the column r refers to declares, for
// a run to order its text by.
func (x *sqlExec) noteCollation(r *sqlir.ColumnRef, sc *colScope) {
	if x.tx.db.kind.InnoDB() {
		return
	}
	name, known := sc.columnCollation(r)
	if x.colls == nil {
		x.colls = map[*sqlir.ColumnRef]colNote{}
	}
	x.colls[r] = colNote{name: name, known: known}
}

// derivedCollation stands for the collation of a CREATE TABLE AS column
// detest could not tell, as of a * or a set operation, which ordering the
// column's text refuses, as for the column of a subquery.
const derivedCollation = "a derived column's collation"

// outputCollations are the collations of the n columns sel gives, empty for
// the database's, and which of them are bytea, which CREATE TABLE AS keeps
// on the columns it creates as Postgres does. A column it cannot tell gets
// derivedCollation, so that the table still loads and ordering its text is
// refused where it matters.
func (x *sqlExec) outputCollations(sel *sqlir.SelectStmt, n int) ([]string, error) {
	db := x.tx.db
	if db.kind.InnoDB() {
		return nil, nil
	}
	_, bytes := db.kind.TextCollation().(sqlir.ByteOrder)
	if bytes && !db.declared() && !x.collates {
		return nil, nil
	}
	names := make([]string, n)
	if sel.SetOp != "" || len(sel.Values) > 0 || len(sel.Targets) != n || slices.ContainsFunc(sel.Targets, func(t sqlir.Target) bool { return t.Star }) {
		if !textlessOutputs(sel, n) {
			for i := range names {
				names[i] = derivedCollation
			}
		}
		return names, nil
	}
	for i, t := range sel.Targets {
		if typ := expressionType(t.Expr, nil); typ != "" && !collatableType(typ) && !db.collatedDomains[typ] {
			continue // a number or a boolean has no collation to keep
		}
		u, err := x.collationOf(t.Expr)
		if err != nil {
			return nil, err
		}
		if u.unknown {
			names[i] = derivedCollation
			continue
		}
		names[i] = u.name
	}
	return names, nil
}

// domainCollation stands for the collation of a domain that declares one,
// which no collation is declared under, so that ordering by it is refused.
const domainCollation = "a domain's collation"

// domainCollation is domainCollation for a type that is a collated domain,
// and empty for any other.
func (db *DB) domainCollation(typ string) string {
	if db.collatedDomains[typ] {
		return domainCollation
	}
	return ""
}

// qualifyDomain is the qualified name of the domain named bare that table,
// the name a statement gives it, refers to: as given when a schema
// qualifies it, and in the first schema of the search path otherwise.
func (db *DB) qualifyDomain(table, bare string) string {
	if table != bare && strings.HasSuffix(table, "."+bare) {
		return table
	}
	return db.searchPath()[0] + "." + bare
}

// ambiguousDomain is the base type of a domain name two schemas declare
// domains of with different base types, which a write to a column of it
// refuses.
const ambiguousDomain = "a domain of a name two schemas declare with different base types"

// baseType is typ, or the base type of the domain typ names.
func (db *DB) baseType(typ string) string {
	if base, ok := db.domainBases[typ]; ok {
		return base
	}
	return typ
}

// typeCollation is the collation a built-in type declares instead of the
// database's: name's is C (typcollation in pg_type).
func typeCollation(typ string) string {
	if typ == "name" {
		return "C"
	}
	return ""
}

// collatableType reports whether a value of typ has a collation.
func collatableType(typ string) bool {
	switch typ {
	case "text", "varchar", "bpchar", "char", "character varying", "character", "name":
		return true
	}
	return false
}

// noteOutput records that the ORDER BY key k names or numbers the select
// list's expression e, whose collation orders it.
func (x *sqlExec) noteOutput(k, e sqlir.Expr) {
	if x.tx.db.kind.InnoDB() {
		return
	}
	if x.outputs == nil {
		x.outputs = map[sqlir.Expr]sqlir.Expr{}
	}
	x.outputs[k] = e
}

// collationUse is how an expression decides its collation, as Postgres
// derives it: an explicit COLLATE outranks the collation a column declares,
// which outranks the database's.
type collationUse struct {
	name     string
	explicit bool
	// unknown is an expression whose collation detest cannot tell, such as
	// the column of a subquery.
	unknown bool
}

// collationOf derives the collation of e from the COLLATE clauses and the
// column references in it, as Postgres derives one for an operator or a
// function from its operands.
func (x *sqlExec) collationOf(e sqlir.Expr) (collationUse, error) {
	switch e := e.(type) {
	case nil:
		return collationUse{}, nil
	case *sqlir.Collate:
		return collationUse{name: e.Name, explicit: true}, nil
	case *sqlir.ColumnRef:
		if out, ok := x.outputs[e]; ok && out != nil {
			return x.collationOf(out)
		}
		n, ok := x.colls[e]
		if !ok || !n.known {
			return collationUse{unknown: true}, nil
		}
		return collationUse{name: n.name}, nil
	case *sqlir.SubQuery, *sqlir.Exists:
		return collationUse{unknown: true}, nil
	case *sqlir.Param:
		return collationUse{}, nil
	case *sqlir.Const:
		if out, ok := x.outputs[e]; ok {
			if out == nil {
				return collationUse{unknown: true}, nil
			}
			return x.collationOf(out)
		}
		return collationUse{}, nil
	case *sqlir.WindowFunc:
		// The result is the function's; PARTITION BY and ORDER BY only
		// group and order the rows it reads.
		return x.collationOf(e.Func)
	case *sqlir.IsNull, *sqlir.InExpr, *sqlir.ArrayCmp, *sqlir.RowExpr:
		// A boolean or a row has no collation to pass on, whatever its
		// operands have.
		return collationUse{}, nil
	case *sqlir.UnaryExpr:
		if e.Op == "NOT" {
			return collationUse{}, nil
		}
	case *sqlir.BinaryExpr:
		switch e.Op {
		case "||":
		default:
			// The comparisons, LIKE, AND, OR and the arithmetic give a
			// boolean or a number; only || gives text.
			return collationUse{}, nil
		}
	case *sqlir.Cast:
		if c := x.tx.db.domainCollation(e.Type); c != "" {
			// A cast to a domain takes the domain's collation.
			return collationUse{name: c}, nil
		}
		if !collatableType(e.Type) {
			return collationUse{}, nil
		}
		if c := typeCollation(e.Type); c != "" {
			// A cast to name with no collation of its own takes name's.
			u, err := x.collationOf(e.X)
			if err != nil || u.explicit || u.unknown || u.name != "" {
				return u, err
			}
			return collationUse{name: c}, nil
		}
	case *sqlir.CaseExpr:
		// The result takes the collation of the branches, not of the
		// conditions.
		branches := []sqlir.Expr{e.Else}
		for _, w := range e.Whens {
			branches = append(branches, w.Then)
		}
		return x.combineAll(branches)
	}
	// A result of a type that has no collation, such as length's integer,
	// ends what its arguments carry, whatever wraps it after.
	if typ := expressionType(e, nil); typ != "" && !collatableType(typ) && !x.tx.db.collatedDomains[typ] {
		return collationUse{}, nil
	}
	var uses []collationUse
	var err error
	v := reflect.ValueOf(e)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct {
		for _, f := range v.Fields() {
			if !f.CanInterface() {
				continue
			}
			switch c := f.Interface().(type) {
			case sqlir.Expr:
				var u collationUse
				if u, err = x.collationOf(c); err != nil {
					return collationUse{}, err
				}
				uses = append(uses, u)
			case []sqlir.Expr:
				for _, it := range c {
					var u collationUse
					if u, err = x.collationOf(it); err != nil {
						return collationUse{}, err
					}
					uses = append(uses, u)
				}
			}
		}
	}
	return x.combineCollations(uses)
}

// combineCollations is the collation of an expression over operands of
// uses. Postgres fails two explicit collations that differ, and two
// implicit ones other than the database's that differ, as of an
// indeterminate collation; detest refuses them.
func (x *sqlExec) combineCollations(uses []collationUse) (collationUse, error) {
	var out collationUse
	for _, u := range uses {
		switch {
		case u.explicit && out.explicit:
			if u.name != out.name {
				return collationUse{}, x.unsupported(fmt.Sprintf("text ordered by two collations, %q and %q", out.name, u.name))
			}
		case u.explicit:
			out = u
		case out.explicit:
		case u.unknown:
			out.unknown = true
		case u.name != "" && out.name != "" && u.name != out.name:
			return collationUse{}, x.unsupported(fmt.Sprintf("text ordered by two collations, %q and %q", out.name, u.name))
		case u.name != "":
			out.name = u.name
		}
	}
	if out.explicit {
		out.unknown = false
	}
	return out, nil
}

// orderCollation is the collation that orders the text exprs compare or
// sort, or nil when compareValues's byte order is it.
func (x *sqlExec) orderCollation(exprs ...sqlir.Expr) (sqlir.Collation, error) {
	db := x.tx.db
	if db.kind.InnoDB() {
		return nil, nil
	}
	if _, bytes := db.kind.TextCollation().(sqlir.ByteOrder); bytes && !x.collates && !db.declared() {
		return nil, nil
	}
	u, err := x.combineAll(exprs)
	if err != nil {
		return nil, err
	}
	if u.unknown && (db.declared() || x.collates) {
		// The item's query may take the collation of a column or of a
		// COLLATE of its own, which detest does not carry, and which matters
		// only when the values are text.
		return undecided{x.unsupported("text of a subquery, view or CTE ordered where a column or COLLATE sets a collation")}, nil
	}
	name := u.name
	if name == derivedCollation {
		return undecided{x.unsupported("text of a CREATE TABLE AS column whose collation detest could not tell")}, nil
	}
	if name == domainCollation {
		return nil, x.unsupported("text of a domain that declares a collation, which detest does not follow")
	}
	if name == "" {
		name = "default"
	}
	c, ok := db.kind.NamedCollation(name)
	if !ok {
		return nil, x.unsupported(fmt.Sprintf("collation %q, which postgres.Collations does not declare", name))
	}
	if _, bytes := c.(sqlir.ByteOrder); bytes {
		return nil, nil
	}
	return c, nil
}

func (x *sqlExec) combineAll(exprs []sqlir.Expr) (collationUse, error) {
	uses := make([]collationUse, 0, len(exprs))
	for _, e := range exprs {
		u, err := x.collationOf(e)
		if err != nil {
			return collationUse{}, err
		}
		uses = append(uses, u)
	}
	return x.combineCollations(uses)
}

// undecided is the collation of text whose collation detest cannot tell,
// which decide refuses once the values to order are text.
type undecided struct{ err error }

// Compare is never reached: decide resolves an undecided collation before
// anything is ordered by it.
func (undecided) Compare(a, b string) int {
	panic("detest: an undecided collation ordered text without decide")
}

// decide is c for ordering vals, failing an undecided collation when one of
// them is text and dropping it otherwise.
func decide(c sqlir.Collation, vals ...any) (sqlir.Collation, error) {
	u, ok := c.(undecided)
	if !ok {
		return c, nil
	}
	for _, v := range vals {
		if _, text := textValue(v); text {
			return nil, u.err
		}
	}
	return nil, nil
}

// hasCollate reports whether a COLLATE, or a cast to a type of a collation
// of its own such as name, stands anywhere under n, which may
// be a table definition with unexported fields: it reads types only, never
// the values behind them.
func hasCollate(n any) bool {
	collate, cast := reflect.TypeFor[*sqlir.Collate](), reflect.TypeFor[*sqlir.Cast]()
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
			if v.Type() == collate {
				found = true
				return
			}
			if v.Type() == cast && typeCollation(v.Elem().FieldByName("Type").String()) != "" {
				found = true
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

// isIndex reports whether an index of the qualified name exists, which
// shares the namespace of tables.
func (db *DB) isIndex(name string) bool {
	schema, rel, ok := strings.Cut(name, ".")
	if !ok {
		return false
	}
	for table, def := range db.defs {
		if !strings.HasPrefix(table, schema+".") {
			continue
		}
		for _, u := range def.uniques {
			if u.Name == rel {
				return true
			}
		}
		for _, ix := range def.indexes {
			if ix.Name == rel {
				return true
			}
		}
	}
	return false
}

// textlessOutputs reports whether every one of the n columns sel gives has
// a type the statement shows to be other than text, as in SELECT 1 UNION
// SELECT 2, which needs no collation kept.
func textlessOutputs(sel *sqlir.SelectStmt, n int) bool {
	for sel.SetOp != "" {
		sel = sel.Larg
	}
	var exprs []sqlir.Expr
	if len(sel.Values) > 0 {
		exprs = sel.Values[0]
	} else {
		for _, t := range sel.Targets {
			if t.Star {
				return false
			}
			exprs = append(exprs, t.Expr)
		}
	}
	if len(exprs) != n {
		return false
	}
	for _, e := range exprs {
		if typ := expressionType(e, nil); typ == "" || collatableType(typ) {
			return false
		}
	}
	return true
}

// declared is declaresCollations, kept until the schema changes, as the
// explorations run the same statements on one schema again and again.
func (db *DB) declared() bool {
	if db.declaredCache == nil {
		d := db.declaresCollations()
		db.declaredCache = &d
	}
	return *db.declaredCache
}

// declaresCollations reports whether a column or a domain of the database
// declares a collation other than the database's, or a COLLATE stands in a
// view's or a materialized view's query, or in an expression a table keeps
// and evaluates on a later write, a CHECK, a default, a generated column or
// an index's predicate, which the column check of that write does not see. "default" is the database's, and so
// are C and POSIX on a database of the C collation.
func (db *DB) declaresCollations() bool {
	_, bytes := db.kind.TextCollation().(sqlir.ByteOrder)
	for _, v := range db.views {
		if hasCollate(v.View) {
			return true
		}
	}
	if hasCollate(db.matviews) {
		return true
	}
	for _, def := range db.defs {
		if hasCollate(def) {
			return true
		}
	}
	other := func(name string) bool {
		if name == "" || name == "default" {
			return false
		}
		if c, ok := db.kind.NamedCollation(name); ok && bytes {
			if _, same := c.(sqlir.ByteOrder); same {
				return false
			}
		}
		return true
	}
	for _, def := range db.defs {
		if slices.ContainsFunc(slices.Collect(maps.Values(def.collations)), other) {
			return true
		}
	}
	// A domain's collation reaches text through a cast to the domain even
	// when no column is of it.
	return len(db.collatedDomains) > 0
}

// textValue is v as text. A []byte is not text here: it is how a bytea
// value is held, which Postgres orders byte by byte whatever the collation.
func textValue(v any) (string, bool) {
	t, ok := derefValue(v).(string)
	return t, ok
}

// compareOrdered is compareValues with text ordered by c, and two strings c
// does not tell apart ordered byte by byte, as Postgres orders them under a
// deterministic collation. A nil c is byte order.
func compareOrdered(c sqlir.Collation, a, b any) (int, bool) {
	if c != nil {
		sa, aok := textValue(a)
		sb, bok := textValue(b)
		if aok && bok {
			if r := c.Compare(sa, sb); r != 0 {
				return r, true
			}
			return strings.Compare(sa, sb), true
		}
	}
	return compareValues(a, b)
}

// callOrdered is callFunc, with greatest and least of text ordered by the
// collation of their arguments.
func (x *sqlExec) callOrdered(v *sqlir.FuncCall, args []any) (any, error) {
	if v.Name != "greatest" && v.Name != "least" {
		return x.callFunc(v.Name, args)
	}
	c, err := x.orderCollation(v.Args...)
	if err == nil {
		c, err = decide(c, args...)
	}
	if err != nil || c == nil {
		if err != nil {
			return nil, err
		}
		return x.callFunc(v.Name, args)
	}
	var best any
	for _, a := range args {
		if derefValue(a) == nil {
			continue
		}
		if best == nil {
			best = a
			continue
		}
		n, _ := compareOrdered(c, a, best)
		if (v.Name == "greatest" && n > 0) || (v.Name == "least" && n < 0) {
			best = a
		}
	}
	if best == nil || !isText(best) {
		return x.callFunc(v.Name, args)
	}
	return best, nil
}

// keyCollations are the collations that order the text of keys, whose
// values the rows of vals hold.
func (x *sqlExec) keyCollations(keys []sqlir.OrderKey, vals [][]any) ([]sqlir.Collation, error) {
	out := make([]sqlir.Collation, len(keys))
	for i, k := range keys {
		if _, row := k.Expr.(*sqlir.RowExpr); row {
			// Postgres orders a row field by field, each by its own
			// collation, where compareValues would compare the row's text.
			return nil, x.unsupported("a row as an ORDER BY key")
		}
		c, err := x.orderCollation(k.Expr)
		if err != nil {
			return nil, err
		}
		col := make([]any, len(vals))
		for r := range vals {
			col[r] = vals[r][i]
			if _, row := derefValue(col[r]).([]any); row {
				// A key that names or numbers a row of the select list.
				return nil, x.unsupported("a row as an ORDER BY key")
			}
		}
		if out[i], err = decide(c, col...); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// orderedOperands is v's comparison of the text l and r under the collation
// of its operands, and false when v is not one, which binary takes.
func (x *sqlExec) orderedOperands(v *sqlir.BinaryExpr, l, r any) (any, bool, error) {
	switch v.Op {
	case "<", "<=", ">", ">=":
	default:
		return nil, false, nil
	}
	c, err := x.orderCollation(v.L, v.R)
	if err == nil {
		c, err = decide(c, l, r)
	}
	if err != nil {
		return nil, false, err
	}
	out, ok := orderedBinary(v.Op, c, l, r)
	return out, ok, nil
}

// orderedBinary is <, <=, > or >= of l and r under c, and false when op is
// another operator or the operands are not both text, which binary takes.
func orderedBinary(op string, c sqlir.Collation, l, r any) (any, bool) {
	if c == nil {
		return nil, false
	}
	switch op {
	case "<", "<=", ">", ">=":
	default:
		return nil, false
	}
	if _, ok := textValue(l); !ok {
		return nil, false
	}
	if _, ok := textValue(r); !ok {
		return nil, false
	}
	n, _ := compareOrdered(c, l, r)
	switch op {
	case "<":
		return n < 0, true
	case "<=":
		return n <= 0, true
	case ">":
		return n > 0, true
	}
	return n >= 0, true
}
