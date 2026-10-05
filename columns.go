package detest

import (
	"fmt"
	"maps"
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

// colScope is the FROM items one query block sees, inside those of the
// blocks around it.
type colScope struct {
	items map[string]colSet
	types map[string]map[string]string
	// outputs are the names the select list gives, which ORDER BY, GROUP BY
	// and DISTINCT ON may refer to.
	outputs colSet
	// anyOutput is a set operation's output whose names detest does not
	// know, as its first query has a *, which any name may refer to.
	anyOutput bool
	// star is a select list with a *, whose columns are output names too,
	// and plain the output names whose item is the column of the name, the
	// same expression a * gives.
	star  bool
	plain colSet
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
	x      *sqlExec
	ctes   []map[string]colSet
	params paramUses
}

func (x *sqlExec) checkColumns(stmt sqlir.Statement) error {
	if x.tx.db.kind.InnoDB() {
		return nil
	}
	if err := x.checkSequenceCalls(stmt); err != nil {
		return err
	}
	c := &columnChecker{x: x}
	var err error
	switch st := stmt.(type) {
	case *sqlir.SelectStmt:
		_, err = c.query(st, nil)
	case *sqlir.InsertStmt:
		err = c.insert(st)
	case *sqlir.UpdateStmt:
		err = c.update(st)
	case *sqlir.DeleteStmt:
		err = c.delete(st)
	}
	if err != nil {
		return err
	}
	if what := c.params.conflict(x.tx.checking); what != "" {
		return x.unsupported(what)
	}
	return nil
}

// paramUses records how a statement uses each parameter. Postgres gives a
// parameter one type for the whole statement, taken from where it first
// stands, so one used as the array of = ANY and elsewhere as a scalar, or as
// the array of = ANY against operands of other types, fails before it runs,
// whatever value is bound to it.
type paramUses struct {
	scalars map[int]bool
	arrays  map[int][]string // the types of the operands compared with it
}

func (u *paramUses) scalar(i int) {
	if u.scalars == nil {
		u.scalars = map[int]bool{}
	}
	u.scalars[i] = true
}

func (u *paramUses) array(i int, operand string) {
	if u.arrays == nil {
		u.arrays = map[int][]string{}
	}
	u.arrays[i] = append(u.arrays[i], operand)
}

// conflict names a use Postgres refuses. Under CheckSQL, which has no schema,
// the operand types are left to the run.
func (u *paramUses) conflict(checking bool) string {
	for i, types := range u.arrays {
		if u.scalars[i] {
			return fmt.Sprintf("parameter $%d used as an array and as a scalar", i+1)
		}
		if len(types) < 2 || checking {
			continue
		}
		for _, t := range types {
			if !sameTypeFamily(t, types[0]) {
				return fmt.Sprintf("parameter $%d used as the array of = ANY or <> ALL against operands of other or unknown types", i+1)
			}
		}
	}
	return ""
}

// sameTypeFamily reports whether an operand of type a is compared with an
// element of type b by an operator detest computes as Postgres does: two
// integer types, text and varchar, or two uuids. Another pair, such as text
// against uuid or a numeric against bigint, Postgres compares through a cast
// or refuses, which detest does not model.
func sameTypeFamily(a, b string) bool {
	family := func(t string) string {
		switch t {
		case "int2", "int4", "int8", "int", "integer", "smallint", "bigint":
			return "integer"
		case "text", "varchar":
			return "text"
		case "uuid":
			return "uuid"
		}
		return ""
	}
	return family(a) != "" && family(a) == family(b)
}

func orUnknown(typ string) string {
	if typ == "" || typ == "unresolved column type" {
		return "value of a type detest does not know"
	}
	return typ
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
		return namesSet(def.columns)
	}
	if v := db.views[table]; v != nil {
		if len(v.ViewColumns) > 0 {
			return namesSet(v.ViewColumns)
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
			cols = namesSet(t.Columns)
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
	if sc.types == nil {
		sc.types = map[string]map[string]string{}
	}
	if t.Sub != nil || t.Func != nil || c.x.tx.db.views[c.x.tx.db.resolve(t.Name)] != nil {
		// Derived outputs keep their names but not their SQL types. Refuse
		// overloaded concatenation rather than guess its text signature.
		sc.types[alias] = map[string]string{}
	}
	if t.Sub == nil && t.Func == nil && len(t.Columns) == 0 {
		if def := c.x.tx.db.defs[c.x.tx.db.resolve(t.Name)]; def != nil {
			// A CTE shadows a table of the same name.
			shadowed := false
			for _, m := range c.ctes {
				_, found := m[t.Name]
				shadowed = shadowed || found
			}
			if !shadowed {
				sc.types[alias] = def.types
			}
		}
	}
	return nil
}

func (sc *colScope) columnType(r *sqlir.ColumnRef) string {
	for s := sc; s != nil; s = s.outer {
		found, unknown, unresolved, typ := 0, false, false, ""
		for alias, cols := range s.items {
			if r.Table != "" && alias != r.Table {
				continue
			}
			if has, _ := cols.has(r.Column); has {
				found++
				typ = s.types[alias][r.Column]
			} else if cols == nil {
				if s.types[alias] != nil {
					unresolved = true
				} else {
					unknown = true
				}
			}
		}
		if unknown {
			return ""
		}
		if unresolved {
			return "unresolved column type"
		}
		if found > 0 {
			if found == 1 && typ != "" {
				return typ
			}
			return "unresolved column type"
		}
		if r.Table == "excluded" && s.hasExcluded {
			return s.types["excluded"][r.Column]
		}
	}
	return ""
}

// query checks a query block and returns its output columns.
// writeWith is with for WITH ... UPDATE or DELETE, whose FROM or USING items
// are items and the rest of the statement rest. The table written is the
// table whatever a CTE is named, where FROM, USING and the subqueries would
// read the CTE, so a CTE of the target's name is refused.
//
// detest runs a CTE in full when the statement first reads it, where
// Postgres runs one only as far as the statement asks for its rows. Read by
// SET, RETURNING, a subquery or another CTE, a CTE that locks rows or has
// effects may be asked for a few rows only, as by a LIMIT, so the rows it
// locks or the effects it has would differ, and that is refused. As an item
// of FROM or USING it is accepted in the shape partlyRun allows only.
func (c *columnChecker) writeWith(with []sqlir.CTE, target, alias string, items []sqlir.TableRef, where sqlir.Expr, rest any) (pop func(), err error) {
	if len(with) == 0 {
		return func() {}, nil
	}
	ctes := map[string]bool{}
	for _, cte := range with {
		if c.x.tx.db.resolve(cte.Name) == c.x.tx.db.resolve(target) {
			return nil, c.x.unsupported(fmt.Sprintf("a CTE named %q as the table the statement writes", cte.Name))
		}
		ctes[cte.Name] = true
	}
	var others []any
	for _, t := range items {
		if t.Sub != nil || t.Func != nil || !ctes[t.Name] {
			others = append(others, t)
		}
	}
	for _, cte := range with {
		others = append(others, cte.Select)
	}
	// A CTE sees only the ones declared before it, and a name of its own or
	// of a later one means a table there. The pending CTEs are found by
	// name, so the later one would stand in for the table.
	index := map[string]int{}
	for i, cte := range with {
		index[cte.Name] = i
	}
	for i, cte := range with {
		for _, name := range freeNames(cte.Select) {
			if j, ok := index[name]; ok && j >= i {
				return nil, c.x.unsupported(fmt.Sprintf("CTE %q reading %q, which is not declared before it", cte.Name, name))
			}
		}
	}
	// A write's CTEs run when first read, by name, so a query inside the
	// statement that declares one of their names would read the write's.
	for _, s := range sqlir.Selects([]any{rest, items, with}) {
		for _, inner := range s.With {
			if ctes[inner.Name] {
				return nil, c.x.unsupported(fmt.Sprintf("CTE %q declared again inside a write that declares it", inner.Name))
			}
		}
	}
	readElsewhere := map[string]bool{}
	for _, name := range freeNames([]any{rest, others}) {
		readElsewhere[name] = true
	}
	effects := map[string]bool{}
	for _, cte := range with {
		if !queryHasEffects(cte.Select) {
			continue
		}
		if readElsewhere[cte.Name] {
			return nil, c.x.unsupported(fmt.Sprintf("CTE %q, which locks rows or has effects, read other than as an item of FROM or USING", cte.Name))
		}
		effects[cte.Name] = true
	}
	pop, err = c.with(with, nil)
	if err != nil {
		return nil, err
	}
	for _, t := range items {
		if t.Sub != nil || t.Func != nil || !effects[t.Name] {
			continue
		}
		if what := c.partlyRun(with, t, items, target, alias, where); what != "" {
			pop()
			return nil, c.x.unsupported(fmt.Sprintf("CTE %q, which locks rows or has effects, %s", t.Name, what))
		}
	}
	return pop, nil
}

// partlyRun names why Postgres might not run all of a CTE with effects that
// item reads in a write's FROM or USING, or returns "". Postgres skips the
// rest of a join, the CTE included, when the other side has no rows, which
// side that is being its planner's choice. That cannot change what the CTE
// does when the CTE reads only the target table and the WHERE only joins the
// two on their columns: the target then has rows whenever the CTE has, as
// both read the statement's snapshot, so the CTE runs in full under any plan.
// That is the shape a job queue claims a job with.
func (c *columnChecker) partlyRun(with []sqlir.CTE, item sqlir.TableRef, items []sqlir.TableRef, target, alias string, where sqlir.Expr) string {
	if len(items) != 1 {
		return "joined with other FROM or USING items"
	}
	var sel *sqlir.SelectStmt
	for _, cte := range with {
		if cte.Name == item.Name {
			sel = cte.Select
		}
	}
	tables := sqlir.TableNames(sel)
	if len(tables) == 0 {
		return "reading no table"
	}
	for _, name := range tables {
		if c.x.tx.db.resolve(name) != c.x.tx.db.resolve(target) || slices.ContainsFunc(with, func(cte sqlir.CTE) bool { return cte.Name == name }) {
			return "reading another table than the target"
		}
	}
	itemAlias := item.Alias
	if itemAlias == "" {
		itemAlias = item.Name
	}
	targetCols := colSet{}
	for _, col := range c.x.tableCols(c.x.tx.db.resolve(target)) {
		targetCols[col] = true
	}
	cteCols := c.ctes[len(c.ctes)-1][item.Name]
	side := func(e sqlir.Expr) string {
		ref, ok := e.(*sqlir.ColumnRef)
		if !ok {
			return ""
		}
		inTarget, _ := targetCols.has(ref.Column)
		inCTE, _ := cteCols.has(ref.Column)
		switch {
		case ref.Table == alias || ref.Table == "" && inTarget && !inCTE:
			return "target"
		case ref.Table == itemAlias || ref.Table == "" && inCTE && !inTarget:
			return "cte"
		}
		return ""
	}
	var joins func(e sqlir.Expr) bool
	joins = func(e sqlir.Expr) bool {
		b, ok := e.(*sqlir.BinaryExpr)
		if !ok {
			return false
		}
		if b.Op == "AND" {
			return joins(b.L) && joins(b.R)
		}
		l, r := side(b.L), side(b.R)
		return b.Op == "=" && l != "" && r != "" && l != r
	}
	if where != nil && !joins(where) {
		return "with a WHERE other than equalities between the target's columns and its own"
	}
	return ""
}

// with checks the CTEs of a statement and declares them for the rest of it,
// each seeing the ones before it. pop takes them out again.
func (c *columnChecker) with(with []sqlir.CTE, outer *colScope) (pop func(), err error) {
	if len(with) == 0 {
		return func() {}, nil
	}
	m := map[string]colSet{}
	c.ctes = append(c.ctes, m)
	pop = func() { c.ctes = c.ctes[:len(c.ctes)-1] }
	for _, cte := range with {
		s, err := c.query(cte.Select, outer)
		if err != nil {
			pop()
			return nil, err
		}
		m[cte.Name] = s
	}
	return pop, nil
}

func (c *columnChecker) query(sel *sqlir.SelectStmt, outer *colScope) (colSet, error) {
	if sel == nil {
		return nil, nil
	}
	pop, err := c.with(sel.With, outer)
	if err != nil {
		return nil, err
	}
	defer pop()
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
			if err := c.position(first, nil, o.Expr, "ORDER BY"); err != nil {
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
		if t.Star && t.Table == "" && sel.From == nil {
			return nil, c.x.tx.db.kind.Error(sqlir.SyntaxError, "SELECT * with no tables specified is not valid", "", "", "")
		}
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
	sc.outputs, sc.plain = colSet{}, colSet{}
	if len(sel.Values) > 0 {
		sc.outputs = outputColumns(sel) // column1, column2, ...
	}
	for _, t := range sel.Targets {
		sc.star = sc.star || t.Star
		if r, ok := t.Expr.(*sqlir.ColumnRef); ok && !t.Star && targetName(t) == r.Column {
			sc.plain[r.Column] = true
		}
		if !t.Star {
			n := targetName(t)
			_, seen := sc.outputs[n]
			sc.outputs[n] = !seen
		}
	}
	for _, e := range sel.GroupBy {
		if err := c.position(sel, sc, e, "GROUP BY"); err != nil {
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
		if err := c.position(sel, sc, o.Expr, "ORDER BY"); err != nil {
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
// of the select list. A * counts the columns of the items it covers, sc's;
// without them, as for a set operation, the position is let through.
func (c *columnChecker) position(sel *sqlir.SelectStmt, sc *colScope, e sqlir.Expr, clause string) error {
	k, ok := e.(*sqlir.Const)
	if !ok {
		return nil
	}
	n, ok := k.Value.(int64)
	if !ok {
		return nil
	}
	width := len(sel.Targets)
	if len(sel.Values) > 0 {
		width = len(sel.Values[0])
	}
	if slices.ContainsFunc(sel.Targets, func(t sqlir.Target) bool { return t.Star }) {
		if clause == "GROUP BY" {
			// The position may point into the columns of the *, which
			// grouping does not expand.
			return c.x.unsupported("a GROUP BY position in a select list with *")
		}
		if sc == nil {
			return nil
		}
		w, ok := starWidth(sel, sc)
		if !ok {
			return c.x.unsupported("an ORDER BY position in a select list with a * over columns detest does not know")
		}
		width = w
	}
	if n < 1 || n > int64(width) {
		return c.x.tx.db.kind.Error(sqlir.InvalidColumnReference, fmt.Sprintf("%s position %d is not in select list", clause, n), "", "", "")
	}
	return nil
}

// starWidth is the number of columns a select list with a * gives, false
// when an item the * covers has columns detest does not know, or two of
// one name, which its set does not count.
func starWidth(sel *sqlir.SelectStmt, sc *colScope) (int, bool) {
	width := 0
	for _, t := range sel.Targets {
		if !t.Star {
			width++
			continue
		}
		for alias, cols := range sc.items {
			if t.Table != "" && alias != t.Table {
				continue
			}
			if cols == nil || slices.Contains(slices.Collect(maps.Values(cols)), false) {
				return 0, false
			}
			width += len(cols)
		}
	}
	return width, true
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
	case found && sc.star && !sc.plain[r.Column] && sc.inputMay(r.Column):
		// A column of the * may give the name as well, which Postgres
		// fails as ambiguous unless the two are the same expression.
		return c.x.unsupported(fmt.Sprintf("output name %q, which a * may give too", r.Column))
	case found:
		return nil
	}
	return c.exprs(e, sc)
}

// inputMay reports whether an item of the scope has, or may have, a column
// of the name.
func (sc *colScope) inputMay(name string) bool {
	for _, cols := range sc.items {
		if found, _ := cols.has(name); cols == nil || found {
			return true
		}
	}
	return false
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
		case found == 1 && twice:
			return c.twice(r.Column)
		case unknown:
			return nil
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
			case *sqlir.ArrayCmp:
				typ := expressionType(e.X, sc.columnType)
				// CheckSQL has no schema, so an operand of a type the
				// statement does not show is left to the run.
				known := typ != "" && typ != "unresolved column type" || !c.x.tx.checking
				if e.ElemType != "" && known && !sameTypeFamily(typ, e.ElemType) {
					err = c.x.unsupported(fmt.Sprintf("= ANY or <> ALL of a %s against an array of %s", orUnknown(typ), e.ElemType))
					return
				}
				if p, ok := e.Array.(*sqlir.Param); ok {
					c.params.array(p.Index, typ)
				}
				walk(reflect.ValueOf(e.X))
				return
			case *sqlir.Param:
				c.params.scalar(e.Index)
				return
			case *sqlir.BinaryExpr:
				if what := concatTypeMismatch(e, sc.columnType); what != "" {
					err = c.x.unsupported(what)
					return
				}
			case *sqlir.FuncCall:
				if what := textArgumentMismatch(e, sc.columnType); what != "" {
					err = c.x.unsupported(what)
					return
				}
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
	sc := &colScope{items: map[string]colSet{targetAlias(table, alias): cols}, types: map[string]map[string]string{}}
	if def := c.x.tx.db.defs[c.x.tx.db.resolve(table)]; def != nil {
		sc.types[targetAlias(table, alias)] = def.types
	}
	return sc, cols
}

func targetAlias(table, alias string) string {
	if alias == "" {
		return relname(table)
	}
	return alias
}

// assigned checks the columns a write assigns, which must be the table's,
// each once.
func (c *columnChecker) assigned(table string, cols colSet, names []string, insert bool) error {
	seen := map[string]bool{}
	for _, n := range names {
		if cols != nil && !cols[n] {
			return c.undefinedIn(table, n)
		}
		if seen[n] && insert {
			return c.x.tx.db.kind.Error(sqlir.DuplicateColumn, fmt.Sprintf("column %q specified more than once", n), relname(table), n, "")
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
		// With FROM or USING items, Postgres's RETURNING * gives their columns
		// after the target's, where detest gives the target's only.
		if t.Table == "" && len(sc.items) > 1 {
			return c.x.unsupported("RETURNING * of a write with FROM or USING items")
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
	if c.x.tx.db.checkTable(c.x.tx.db.resolve(ins.Table)) != nil {
		return nil // the missing table is the error, which the INSERT reports first
	}
	if ins.Select != nil && len(ins.Select.With) > 0 {
		// WITH ... INSERT's CTEs go to its query alone, where Postgres
		// lets RETURNING and ON CONFLICT read them too.
		var rest []any
		rest = append(rest, ins.Returning)
		if oc := ins.OnConflict; oc != nil {
			rest = append(rest, oc.Set, oc.Where)
		}
		for _, name := range freeNames(rest) {
			if slices.ContainsFunc(ins.Select.With, func(cte sqlir.CTE) bool { return cte.Name == name }) {
				return c.x.unsupported(fmt.Sprintf("CTE %q read outside the INSERT's query", name))
			}
		}
	}
	sc, cols := c.target(ins.Table, ins.Alias)
	if err := c.assigned(ins.Table, cols, ins.Columns, true); err != nil {
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
		if err := c.assigned(ins.Table, cols, assignedColumns(oc.Set), false); err != nil {
			return err
		}
		up := &colScope{items: sc.items, types: maps.Clone(sc.types), excluded: cols, hasExcluded: true}
		up.types["excluded"] = sc.types[targetAlias(ins.Table, ins.Alias)]
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
	if c.x.tx.db.checkTable(c.x.tx.db.resolve(up.Table)) != nil {
		return nil // the missing table is the error, which the UPDATE reports first
	}
	pop, err := c.writeWith(up.With, up.Table, targetAlias(up.Table, up.Alias), up.From, up.Where, []any{up.Set, up.Where, up.Returning, up.OrderBy, up.Limit})
	if err != nil {
		return err
	}
	defer pop()
	sc, cols := c.target(up.Table, up.Alias)
	if err := c.assigned(up.Table, cols, assignedColumns(up.Set), false); err != nil {
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
		sc.types[alias] = own.types[alias]
	}
	return nil
}

func (c *columnChecker) delete(del *sqlir.DeleteStmt) error {
	if c.x.tx.db.checkTable(c.x.tx.db.resolve(del.Table)) != nil {
		return nil // the missing table is the error, which the DELETE reports first
	}
	pop, err := c.writeWith(del.With, del.Table, targetAlias(del.Table, del.Alias), del.Using, del.Where, []any{del.Where, del.Returning, del.OrderBy, del.Limit})
	if err != nil {
		return err
	}
	defer pop()
	sc, _ := c.target(del.Table, del.Alias)
	if err := c.extraItems(del.Using, sc, targetAlias(del.Table, del.Alias)); err != nil {
		return err
	}
	if err := c.exprs(del.Where, sc); err != nil {
		return err
	}
	return c.returning(del.Returning, sc, targetAlias(del.Table, del.Alias))
}
