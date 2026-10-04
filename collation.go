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
	// The tables the statement names, by alias and by name.
	tables := map[string]string{}
	add := func(name, alias string) {
		if name == "" {
			return
		}
		t := db.resolve(name)
		if db.defs[t] == nil {
			return
		}
		tables[strings.ToLower(relname(name))] = t
		if alias != "" {
			tables[strings.ToLower(alias)] = t
		}
	}
	walkNodes(stmt, func(n any) {
		switch n := n.(type) {
		case *sqlir.TableRef:
			add(n.Name, n.Alias)
		case *sqlir.InsertStmt:
			add(n.Table, n.Alias)
		case *sqlir.UpdateStmt:
			add(n.Table, n.Alias)
		case *sqlir.DeleteStmt:
			add(n.Table, n.Alias)
		}
	})
	anyCI := false
	for _, t := range tables {
		if len(db.defs[t].ci) > 0 {
			anyCI = true
		}
	}
	if !anyCI {
		return nil
	}
	isCI := func(c *sqlir.ColumnRef) bool {
		if c.Table != "" {
			t, ok := tables[strings.ToLower(c.Table)]
			return ok && db.defs[t].ci[c.Column]
		}
		for _, t := range tables {
			if db.defs[t].ci[c.Column] {
				return true
			}
		}
		return false
	}
	exprCI := func(e sqlir.Expr) bool { return slices.ContainsFunc(sqlir.ColumnRefs(e), isCI) }
	starCI := func(targets []sqlir.Target) bool {
		for _, t := range targets {
			if t.Star || exprCI(t.Expr) {
				return true
			}
		}
		return false
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
			case n.Lock != nil && n.Limit != nil && len(n.OrderBy) == 0 && n.From != nil && pkCI(db, tables[strings.ToLower(cmp.Or(n.From.Alias, relname(n.From.Name)))]):
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
