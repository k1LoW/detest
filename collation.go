package detest

import (
	"cmp"
	"reflect"
	"slices"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// compareOps are the operators whose result depends on how two strings
// compare under their collation.
var compareOps = map[string]bool{
	"=": true, "<>": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true, "<=>": true,
	"LIKE": true, "NOT LIKE": true,
}

// orderingFuncs are the functions whose result depends on how strings order
// or equal each other.
var orderingFuncs = map[string]bool{
	"min": true, "max": true, "greatest": true, "least": true, "nullif": true,
	"mysql_greatest": true, "mysql_least": true, "mysql_nullif": true,
}

// collationCheck refuses a MySQL statement whose outcome depends on how a
// case-insensitive text column compares: a comparison, LIKE or IN on it, an
// ORDER BY, GROUP BY, DISTINCT or window over it, MIN and MAX of it, a
// write a unique index or a foreign key over it checks, and a LIMIT scan
// along a primary key that holds it. detest compares strings exactly, as a
// _bin collation does, and MySQL 8's default collation, utf8mb4_0900_ai_ci,
// does not; refusing the statements that depend on it keeps the schemas
// that declare it loading, while a value read or written as it is passes.
func (x *sqlExec) collationCheck(stmt sqlir.Statement) error {
	db := x.tx.db
	if !db.kind.InnoDB() {
		return nil
	}
	// The tables the statement names, by the name each item is known by.
	// One name may stand for other tables in other query blocks.
	// derived holds the names of items that are not tables, such as a
	// derived table or a WITH query, whose columns' collations detest does
	// not follow through the query they are made of.
	tables, derived := map[string][]string{}, map[string]bool{}
	// opaque holds the names of the items whose output columns are not
	// known by position, all of which are taken as case-insensitive.
	opaque := map[string]bool{}
	var view func(k, name string, v *sqlir.SchemaChange)
	add := func(name, alias string) {
		// An alias hides the table's name, which may be another item's
		// alias.
		k := cmp.Or(alias, relname(name))
		t := ""
		if name != "" {
			t = db.resolve(name)
		}
		if v := db.views[t]; v != nil && v.View != nil {
			view(k, t, v)
			return
		}
		if db.defs[t] == nil {
			if k != "" {
				derived[k] = true
			}
			return
		}
		if !slices.Contains(tables[k], t) {
			tables[k] = append(tables[k], t)
		}
	}
	// bodies are the queries each derived table or WITH query is made of,
	// by its name.
	bodies := map[string][]*sqlir.SelectStmt{}
	body := func(k string, q *sqlir.SelectStmt) {
		if !slices.Contains(bodies[k], q) {
			bodies[k] = append(bodies[k], q)
		}
	}
	// ctes are the queries of the WITH queries, by name.
	ctes := map[string][]*sqlir.SelectStmt{}
	with := func(list []sqlir.CTE) {
		for _, c := range list {
			// A WITH query hides a table of its name.
			derived[c.Name] = true
			if !slices.Contains(ctes[c.Name], c.Select) {
				ctes[c.Name] = append(ctes[c.Name], c.Select)
			}
			body(c.Name, c.Select)
		}
	}
	var collect func(root any)
	// The tables of a view's query join the statement's, as the queries of
	// derived tables and WITH queries do, so that its qualified references
	// resolve and a statement over views alone is checked at all. seen
	// walks each view's query once however often it is read.
	seen := map[string]bool{}
	view = func(k, name string, v *sqlir.SchemaChange) {
		derived[k] = true
		q := v.View
		// A * the binding left unexpanded gives columns at positions the
		// view's names do not tell.
		if slices.ContainsFunc(setArms(q), func(a *sqlir.SelectStmt) bool {
			return slices.ContainsFunc(a.Targets, func(t sqlir.Target) bool { return t.Star })
		}) {
			opaque[k] = true
		} else if len(v.ViewColumns) > 0 {
			q = viewNamed(q, v.ViewColumns)
		}
		body(k, q)
		if !seen[name] {
			seen[name] = true
			collect(v.View)
		}
	}
	collect = func(root any) {
		walkNodes(root, func(n any) {
			switch n := n.(type) {
			case *sqlir.SelectStmt:
				with(n.With)
			case *sqlir.UpdateStmt:
				with(n.With)
			case *sqlir.DeleteStmt:
				with(n.With)
			}
		})
		walkNodes(root, func(n any) {
			switch n := n.(type) {
			case *sqlir.TableRef:
				add(n.Name, n.Alias)
				switch {
				case n.Sub != nil && n.Alias != "":
					body(n.Alias, n.Sub)
				case n.Sub == nil && n.Alias != "" && ctes[n.Name] != nil:
					// The name may read a WITH query rather than the table it
					// hides; WITH scopes are not followed, so it is taken as
					// both.
					derived[n.Alias] = true
					for _, q := range ctes[n.Name] {
						body(n.Alias, q)
					}
				}
			case *sqlir.InsertStmt:
				add(n.Table, n.Alias)
			case *sqlir.UpdateStmt:
				add(n.Table, n.Alias)
			case *sqlir.DeleteStmt:
				add(n.Table, n.Alias)
			}
		})
	}
	collect(stmt)
	anyCI := false
	for _, ts := range tables {
		for _, t := range ts {
			if len(db.defs[t].ci) > 0 {
				anyCI = true
			}
		}
	}
	if !anyCI {
		return nil
	}
	// renamed holds, by the name of a derived table or a WITH query, the
	// lower cased output names it gives a case-insensitive column under
	// another name, such as v of SELECT s AS v, which no table has.
	renamed := map[string]map[string]bool{}
	// A qualifier is not resolved to its query block, so a name that
	// stands for several tables refuses a column case-insensitive in any,
	// and one that may stand for an item other than a table refuses a
	// column case-insensitive in any table, as an unqualified one does.
	isCI := func(c *sqlir.ColumnRef) bool {
		whole := c.Table == "" || derived[c.Table] || len(tables[c.Table]) == 0
		col := strings.ToLower(c.Column)
		for k := range opaque {
			if whole || k == c.Table {
				return true
			}
		}
		for k, names := range renamed {
			if (whole || k == c.Table) && names[col] {
				return true
			}
		}
		for k, ts := range tables {
			if !whole && k != c.Table {
				continue
			}
			for _, t := range ts {
				if db.defs[t].ci[c.Column] {
					return true
				}
			}
		}
		return false
	}
	// A * in a subquery, such as (SELECT * FROM t), gives a column no
	// ColumnRef names, so it is taken as case-insensitive, as a * is in
	// starCI below.
	exprCI := func(e sqlir.Expr) bool {
		return slices.ContainsFunc(sqlir.ColumnRefs(e), isCI) || slices.ContainsFunc(sqlir.Selects(e), func(q *sqlir.SelectStmt) bool {
			return slices.ContainsFunc(q.Targets, func(t sqlir.Target) bool { return t.Star })
		})
	}
	starCI := func(targets []sqlir.Target) bool {
		for _, t := range targets {
			if t.Star || exprCI(t.Expr) {
				return true
			}
		}
		return false
	}
	// A rename may pass through another derived table or WITH query, so
	// the names are gathered until none is added.
	for changed := true; changed; {
		changed = false
		for k, qs := range bodies {
			for _, q := range qs {
				names, known := ciOutputs(q, exprCI)
				if !known && !opaque[k] {
					opaque[k] = true
					changed = true
				}
				for _, name := range names {
					if renamed[k] == nil {
						renamed[k] = map[string]bool{}
					}
					if !renamed[k][name] {
						renamed[k][name] = true
						changed = true
					}
				}
			}
		}
	}
	refuse := func(what string) error {
		return x.unsupported(what + " a case-insensitive text column (declare it with a _bin collation, such as COLLATE utf8mb4_bin)")
	}
	keysCI := func(keys []sqlir.OrderKey, targets []sqlir.Target) bool {
		for _, k := range keys {
			if exprCI(k.Expr) {
				return true
			}
			// ORDER BY may name a select alias.
			if c, ok := k.Expr.(*sqlir.ColumnRef); ok && c.Table == "" {
				for _, t := range targets {
					if strings.EqualFold(t.Alias, c.Column) && exprCI(t.Expr) {
						return true
					}
				}
			}
		}
		return false
	}
	var err error
	walkNodes(stmt, func(n any) {
		if err != nil {
			return
		}
		switch n := n.(type) {
		case *sqlir.BinaryExpr:
			if compareOps[n.Op] && (exprCI(n.L) || exprCI(n.R)) {
				err = refuse("a comparison of")
			}
		case *sqlir.InExpr:
			if exprCI(n.X) || slices.ContainsFunc(n.List, exprCI) || n.Sub != nil && starCI(n.Sub.Targets) {
				err = refuse("IN over")
			}
		case *sqlir.CaseExpr:
			if n.Arg != nil && (exprCI(n.Arg) || slices.ContainsFunc(n.Whens, func(w sqlir.CaseWhen) bool { return exprCI(w.When) })) {
				err = refuse("CASE comparing")
			}
		case *sqlir.FuncCall:
			if (orderingFuncs[n.Name] || n.Distinct) && slices.ContainsFunc(n.Args, exprCI) {
				err = refuse(strings.ToUpper(n.Name) + " of")
			}
		case *sqlir.WindowFunc:
			if slices.ContainsFunc(n.Partition, exprCI) || keysCI(n.Order, nil) {
				err = refuse("a window over")
			}
		case *sqlir.SelectStmt:
			switch {
			case slices.ContainsFunc(n.GroupBy, exprCI):
				err = refuse("GROUP BY")
			case keysCI(n.OrderBy, n.Targets):
				err = refuse("ORDER BY")
			case slices.ContainsFunc(n.DistinctOn, exprCI) || n.Distinct && starCI(n.Targets):
				err = refuse("DISTINCT over")
			case n.SetOp != "" && !n.SetAll && n.Larg != nil && n.Rarg != nil && (starCI(n.Larg.Targets) || starCI(n.Rarg.Targets)):
				err = refuse(strings.ToUpper(n.SetOp) + " over")
			case n.Lock != nil && n.Limit != nil && len(n.OrderBy) == 0 && n.From != nil && slices.ContainsFunc(tables[cmp.Or(n.From.Alias, relname(n.From.Name))], func(t string) bool { return pkCI(db, t) }):
				err = refuse("a locking LIMIT scan along a primary key that holds")
			}
		case *sqlir.UpdateStmt:
			table := db.resolve(n.Table)
			switch {
			case keysCI(n.OrderBy, nil):
				err = refuse("ORDER BY")
			case n.Limit != nil && len(n.OrderBy) == 0 && pkCI(db, table):
				err = refuse("a LIMIT scan along a primary key that holds")
			case slices.ContainsFunc(n.Set, func(a sqlir.Assignment) bool { return uniqueCI(db, table, a.Column) || fkCI(db, table, a.Column) }):
				err = refuse("a write a unique index or a foreign key checks, over")
			}
		case *sqlir.DeleteStmt:
			table := db.resolve(n.Table)
			switch {
			case keysCI(n.OrderBy, nil):
				err = refuse("ORDER BY")
			case n.Limit != nil && len(n.OrderBy) == 0 && pkCI(db, table):
				err = refuse("a LIMIT scan along a primary key that holds")
			case referencedCI(db, table):
				err = refuse("a delete a foreign key checks, over")
			}
		case *sqlir.InsertStmt:
			table := db.resolve(n.Table)
			if uniqueCI(db, table, "") || fkCI(db, table, "") {
				err = refuse("an insert a unique index or a foreign key checks, over")
			}
		}
	})
	return err
}

// ciOutputs returns the lower cased names q gives its output columns that
// ciExpr reports case-insensitive, of the columns an alias or a plain column
// names. A set operation's column takes its name from its left query, and
// is taken as case-insensitive when any query's column at its position is.
// That also refuses a _bin column beside a _ci one, which MySQL compares
// by _bin, as weighing collations against each other is not worth it for a
// form so rare. known is false when a query of a set operation has a *,
// whose columns stand at positions the targets do not tell, and when a
// VALUES list holds a case-insensitive value, which is not followed by
// position for a form so rare.
func ciOutputs(q *sqlir.SelectStmt, ciExpr func(sqlir.Expr) bool) (names []string, known bool) {
	var arms []*sqlir.SelectStmt
	var leaves func(q *sqlir.SelectStmt)
	leaves = func(q *sqlir.SelectStmt) {
		if q.SetOp != "" && q.Larg != nil && q.Rarg != nil {
			leaves(q.Larg)
			leaves(q.Rarg)
			return
		}
		arms = append(arms, q)
	}
	leaves(q)
	if len(arms) > 1 && slices.ContainsFunc(arms, func(a *sqlir.SelectStmt) bool {
		return slices.ContainsFunc(a.Targets, func(t sqlir.Target) bool { return t.Star })
	}) {
		return nil, false
	}
	if slices.ContainsFunc(arms, func(a *sqlir.SelectStmt) bool {
		return slices.ContainsFunc(a.Values, func(row []sqlir.Expr) bool { return slices.ContainsFunc(row, ciExpr) })
	}) {
		return nil, false
	}
	for i, t := range arms[0].Targets {
		name := t.Alias
		if c, ok := t.Expr.(*sqlir.ColumnRef); ok && name == "" {
			name = c.Column
		}
		if name == "" {
			continue
		}
		if slices.ContainsFunc(arms, func(a *sqlir.SelectStmt) bool {
			return i < len(a.Targets) && ciExpr(a.Targets[i].Expr)
		}) {
			names = append(names, strings.ToLower(name))
		}
	}
	return names, true
}

// setArms returns the queries a set operation q is made of, left to right,
// or q itself when it is not one.
func setArms(q *sqlir.SelectStmt) []*sqlir.SelectStmt {
	if q.SetOp != "" && q.Larg != nil && q.Rarg != nil {
		return append(setArms(q.Larg), setArms(q.Rarg)...)
	}
	return []*sqlir.SelectStmt{q}
}

// viewNamed returns a copy of a view's query whose first query names its
// output columns by the view's column list, which is where ciOutputs reads
// a set operation's names from.
func viewNamed(q *sqlir.SelectStmt, cols []string) *sqlir.SelectStmt {
	sub, _ := sqlir.CloneExpr(&sqlir.SubQuery{Select: q}).(*sqlir.SubQuery)
	first := setArms(sub.Select)[0]
	for i := range first.Targets {
		if i < len(cols) {
			first.Targets[i].Alias = cols[i]
		}
	}
	return sub.Select
}

// pkCI reports whether table's primary key holds a case-insensitive column.
func pkCI(db *DB, table string) bool {
	def := db.defs[table]
	return def != nil && slices.ContainsFunc(def.pk, func(c string) bool { return def.ci[c] })
}

// uniqueCI reports whether a unique index of table, the primary key
// included, holds a case-insensitive column, and with col, whether it holds
// col too, so that writing col checks it.
func uniqueCI(db *DB, table, col string) bool {
	def := db.defs[table]
	if def == nil || len(def.ci) == 0 {
		return false
	}
	check := func(cols []string) bool {
		return slices.ContainsFunc(cols, func(c string) bool { return def.ci[c] }) && (col == "" || slices.Contains(cols, col))
	}
	if check(def.pk) {
		return true
	}
	for _, u := range def.uniques {
		var cols []string
		for _, e := range u.Elems {
			for _, c := range sqlir.ColumnRefs(e) {
				cols = append(cols, c.Column)
			}
		}
		if check(cols) {
			return true
		}
	}
	return false
}

// fkCI reports whether a foreign key of table, on col when given, matches a
// case-insensitive column, its own or its parent's.
func fkCI(db *DB, table, col string) bool {
	def := db.defs[table]
	if def == nil {
		return false
	}
	for _, fk := range def.fks {
		if col != "" && !slices.Contains(fk.Columns, col) {
			continue
		}
		parent := db.defs[fk.RefTable]
		if slices.ContainsFunc(fk.Columns, func(c string) bool { return def.ci[c] }) ||
			parent != nil && slices.ContainsFunc(fk.RefColumns, func(c string) bool { return parent.ci[c] }) {
			return true
		}
	}
	return false
}

// referencedCI reports whether a foreign key referencing table matches a
// case-insensitive column, which a delete of a parent row looks children up
// by.
func referencedCI(db *DB, table string) bool {
	for child, def := range db.defs {
		for _, fk := range def.fks {
			if fk.RefTable == table && fkCI(db, child, "") {
				return true
			}
		}
	}
	return false
}

// walkNodes calls f with every pointer node under root, and with a pointer
// to every TableRef held by value, as a Join holds its table.
func walkNodes(root any, f func(any)) {
	tableRef := reflect.TypeFor[sqlir.TableRef]()
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Pointer:
			if v.IsNil() {
				return
			}
			f(v.Interface())
			walk(v.Elem())
		case reflect.Struct:
			if v.Type() == tableRef {
				if v.CanAddr() {
					f(v.Addr().Interface())
				} else if c, ok := reflect.TypeAssert[sqlir.TableRef](v); ok {
					f(&c)
				}
			}
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i))
				}
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(root))
}
