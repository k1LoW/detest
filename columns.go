package detest

import (
	"fmt"
	"reflect"
	"slices"

	"github.com/k1LoW/detest/internal/sqlir"
)

// colSet is the columns of a FROM item, each true unless the item has two
// columns of the name, as a query's output may. A nil set is one whose
// columns detest does not know, such as a table no schema declared or a
// function, and resolves any name, so that a statement is never refused for
// a column it may have.
type colSet map[string]bool

// has reports whether the set has a column of the name, and whether it has
// only one.
func (s colSet) has(name string) (found, once bool) {
	once, found = s[name]
	return found, once
}

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
	outputs colSet
	// anyOutput is a set operation's output whose names detest does not
	// know, as its first query has a *, which any name may refer to.
	anyOutput bool
	// hidden is the target of an UPDATE or a DELETE, which its FROM or
	// USING items may not refer to.
	hidden string
	// excluded are the columns of ON CONFLICT DO UPDATE's proposed row,
	// with hasExcluded telling a table of unknown columns from none.
	excluded    colSet
	hasExcluded bool
	outer       *colScope
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
	if x.tx.db.kind.InnoDB() {
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

// twice refuses a name a derived table or CTE gives two of its columns,
// which Postgres fails with 42702 and detest would read as one of them.
func (c *columnChecker) twice(col string) error {
	return c.x.unsupported(fmt.Sprintf("column %q, which the query it comes from gives twice", col))
}

func (c *columnChecker) missingItem(alias string) error {
	return c.x.tx.db.kind.Error(sqlir.UndefinedTable, fmt.Sprintf("missing FROM-clause entry for table %q", alias), alias, "", "")
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
	return namesSet(outputNames(sel))
}

// outputNames are the names of a query's columns in order, nil as for
// outputColumns.
func outputNames(sel *sqlir.SelectStmt) []string {
	for sel.SetOp != "" && sel.Larg != nil {
		sel = sel.Larg
	}
	var out []string
	if len(sel.Values) > 0 {
		for i := range sel.Values[0] {
			out = append(out, fmt.Sprintf("column%d", i+1))
		}
		return out
	}
	for _, t := range sel.Targets {
		if t.Star {
			return nil
		}
		out = append(out, targetName(t))
	}
	return out
}

// namesSet is the set of names, a name given twice marked as such; nil
// for nil.
func namesSet(names []string) colSet {
	if names == nil {
		return nil
	}
	s := colSet{}
	for _, n := range names {
		_, seen := s[n]
		s[n] = !seen
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
	if len(t.Columns) > 0 && t.Func == nil {
		// AS alias(a, b) renames the first columns, which needs them in
		// order.
		names := outputNames(t.Sub)
		cols = nil
		if names != nil {
			cols = namesSet(renamedCols(names, t.Columns))
		}
	}
	if _, dup := sc.items[alias]; dup {
		// Postgres fails with 42712; detest would keep one of the two.
		return c.x.unsupported(fmt.Sprintf("FROM items with the same name %q", alias))
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
		sc := &colScope{items: map[string]colSet{}, outputs: out, anyOutput: out == nil, outer: outer}
		first := sel
		for first.SetOp != "" && first.Larg != nil {
			first = first.Larg
		}
		for _, o := range sel.OrderBy {
			if err := c.position(first, o.Expr, "ORDER BY"); err != nil {
				return nil, err
			}
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
					return nil, c.missingItem(t.Table)
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
	// The names the select list gives itself; those a * gives are the
	// input's, which a reference resolves against anyway.
	sc.outputs = colSet{}
	for _, t := range sel.Targets {
		if !t.Star {
			n := targetName(t)
			_, seen := sc.outputs[n]
			sc.outputs[n] = !seen
		}
	}
	for _, e := range sel.GroupBy {
		if err := c.position(sel, e, "GROUP BY"); err != nil {
			return nil, err
		}
		if err := c.groupExpr(e, sc); err != nil {
			return nil, err
		}
	}
	for _, e := range sel.DistinctOn {
		if err := c.orderExpr(e, sc); err != nil {
			return nil, err
		}
	}
	for _, o := range sel.OrderBy {
		if err := c.position(sel, o.Expr, "ORDER BY"); err != nil {
			return nil, err
		}
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

// position fails an integer GROUP BY or ORDER BY item that is no position
// of the select list. A * makes the list longer by columns detest does not
// count here, so a query with one is let through.
func (c *columnChecker) position(sel *sqlir.SelectStmt, e sqlir.Expr, clause string) error {
	k, ok := e.(*sqlir.Const)
	if !ok {
		return nil
	}
	n, ok := k.Value.(int64)
	if !ok {
		return nil
	}
	if slices.ContainsFunc(sel.Targets, func(t sqlir.Target) bool { return t.Star }) {
		if clause == "GROUP BY" {
			// The position may point into the columns of the *, which
			// grouping does not expand.
			return c.x.unsupported("a GROUP BY position in a select list with *")
		}
		return nil
	}
	if n < 1 || n > int64(len(sel.Targets)) {
		return c.x.tx.db.kind.Error(sqlir.InvalidColumnReference, fmt.Sprintf("%s position %d is not in select list", clause, n), "", "", "")
	}
	return nil
}

// orderExpr checks an ORDER BY, GROUP BY or DISTINCT ON item, which may name
// an output column of the select list.
func (c *columnChecker) orderExpr(e sqlir.Expr, sc *colScope) error {
	r, ok := e.(*sqlir.ColumnRef)
	if !ok || r.Table != "" {
		return c.exprs(e, sc)
	}
	if sc.anyOutput {
		return nil
	}
	switch found, once := sc.outputs.has(r.Column); {
	case found && !once:
		// Postgres fails such a name with 42702 unless the columns are
		// the same expression, a rule detest does not follow.
		return c.x.unsupported(fmt.Sprintf("output name %q given twice and referred to", r.Column))
	case found:
		return nil
	}
	return c.exprs(e, sc)
}

// groupExpr checks a GROUP BY item. Unlike ORDER BY, a name refers to a
// column of the input first, and to an output column only when the input
// has none of the name.
func (c *columnChecker) groupExpr(e sqlir.Expr, sc *colScope) error {
	r, ok := e.(*sqlir.ColumnRef)
	if !ok || r.Table != "" {
		return c.exprs(e, sc)
	}
	for _, cols := range sc.items {
		if found, _ := cols.has(r.Column); cols == nil || found {
			return c.resolve(r, sc)
		}
	}
	return c.orderExpr(e, sc)
}

func (sc *colScope) lookupItem(alias string) (colSet, bool) {
	for s := sc; s != nil; s = s.outer {
		if alias == "excluded" && s.hasExcluded {
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
		if !ok {
			for s := sc; s != nil; s = s.outer {
				if s.hidden == r.Table {
					return c.x.tx.db.kind.Error(sqlir.InvalidColumnReference, fmt.Sprintf("invalid reference to FROM-clause entry for table %q", r.Table), r.Table, "", "")
				}
			}
			return c.missingItem(r.Table)
		}
		if cols == nil {
			return nil
		}
		switch found, once := cols.has(r.Column); {
		case found && !once:
			return c.twice(r.Table + "." + r.Column)
		case found:
			return nil
		}
		return c.undefined(r.Table + "." + r.Column)
	}
	// The innermost scope with the name decides it. An item whose columns
	// are unknown may have it, so it decides nothing beyond what the known
	// items already settle: two of them having the name is ambiguous
	// whatever it has.
	for s := sc; s != nil; s = s.outer {
		found, twice, wholeRow, unknown := 0, false, false, false
		for alias, cols := range s.items {
			if cols == nil {
				unknown = true
				continue
			}
			if has, once := cols.has(r.Column); has {
				found++
				twice = twice || !once
			}
			wholeRow = wholeRow || alias == r.Column
		}
		switch {
		case found > 1:
			return c.x.tx.db.kind.Error(sqlir.AmbiguousColumn, fmt.Sprintf("column reference %q is ambiguous", r.Column), "", r.Column, "")
		case unknown:
			return nil
		case found == 1 && twice:
			return c.twice(r.Column)
		case found == 1:
			return nil
		case wholeRow:
			// A table's name is a whole-row value of it, which detest
			// would read as NULL.
			return c.x.unsupported(fmt.Sprintf("the whole-row value %q of a table", r.Column))
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
	cols := c.tableColumns(table)
	return &colScope{items: map[string]colSet{targetAlias(table, alias): cols}}, cols
}

func targetAlias(table, alias string) string {
	if alias == "" {
		return relname(table)
	}
	return alias
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

// returning checks RETURNING, whose * detest gives the target's columns
// for, so a * of another item, which UPDATE ... FROM and DELETE ... USING
// may name, is refused.
func (c *columnChecker) returning(ts []sqlir.Target, sc *colScope, target string) error {
	for _, t := range ts {
		if !t.Star {
			if err := c.exprs(t.Expr, sc); err != nil {
				return err
			}
			continue
		}
		if t.Table == "" || t.Table == target {
			continue
		}
		if _, ok := sc.lookupItem(t.Table); !ok {
			return c.missingItem(t.Table)
		}
		return c.x.unsupported(fmt.Sprintf("RETURNING %s.*, which is not the target's", t.Table))
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
		up := &colScope{items: sc.items, excluded: cols, hasExcluded: true}
		if err := c.exprs(assignedValues(oc.Set), up); err != nil {
			return err
		}
		if err := c.exprs(oc.Where, up); err != nil {
			return err
		}
	}
	return c.returning(ins.Returning, sc, targetAlias(ins.Table, ins.Alias))
}

func (c *columnChecker) update(up *sqlir.UpdateStmt) error {
	sc, cols := c.target(up.Table, up.Alias)
	if err := c.assigned(up.Table, cols, assignedColumns(up.Set)); err != nil {
		return err
	}
	if err := c.extraItems(up.From, sc, targetAlias(up.Table, up.Alias)); err != nil {
		return err
	}
	if err := c.exprs(assignedValues(up.Set), sc); err != nil {
		return err
	}
	if err := c.exprs(up.Where, sc); err != nil {
		return err
	}
	return c.returning(up.Returning, sc, targetAlias(up.Table, up.Alias))
}

// extraItems adds UPDATE's FROM or DELETE's USING items to the target's
// scope. They are checked in a scope of their own, as a LATERAL one sees
// the items before it but not the target, which Postgres does not expose
// to them.
func (c *columnChecker) extraItems(items []sqlir.TableRef, sc *colScope, target string) error {
	own := &colScope{items: map[string]colSet{}, hidden: target}
	for _, t := range items {
		if err := c.item(t, own); err != nil {
			return err
		}
	}
	for alias, cols := range own.items {
		if _, dup := sc.items[alias]; dup {
			return c.x.unsupported(fmt.Sprintf("FROM items with the same name %q", alias))
		}
		sc.items[alias] = cols
	}
	return nil
}

func (c *columnChecker) delete(del *sqlir.DeleteStmt) error {
	sc, _ := c.target(del.Table, del.Alias)
	if err := c.extraItems(del.Using, sc, targetAlias(del.Table, del.Alias)); err != nil {
		return err
	}
	if err := c.exprs(del.Where, sc); err != nil {
		return err
	}
	return c.returning(del.Returning, sc, targetAlias(del.Table, del.Alias))
}
