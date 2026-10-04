package detest

import (
	"fmt"
	"reflect"
	"slices"

	"github.com/k1LoW/detest/internal/sqlir"
)

// colSet is the columns of a FROM item. A nil set is one whose columns
// detest does not know, such as a table no schema declared or a function,
// and resolves any name, so that a statement is never refused for a column
// it may have.
type colSet map[string]bool

func setOf(cols []string) colSet {
	s := colSet{}
	for _, c := range cols {
		s[c] = true
	}
	return s
}

// colScope is the FROM items one query block sees, inside those of the
// blocks around it.
type colScope struct {
	items map[string]colSet
	// outputs are the names the select list gives, which ORDER BY, GROUP BY
	// and DISTINCT ON may refer to.
	outputs  colSet
	excluded colSet
	outer    *colScope
}

// columnChecker resolves the column references of a statement against the
// schema before it runs, as Postgres does when it plans one: a name no FROM
// item in scope has fails with 42703 whatever the rows are. Without the
// check, detest would read the name as NULL and write it as a column of its
// own.
type columnChecker struct {
	x    *sqlExec
	ctes []map[string]colSet
}

func (x *sqlExec) checkColumns(stmt sqlir.Statement) error {
	if x.tx.db.kind.InnoDB() || x.tx.db.defs == nil {
		return nil
	}
	c := &columnChecker{x: x}
	switch st := stmt.(type) {
	case *sqlir.SelectStmt:
		_, err := c.query(st, nil)
		return err
	case *sqlir.InsertStmt:
		return c.insert(st)
	case *sqlir.UpdateStmt:
		return c.update(st)
	case *sqlir.DeleteStmt:
		return c.delete(st)
	}
	return nil
}

func (c *columnChecker) undefined(col string) error {
	return c.x.tx.db.kind.Error(sqlir.UndefinedColumn, fmt.Sprintf("column %q does not exist", col), "", col, "")
}

func (c *columnChecker) undefinedIn(table, col string) error {
	return c.x.tx.db.kind.Error(sqlir.UndefinedColumn, fmt.Sprintf("column %q of relation %q does not exist", col, relname(table)), relname(table), col, "")
}

// tableColumns is the column set of the table, view or CTE name refers to.
func (c *columnChecker) tableColumns(name string) colSet {
	for _, m := range slices.Backward(c.ctes) {
		if s, ok := m[name]; ok {
			return s
		}
	}
	db := c.x.tx.db
	table := db.resolve(name)
	if def := db.defs[table]; def != nil {
		return setOf(def.columns)
	}
	if v := db.views[table]; v != nil {
		if len(v.ViewColumns) > 0 {
			return setOf(v.ViewColumns)
		}
		return outputColumns(v.View)
	}
	return nil
}

// outputColumns is the names a query's rows have, nil when a select list
// item's name depends on more than detest looks at, such as a *.
func outputColumns(sel *sqlir.SelectStmt) colSet {
	for sel.SetOp != "" && sel.Larg != nil {
		sel = sel.Larg
	}
	if len(sel.Values) > 0 {
		s := colSet{}
		for i := range sel.Values[0] {
			s[fmt.Sprintf("column%d", i+1)] = true
		}
		return s
	}
	s := colSet{}
	for _, t := range sel.Targets {
		if t.Star {
			return nil
		}
		s[targetName(t)] = true
	}
	return s
}

// item adds a FROM item to scope, checking the query it is made of.
func (c *columnChecker) item(t sqlir.TableRef, sc *colScope) error {
	alias := t.Alias
	var cols colSet
	switch {
	case t.Sub != nil:
		outer := sc.outer
		if t.Lateral {
			outer = sc
		}
		s, err := c.query(t.Sub, outer)
		if err != nil {
			return err
		}
		cols = s
	case t.Func != nil:
		if err := c.exprs(t.Func, sc); err != nil {
			return err
		}
		if alias == "" {
			alias = t.Func.Name
		}
		cols = nil
		if len(t.Columns) > 0 {
			cols = setOf(t.Columns)
			if t.Ordinality {
				cols = nil // the ordinality column's name is the alias's next
			}
		}
	default:
		if alias == "" {
			alias = relname(t.Name)
		}
		cols = c.tableColumns(t.Name)
	}
	if len(t.Columns) > 0 && t.Func == nil && cols != nil {
		// AS alias(a, b) renames the first columns; which the others are
		// detest does not track.
		cols = nil
	}
	sc.items[alias] = cols
	return nil
}

// query checks a query block and returns its output columns.
func (c *columnChecker) query(sel *sqlir.SelectStmt, outer *colScope) (colSet, error) {
	if sel == nil {
		return nil, nil
	}
	if len(sel.With) > 0 {
		m := map[string]colSet{}
		c.ctes = append(c.ctes, m)
		defer func() { c.ctes = c.ctes[:len(c.ctes)-1] }()
		for _, cte := range sel.With {
			s, err := c.query(cte.Select, outer)
			if err != nil {
				return nil, err
			}
			m[cte.Name] = s
		}
	}
	if sel.SetOp != "" {
		out, err := c.query(sel.Larg, outer)
		if err != nil {
			return nil, err
		}
		if _, err := c.query(sel.Rarg, outer); err != nil {
			return nil, err
		}
		sc := &colScope{items: map[string]colSet{}, outputs: out, outer: outer}
		for _, o := range sel.OrderBy {
			if err := c.orderExpr(o.Expr, sc); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	sc := &colScope{items: map[string]colSet{}, outer: outer}
	for _, row := range sel.Values {
		if err := c.exprs(row, sc); err != nil {
			return nil, err
		}
	}
	if sel.From != nil {
		if err := c.item(*sel.From, sc); err != nil {
			return nil, err
		}
	}
	// A join's ON sees the items before it and its own, not those joined
	// after it.
	for _, j := range sel.Joins {
		if err := c.item(j.Table, sc); err != nil {
			return nil, err
		}
		if err := c.exprs(j.On, sc); err != nil {
			return nil, err
		}
	}
	for _, t := range sel.Targets {
		if t.Star {
			if t.Table != "" {
				if _, ok := sc.lookupItem(t.Table); !ok {
					return nil, c.x.tx.db.kind.Error(sqlir.UndefinedTable, fmt.Sprintf("missing FROM-clause entry for table %q", t.Table), t.Table, "", "")
				}
			}
			continue
		}
		if err := c.exprs(t.Expr, sc); err != nil {
			return nil, err
		}
	}
	if err := c.exprs(sel.Where, sc); err != nil {
		return nil, err
	}
	if err := c.exprs(sel.Having, sc); err != nil {
		return nil, err
	}
	sc.outputs = outputColumns(sel)
	for _, e := range sel.GroupBy {
		if err := c.orderExpr(e, sc); err != nil {
			return nil, err
		}
	}
	for _, e := range sel.DistinctOn {
		if err := c.orderExpr(e, sc); err != nil {
			return nil, err
		}
	}
	for _, o := range sel.OrderBy {
		if err := c.orderExpr(o.Expr, sc); err != nil {
			return nil, err
		}
	}
	if err := c.exprs(sel.Limit, sc); err != nil {
		return nil, err
	}
	if err := c.exprs(sel.Offset, sc); err != nil {
		return nil, err
	}
	return outputColumns(sel), nil
}

// orderExpr checks an ORDER BY, GROUP BY or DISTINCT ON item, which may name
// an output column of the select list.
func (c *columnChecker) orderExpr(e sqlir.Expr, sc *colScope) error {
	if r, ok := e.(*sqlir.ColumnRef); ok && r.Table == "" && (sc.outputs == nil || sc.outputs[r.Column]) {
		return nil
	}
	return c.exprs(e, sc)
}

func (sc *colScope) lookupItem(alias string) (colSet, bool) {
	for s := sc; s != nil; s = s.outer {
		if alias == "excluded" && s.excluded != nil {
			return s.excluded, true
		}
		if cols, ok := s.items[alias]; ok {
			return cols, true
		}
	}
	return nil, false
}

// resolve reports a column reference no FROM item in scope has.
func (c *columnChecker) resolve(r *sqlir.ColumnRef, sc *colScope) error {
	if r.Table != "" {
		cols, ok := sc.lookupItem(r.Table)
		if !ok || cols == nil || cols[r.Column] {
			// A qualifier detest does not tell apart, such as a schema, is
			// let through.
			return nil
		}
		return c.undefined(r.Table + "." + r.Column)
	}
	for s := sc; s != nil; s = s.outer {
		for alias, cols := range s.items {
			// A table's name is a whole-row value of it.
			if cols == nil || cols[r.Column] || alias == r.Column {
				return nil
			}
		}
	}
	return c.undefined(r.Column)
}

// exprs checks the column references under n, and the subqueries in it
// with their own scopes.
func (c *columnChecker) exprs(n any, sc *colScope) error {
	var err error
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		if err != nil {
			return
		}
		switch v.Kind() {
		case reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Pointer:
			if v.IsNil() {
				return
			}
			switch e := v.Interface().(type) {
			case *sqlir.ColumnRef:
				err = c.resolve(e, sc)
				return
			case *sqlir.SelectStmt:
				_, err = c.query(e, sc)
				return
			}
			walk(v.Elem())
		case reflect.Struct:
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
	walk(reflect.ValueOf(n))
	return err
}

// target is the scope of the table a write targets.
func (c *columnChecker) target(table, alias string) (*colScope, colSet) {
	if alias == "" {
		alias = relname(table)
	}
	cols := c.tableColumns(table)
	return &colScope{items: map[string]colSet{alias: cols}}, cols
}

// assigned checks the columns a write assigns, which must be the table's,
// each once.
func (c *columnChecker) assigned(table string, cols colSet, names []string) error {
	seen := map[string]bool{}
	for _, n := range names {
		if cols != nil && !cols[n] {
			return c.undefinedIn(table, n)
		}
		if seen[n] {
			return c.x.tx.db.kind.Error(sqlir.SyntaxError, fmt.Sprintf("multiple assignments to same column %q", n), relname(table), n, "")
		}
		seen[n] = true
	}
	return nil
}

func (c *columnChecker) returning(ts []sqlir.Target, sc *colScope) error {
	for _, t := range ts {
		if !t.Star {
			if err := c.exprs(t.Expr, sc); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *columnChecker) insert(ins *sqlir.InsertStmt) error {
	sc, cols := c.target(ins.Table, ins.Alias)
	if err := c.assigned(ins.Table, cols, ins.Columns); err != nil {
		return err
	}
	values := &colScope{items: map[string]colSet{}}
	for _, row := range ins.Rows {
		if err := c.exprs(row, values); err != nil {
			return err
		}
	}
	if ins.Select != nil {
		if _, err := c.query(ins.Select, nil); err != nil {
			return err
		}
	}
	if oc := ins.OnConflict; oc != nil {
		for _, col := range oc.Columns {
			if cols != nil && !cols[col] {
				return c.undefined(col)
			}
		}
		if err := c.exprs(oc.Elems, sc); err != nil {
			return err
		}
		if err := c.exprs(oc.InferWhere, sc); err != nil {
			return err
		}
		if err := c.assigned(ins.Table, cols, assignedColumns(oc.Set)); err != nil {
			return err
		}
		up := &colScope{items: sc.items, excluded: cols}
		if err := c.exprs(assignedValues(oc.Set), up); err != nil {
			return err
		}
		if err := c.exprs(oc.Where, up); err != nil {
			return err
		}
	}
	return c.returning(ins.Returning, sc)
}

func (c *columnChecker) update(up *sqlir.UpdateStmt) error {
	sc, cols := c.target(up.Table, up.Alias)
	if err := c.assigned(up.Table, cols, assignedColumns(up.Set)); err != nil {
		return err
	}
	for _, t := range up.From {
		if err := c.item(t, sc); err != nil {
			return err
		}
	}
	if err := c.exprs(assignedValues(up.Set), sc); err != nil {
		return err
	}
	if err := c.exprs(up.Where, sc); err != nil {
		return err
	}
	return c.returning(up.Returning, sc)
}

func (c *columnChecker) delete(del *sqlir.DeleteStmt) error {
	sc, _ := c.target(del.Table, del.Alias)
	for _, t := range del.Using {
		if err := c.item(t, sc); err != nil {
			return err
		}
	}
	if err := c.exprs(del.Where, sc); err != nil {
		return err
	}
	return c.returning(del.Returning, sc)
}
