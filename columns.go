package detest

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"

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
	// colls are the collations the columns of the base tables in scope
	// declare, by alias. A derived item has no entry, as detest does not
	// carry the collation of its query's columns.
	colls map[string]map[string]string
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
	// outputExprs are the select list's expressions by output name, and
	// outputList in order unless a * stands in it, which an ORDER BY of the
	// name or the position orders by the collation of.
	outputExprs map[string]sqlir.Expr
	outputList  []sqlir.Expr
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
	// cteTypes are the types of the CTEs' columns, by CTE and column, as
	// ctes holds their names.
	cteTypes []map[string]map[string]string
	// outTypes are the types of a query's output columns by position, ""
	// where expressionType does not know one, for a FROM item, a CTE, a
	// view or a scalar subquery made of it.
	outTypes map[*sqlir.SelectStmt][]string
	// outNamed are the types of a query's output columns by name, which a
	// select list with a * has where outTypes has none; "" for a name two
	// columns give or of a type expressionType does not know.
	outNamed map[*sqlir.SelectStmt]map[string]string
	params   paramUses
	// untyped holds the reads of string literals and parameters, run after
	// the walk, as Postgres types the parameters before it binds them.
	untyped []func() error
	// timeConflict names a parameter given two date and time types.
	timeConflict string
	// paramNumbers is the number type each parameter takes, and
	// numberConflict names one given two, by numberRank.
	paramNumbers   map[int]string
	numberConflict string
	// paramAt are the uses of parameters the walk met, paramTyped those a
	// check gave a type, and paramRead the parameters whose input is read
	// by a cast to a type before the run.
	paramAt, paramTyped map[*sqlir.Param]bool
	paramRead           map[int]bool
	// subTypes are the types subqueryType resolved, shared with the
	// checkers it starts, so that the branch checks, which resolve a
	// subquery before the walk reaches it, resolve each one once rather
	// than again at every level of nesting above it.
	subTypes map[*sqlir.SubQuery]string
	// valuesTypes are the column types of a VALUES query, resolved over
	// its rows in the scope the walk checked them in, which setOpTypes has
	// no other way to see for a correlated column.
	valuesTypes map[*sqlir.SelectStmt][]string
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
	case *sqlir.CreateTableAsStmt:
		// Checked as Postgres plans the query, and so that the collations
		// and the types of its columns are known to the table it creates.
		if _, err = c.query(st.Select, nil); err == nil {
			first := st.Select
			for first.SetOp != "" && first.Larg != nil {
				first = first.Larg
			}
			x.ctasTypes, x.ctasNamed = c.setOpTypes(st.Select), c.outNamed[first]
		}
	}
	if err != nil {
		return err
	}
	if what := c.params.conflict(x.tx.checking); what != "" {
		return x.unsupported(what)
	}
	if c.timeConflict != "" {
		return x.unsupported(c.timeConflict)
	}
	if c.numberConflict != "" {
		return x.unsupported(c.numberConflict)
	}
	if what := c.untrackedParam(); what != "" {
		return x.unsupported(what)
	}
	for _, read := range c.untyped {
		if err := read(); err != nil {
			return err
		}
	}
	return nil
}

// paramUses records how a statement uses each parameter. Postgres gives a
// parameter one type for the whole statement, taken from where it first
// stands, so one used as the array of = ANY and elsewhere as a scalar, as
// the array of = ANY against operands of other types, or as a scalar of two
// families, such as a boolean column's value and an integer column's, fails
// before it runs, whatever value is bound to it.
type paramUses struct {
	scalars  map[int]bool
	arrays   map[int][]string // the types of the operands compared with it
	compared map[int]string   // the family of the first type it takes as a scalar
	mixed    map[int]bool     // taken as a scalar of two families
}

// comparedWith records the family f, by paramTypeFamily, of a type the
// scalar parameter i takes, from an operand it is compared with or from
// where it stands, as the column it is written to.
func (u *paramUses) comparedWith(i int, f string) {
	if f == "" {
		return
	}
	if u.compared == nil {
		u.compared, u.mixed = map[int]string{}, map[int]bool{}
	}
	if first, ok := u.compared[i]; !ok {
		u.compared[i] = f
	} else if first != f {
		u.mixed[i] = true
	}
}

// paramTypeFamily is paramFamily, with an enum a family of its own, as
// Postgres gives a parameter one type for the statement and an enum casts
// implicitly to no other type.
func (c *columnChecker) paramTypeFamily(typ string) string {
	if t := c.x.tx.db.enumOf(typ); t != nil {
		return "enum " + t.qualified
	}
	switch f := paramFamily(typ); {
	case f != "":
		return f
	case typ == "bpchar":
		return "text"
	case typ == "" || typ == "unresolved column type":
		return ""
	}
	// A type paramFamily does not group is a family of its own, so that a
	// parameter taken as it and as another type is refused, even where
	// Postgres would cast one to the other.
	return "type " + typ
}

// paramFamily groups the types whose values detest reads a parameter
// as alike. Integers and floats are kept apart, although Postgres compares
// them across types, because the parameter takes the first one's type and
// detest reads it against each operand.
func paramFamily(typ string) string {
	switch typ {
	case "int2", "int4", "int8", "int", "integer", "smallint", "bigint":
		return "integer"
	case "float4", "float8", "real", "double precision", "numeric", "decimal":
		return "float"
	case "bool", "boolean", "uuid", "interval":
		return typ
	case "text", "varchar":
		return "text"
	}
	return ""
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
	if len(u.mixed) > 0 {
		return fmt.Sprintf("parameter $%d taken as values of different types", slices.Min(slices.Collect(maps.Keys(u.mixed)))+1)
	}
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
	// A derived item's columns have the types of its query's select list
	// where expressionType knows them, and an unresolved type otherwise, so
	// the checks that need a type refuse rather than guess.
	switch {
	case t.Func != nil:
		sc.types[alias] = map[string]string{}
	case t.Sub != nil:
		sc.types[alias] = namedTypes(t.Sub, c.outTypes[t.Sub], t.Columns)
	default:
		db := c.x.tx.db
		if types, ok := c.cteTypesOf(t.Name); ok {
			// A CTE shadows a table of the same name.
			if types == nil || len(t.Columns) > 0 {
				types = map[string]string{}
			}
			sc.types[alias] = types
		} else if v := db.views[db.resolve(t.Name)]; v != nil {
			if len(t.Columns) > 0 {
				sc.types[alias] = map[string]string{}
			} else {
				sc.types[alias] = c.viewTypes(v)
			}
		} else if def := db.defs[db.resolve(t.Name)]; def != nil && len(t.Columns) == 0 {
			sc.types[alias] = def.types
			sc.noteCollations(alias, def)
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
	// Its rows must come from the target's rows alone, so that it has none
	// when the target has none. A table merely named in a branch, a nested
	// WITH or a subquery does not make that so, and an aggregate gives a row
	// over no rows.
	if f := sel.From; sel.With != nil || sel.SetOp != "" || sel.Values != nil || len(sel.Joins) > 0 ||
		f == nil || f.Sub != nil || f.Func != nil || c.x.tx.db.resolve(f.Name) != c.x.tx.db.resolve(target) ||
		slices.ContainsFunc(with, func(cte sqlir.CTE) bool { return cte.Name == f.Name }) {
		return "reading other rows than the target's alone"
	}
	if len(sel.GroupBy) > 0 || sel.Having != nil || slices.ContainsFunc(sel.Targets, func(t sqlir.Target) bool { return hasAggregate(t.Expr) }) {
		return "aggregating the target's rows"
	}
	// LIMIT and OFFSET are evaluated before the first row is read.
	if hasEffects(sel.Limit) || hasEffects(sel.Offset) {
		return "with effects in its LIMIT or OFFSET"
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
	m, tm := map[string]colSet{}, map[string]map[string]string{}
	c.ctes = append(c.ctes, m)
	c.cteTypes = append(c.cteTypes, tm)
	pop = func() {
		c.ctes = c.ctes[:len(c.ctes)-1]
		c.cteTypes = c.cteTypes[:len(c.cteTypes)-1]
	}
	for _, cte := range with {
		s, err := c.query(cte.Select, outer)
		if err != nil {
			pop()
			return nil, err
		}
		m[cte.Name] = s
		tm[cte.Name] = namedTypes(cte.Select, c.outTypes[cte.Select], nil)
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
		if err := c.noteSetOpColumns(sel); err != nil {
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
	if len(sel.Values) > 0 {
		// Postgres resolves each column of a VALUES query over its rows
		// when it plans the statement, and fails one of two types (42804)
		// whether or not a LIMIT drops the rows. INSERT ... VALUES, whose
		// rows each take the column's type, is checked in insert.
		for _, row := range sel.Values[1:] {
			if len(row) != len(sel.Values[0]) {
				// Postgres fails this when it parses the statement, before
				// it reads any input by the columns' types.
				return nil, c.x.tx.db.kind.Error(sqlir.SyntaxError, "VALUES lists must all be the same length", "", "", "")
			}
		}
		types := make([]string, len(sel.Values[0]))
		for i := range sel.Values[0] {
			col := setOpColumnExprs(sel, i)
			if err := c.branchTypes(col, sc, "VALUES rows"); err != nil {
				return nil, err
			}
			// The untyped rows take the type of the typed ones. A column
			// of no typed row is left to a set operation around it.
			types[i] = commonType(col, sc.columnType, func(q *sqlir.SubQuery) string { return c.subqueryType(q, sc) })
			c.timeParams(types[i], col...)
			for _, e := range col {
				c.inputAs(e, types[i])
			}
		}
		if c.valuesTypes == nil {
			c.valuesTypes = map[*sqlir.SelectStmt][]string{}
		}
		c.valuesTypes[sel] = types
	}
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
		if !t.Star {
			sc.outputList = append(sc.outputList, t.Expr)
		}
		sc.star = sc.star || t.Star
		if r, ok := t.Expr.(*sqlir.ColumnRef); ok && !t.Star && targetName(t) == r.Column {
			sc.plain[r.Column] = true
		}
		if !t.Star {
			n := targetName(t)
			_, seen := sc.outputs[n]
			sc.outputs[n] = !seen
			if sc.outputExprs == nil {
				sc.outputExprs = map[string]sqlir.Expr{}
			}
			sc.outputExprs[n] = t.Expr
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
	c.noteOutputTypes(sel, sc)
	return outputColumns(sel), nil
}

// noteOutputTypes records the types of a query's select list items that
// expressionType knows, by position, and by name. A *'s are recorded by
// name only, as the order of the columns it gives is not kept, and a set
// operation's not at all, so their columns stay of unresolved type.
func (c *columnChecker) noteOutputTypes(sel *sqlir.SelectStmt, sc *colScope) {
	var types []string
	named := map[string]string{}
	seen := map[string]bool{}
	note := func(name, typ string) {
		if seen[name] {
			typ = ""
		}
		seen[name] = true
		named[name] = typ
	}
	star, starKnown := false, true
	for _, t := range sel.Targets {
		if t.Star {
			star = true
			if cols := c.starColumns(sel, t); cols != nil {
				types = append(types, cols...)
			} else {
				starKnown = false
			}
			for alias, cols := range sc.items {
				if t.Table != "" && alias != t.Table {
					continue
				}
				for name, once := range cols {
					typ := sc.types[alias][name]
					if !once || typ == "unresolved column type" {
						typ = ""
					}
					note(name, typ)
				}
			}
			continue
		}
		typ := expressionTypeIn(t.Expr, sc.columnType, func(q *sqlir.SubQuery) string { return c.subqueryType(q, sc) })
		if typ == "unresolved column type" {
			typ = ""
		}
		if typ == "" && nestedBranches(t.Expr) != nil && untypedBranch(t.Expr) {
			typ = "text" // as Postgres resolves a CASE or COALESCE of untyped branches
		}
		types = append(types, typ)
		note(targetName(t), typ)
	}
	if c.outTypes == nil {
		c.outTypes = map[*sqlir.SelectStmt][]string{}
		c.outNamed = map[*sqlir.SelectStmt]map[string]string{}
	}
	if !star || starKnown {
		c.outTypes[sel] = types
	}
	c.outNamed[sel] = named
}

// starColumns are the types of the columns the * t of sel gives, in order,
// when its one FROM item is a table the schema declares, the order of whose
// columns the check knows, and nil otherwise.
func (c *columnChecker) starColumns(sel *sqlir.SelectStmt, t sqlir.Target) []string {
	from := sel.From
	if from == nil || len(sel.Joins) > 0 || from.Sub != nil || from.Func != nil || len(from.Columns) > 0 {
		return nil
	}
	if t.Table != "" && t.Table != targetAlias(from.Name, from.Alias) {
		return nil
	}
	db := c.x.tx.db
	if _, cte := c.cteTypesOf(from.Name); cte || db.views[db.resolve(from.Name)] != nil {
		return nil
	}
	def := db.defs[db.resolve(from.Name)]
	if def == nil {
		return nil
	}
	types := make([]string, len(def.columns))
	for i, col := range def.columns {
		if typ := def.types[col]; typ != "unresolved column type" {
			types[i] = typ
		}
	}
	return types
}

// noteSetOpColumns records the columns of the set operation sel its
// queries resolve to bytea or to text, by position and, for the columns of
// a * in the first query, whose position the check does not know, by name.
func (c *columnChecker) noteSetOpColumns(sel *sqlir.SelectStmt) error {
	db := c.x.tx.db
	if db.kind.InnoDB() {
		return nil
	}
	if l, ok := c.knownWidth(sel.Larg); ok {
		if r, ok := c.knownWidth(sel.Rarg); ok && l != r {
			// Postgres fails this before it reads any input by the
			// columns' types, as the run does once it has both queries.
			return db.kind.Error(sqlir.SyntaxError, fmt.Sprintf("each %s query must have the same number of columns", strings.ToUpper(sel.SetOp)), "", "", "")
		}
	}
	for _, q := range []*sqlir.SelectStmt{sel.Larg, sel.Rarg} {
		if starQuery(q) && slices.ContainsFunc(q.Targets, func(t sqlir.Target) bool { return !t.Star && untypedExpr(t.Expr) }) {
			// The check does not place the targets beside a * by position,
			// which Postgres types their input by.
			return c.x.unsupported("a set operation of a query with a * beside a string literal or a parameter")
		}
	}
	types := c.setOpTypes(sel)
	cols := setOpColumns{byPos: make([]string, len(types)), byName: map[string]string{}}
	for i, t := range types {
		cols.byPos[i] = db.branchKind(resolvedType(t, setOpColumnExprs(sel, i)))
		lt, rt := c.setOpBranchType(sel.Larg, i), c.setOpBranchType(sel.Rarg, i)
		// Postgres fails a pair it cannot resolve to one type when it plans
		// the statement (42804), which the run refuses only once it has
		// rows of both. Two types are taken as one only where valueFamily
		// groups them, so a pair it does not group is refused even where
		// Postgres resolves it, such as a date with a timestamp, rather
		// than modeled pair by pair.
		if lt != "" && rt != "" && lt != rt && (valueFamily(lt) == "" || valueFamily(lt) != valueFamily(rt)) {
			return c.x.unsupported("a set operation over a column of values of two types")
		}
		pair := lt
		if pair == "" || rt == "bytea" && rt != "" {
			pair = rt
		}
		if pair == "" && (hasStar(sel.Larg) || hasStar(sel.Rarg)) && (untypedColumn(sel.Larg, i) || untypedColumn(sel.Rarg, i)) {
			// The column of a * has a type the check does not know by
			// position, which Postgres reads the other query's input by.
			return c.x.unsupported("a set operation of a * against a string literal or a parameter")
		}
		// Untyped queries alone resolve to text, which the parameters among
		// them take.
		pair = resolvedType(pair, setOpColumnExprs(sel, i))
		// Only the queries of this pair are read by its type. Those of a
		// nested set operation took that one's, as Postgres resolves the
		// pairs from the inside out.
		for _, q := range []*sqlir.SelectStmt{sel.Larg, sel.Rarg} {
			if q != nil && q.SetOp == "" {
				exprs := setOpColumnExprs(q, i)
				typ := pair
				if own := c.setOpBranchType(q, i); len(q.Values) > 0 && own != "" {
					// VALUES reads its rows by its own column's type first.
					typ = own
				}
				c.timeParams(typ, exprs...)
				for _, e := range exprs {
					c.inputAs(e, typ)
				}
			}
		}
	}
	first := sel
	for first.SetOp != "" && first.Larg != nil {
		first = first.Larg
	}
	for name, t := range c.outNamed[first] {
		if k := db.branchKind(t); k != "" {
			cols.byName[name] = k
		}
	}
	if !slices.ContainsFunc(cols.byPos, func(k string) bool { return k != "" }) && len(cols.byName) == 0 {
		return nil
	}
	if c.x.setOpColumns == nil {
		c.x.setOpColumns = map[*sqlir.SelectStmt]setOpColumns{}
	}
	c.x.setOpColumns[sel] = cols
	return nil
}

// starQuery reports whether q is a query whose select list has a *.
func starQuery(q *sqlir.SelectStmt) bool {
	return q != nil && q.SetOp == "" && slices.ContainsFunc(q.Targets, func(t sqlir.Target) bool { return t.Star })
}

// hasStar reports whether q, or a query of q when it is a set operation,
// has a * in its select list.
func hasStar(q *sqlir.SelectStmt) bool {
	if q != nil && q.SetOp != "" {
		return hasStar(q.Larg) || hasStar(q.Rarg)
	}
	return starQuery(q)
}

// untypedColumn reports whether the i-th column of q, a query that is no set
// operation, is a string literal or a parameter.
func untypedColumn(q *sqlir.SelectStmt, i int) bool {
	if q == nil || q.SetOp != "" {
		return false
	}
	return slices.ContainsFunc(setOpColumnExprs(q, i), untypedExpr)
}

// untypedOutput reports whether e, a scalar subquery's one target, is of
// no type of its own, which Postgres resolves to text at the subquery's
// output: untyped input, a CASE or COALESCE of it alone, or a bare NULL.
func untypedOutput(e sqlir.Expr) bool {
	if k, ok := e.(*sqlir.Const); ok && k.Value == nil {
		return true
	}
	return untypedBranch(e)
}

func isParam(e sqlir.Expr) bool {
	_, ok := e.(*sqlir.Param)
	return ok
}

// recordedSubqueryType is the type of a scalar subquery's one column as the
// walk recorded it, "" where it recorded none.
func (c *columnChecker) recordedSubqueryType(q *sqlir.SubQuery) string {
	sel := q.Select
	switch {
	case sel == nil:
		return ""
	case sel.SetOp != "" || len(sel.Values) > 0:
		if t := c.setOpTypes(sel); len(t) == 1 {
			return resolvedType(t[0], setOpColumnExprs(sel, 0))
		}
		return ""
	case len(sel.Targets) == 1 && untypedOutput(sel.Targets[0].Expr):
		return "text" // as subqueryType resolves it
	}
	if t := c.outTypes[sel]; len(t) == 1 {
		return t[0]
	}
	return ""
}

// setOpBranchType is the type of the i-th column of q, a query of a set
// operation, before the set operation resolves it with the other: a nested
// set operation's own, text when every query of it is untyped there, and ""
// for a column of no type of its own, which takes the other query's.
func (c *columnChecker) setOpBranchType(q *sqlir.SelectStmt, i int) string {
	if q == nil {
		return ""
	}
	if len(q.Values) > 0 {
		if t := c.setOpTypes(q); i < len(t) {
			return t[i]
		}
		return ""
	}
	if q.SetOp == "" {
		if t := c.outTypes[q]; i < len(t) {
			return t[i]
		}
		return ""
	}
	var t string
	if ts := c.setOpTypes(q); i < len(ts) {
		t = ts[i]
	}
	return resolvedType(t, setOpColumnExprs(q, i))
}

// setOpColumnExprs are the expressions of the i-th column of every query
// of the set operation sel, nil when one of them is a * or has no i-th.
func setOpColumnExprs(sel *sqlir.SelectStmt, i int) []sqlir.Expr {
	if sel.SetOp != "" && sel.Larg != nil && sel.Rarg != nil {
		l, r := setOpColumnExprs(sel.Larg, i), setOpColumnExprs(sel.Rarg, i)
		if l == nil || r == nil {
			return nil
		}
		return append(l, r...)
	}
	var out []sqlir.Expr
	for _, row := range sel.Values {
		if i >= len(row) {
			return nil
		}
		out = append(out, row[i])
	}
	if len(sel.Values) > 0 {
		return out
	}
	if i >= len(sel.Targets) || slices.ContainsFunc(sel.Targets, func(t sqlir.Target) bool { return t.Star }) {
		return nil
	}
	return []sqlir.Expr{sel.Targets[i].Expr}
}

// setOpColumns are the kinds of type, by branchKind, the columns of a set
// operation resolve to, by position and by the name of the first query's
// column.
type setOpColumns struct {
	byPos  []string
	byName map[string]string
}

// kind is the kind of type of the j-th column, named name.
func (s setOpColumns) kind(j int, name string) string {
	if j < len(s.byPos) && s.byPos[j] != "" {
		return s.byPos[j]
	}
	return s.byName[name]
}

// setOpTypes are the types of the output columns of sel by position, over
// the queries of a set operation: bytea where one of them gives bytea, as
// Postgres resolves the others to it or fails, and the first type one
// gives otherwise.
func (c *columnChecker) setOpTypes(sel *sqlir.SelectStmt) []string {
	if len(sel.Values) > 0 {
		// VALUES resolves each column over its rows, and leaves one of
		// untyped rows alone untyped, where a set operation makes it text.
		out := make([]string, len(sel.Values[0]))
		for i := range out {
			col := setOpColumnExprs(sel, i)
			if t, ok := c.valuesTypes[sel]; ok && i < len(t) {
				out[i] = t[i]
			} else {
				out[i] = commonType(col, nil, c.recordedSubqueryType)
			}
			if out[i] == "" && slices.ContainsFunc(col, func(e sqlir.Expr) bool { return nestedBranches(e) != nil && untypedBranch(e) }) {
				// A CASE or COALESCE of untyped branches alone is text,
				// as it is in a select list.
				out[i] = "text"
			}
		}
		return out
	}
	if sel.SetOp == "" || sel.Larg == nil || sel.Rarg == nil {
		return c.outTypes[sel]
	}
	l, r := c.setOpTypes(sel.Larg), c.setOpTypes(sel.Rarg)
	out := slices.Clone(l)
	if len(out) < len(r) {
		// The left query's types are unknown by position, as of a * or
		// VALUES, which leaves the right one's.
		out = append(out, make([]string, len(r)-len(out))...)
	}
	for i, t := range r {
		switch {
		case i >= len(out):
		case out[i] == "" || t == "bytea":
			if t != "" {
				out[i] = t
			}
		case numberRank[out[i]] > 0 && numberRank[t] > numberRank[out[i]], timeRank[out[i]] > 0 && timeRank[t] > timeRank[out[i]]:
			// Numbers, and dates and timestamps, meet at the widest, as
			// they do in commonType.
			out[i] = t
		}
	}
	return out
}

// namedTypes are the output types of sel by the names its columns have,
// under those an AS alias(a, b) or a view's column list gives the first
// ones. A name two columns have is left unresolved, as its type is either.
func namedTypes(sel *sqlir.SelectStmt, types []string, names []string) map[string]string {
	out := map[string]string{}
	// Without any, as of a * or a set operation, the item's columns are of
	// unresolved type rather than unknown.
	cols := outputNames(sel)
	if types == nil || len(cols) != len(types) {
		return out
	}
	cols = renamedCols(cols, names)
	seen := map[string]int{}
	for _, n := range cols {
		seen[n]++
	}
	for i, n := range cols {
		if seen[n] == 1 && types[i] != "" {
			out[n] = types[i]
		}
	}
	return out
}

// subqueryType is the type of a scalar subquery's one column, read in its
// own scope inside sc: text for a string literal or a parameter, as
// Postgres resolves one, and "" where expressionType does not know it.
func (c *columnChecker) subqueryType(q *sqlir.SubQuery, sc *colScope) string {
	if q.Select == nil {
		return ""
	}
	if q.Select.SetOp != "" || len(q.Select.Values) > 0 {
		if typ, ok := c.subTypes[q]; ok {
			return typ
		}
		typ := c.setOpSubqueryType(q, sc)
		if c.subTypes == nil {
			c.subTypes = map[*sqlir.SubQuery]string{}
		}
		c.subTypes[q] = typ
		return typ
	}
	if len(q.Select.Targets) != 1 {
		return ""
	}
	if untypedOutput(q.Select.Targets[0].Expr) {
		return "text"
	}
	if typ, ok := c.subTypes[q]; ok {
		return typ
	}
	typ := c.checkedSubqueryType(q, sc)
	if c.subTypes == nil {
		c.subTypes = map[*sqlir.SubQuery]string{}
	}
	c.subTypes[q] = typ
	return typ
}

// checkedSubqueryType is subqueryType's type of q, a subquery of one
// target, from the walk's record or from checking q.
func (c *columnChecker) checkedSubqueryType(q *sqlir.SubQuery, sc *colScope) string {
	if _, walked := c.outNamed[q.Select]; walked {
		// The walk has checked the subquery already, in this scope, and
		// checking it again here would double the work at each level of
		// nesting.
		types, ok := c.outTypes[q.Select]
		switch {
		case !ok:
			return "unresolved column type"
		case len(types) == 1:
			return types[0]
		}
		return ""
	}
	if c.subTypes == nil {
		c.subTypes = map[*sqlir.SubQuery]string{}
	}
	vc := &columnChecker{x: c.x, ctes: c.ctes, cteTypes: c.cteTypes, subTypes: c.subTypes}
	if _, err := vc.query(q.Select, sc); err != nil {
		return ""
	}
	types, ok := vc.outTypes[q.Select]
	if !ok {
		// A set operation's or a *'s column may be an interval.
		return "unresolved column type"
	}
	if len(types) == 1 {
		return types[0]
	}
	return ""
}

// setOpSubqueryType is subqueryType's type of q, a set operation or a
// VALUES query, as setOpTypes resolves its one column once q is checked.
func (c *columnChecker) setOpSubqueryType(q *sqlir.SubQuery, sc *colScope) string {
	if c.subTypes == nil {
		c.subTypes = map[*sqlir.SubQuery]string{}
	}
	vc := &columnChecker{x: c.x, ctes: c.ctes, cteTypes: c.cteTypes, subTypes: c.subTypes}
	if _, err := vc.query(q.Select, sc); err != nil {
		return ""
	}
	types := vc.setOpTypes(q.Select)
	if len(types) != 1 {
		return ""
	}
	if typ := resolvedType(types[0], setOpColumnExprs(q.Select, 0)); typ != "" {
		return typ
	}
	// A column of no type the check knows, as of a *, may be an interval.
	return "unresolved column type"
}

// viewTypes are the output types of a view's query, checked apart from the
// statement that reads the view.
func (c *columnChecker) viewTypes(v *sqlir.SchemaChange) map[string]string {
	vc := &columnChecker{x: c.x}
	if _, err := vc.query(v.View, nil); err != nil {
		return map[string]string{}
	}
	return namedTypes(v.View, vc.outTypes[v.View], v.ViewColumns)
}

// cteTypesOf are the column types of the CTE name, the innermost of that
// name, and whether a CTE of the name is in scope.
func (c *columnChecker) cteTypesOf(name string) (map[string]string, bool) {
	for i, m := range slices.Backward(c.ctes) {
		if _, ok := m[name]; ok {
			return c.cteTypes[i][name], true
		}
	}
	return nil, false
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

// insertWidth fails an INSERT ... SELECT whose query gives more columns
// than it writes, or fewer than its column list names, as Postgres does
// when it plans the statement (42601), before it reads any input by the
// columns' types. Without a column list, fewer write the first columns.
func (c *columnChecker) insertWidth(ins *sqlir.InsertStmt) error {
	db := c.x.tx.db
	if db.kind.InnoDB() {
		return nil
	}
	w, ok := c.knownWidth(ins.Select)
	if !ok {
		return nil
	}
	n := len(ins.Columns)
	if n == 0 {
		def := db.defs[db.resolve(ins.Table)]
		if def == nil {
			return nil
		}
		n = len(def.columns)
	}
	switch {
	case w > n:
		return db.kind.Error(sqlir.SyntaxError, "INSERT has more expressions than target columns", relname(ins.Table), "", "")
	case w < n && len(ins.Columns) > 0:
		return db.kind.Error(sqlir.SyntaxError, "INSERT has more target columns than expressions", relname(ins.Table), "", "")
	}
	return nil
}

// knownWidth is the number of columns q gives, false for a * whose columns
// the walk did not record by position.
func (c *columnChecker) knownWidth(q *sqlir.SelectStmt) (int, bool) {
	switch {
	case q == nil:
		return 0, false
	case q.SetOp != "":
		return c.knownWidth(q.Larg)
	case len(q.Values) > 0:
		return len(q.Values[0]), true
	case slices.ContainsFunc(q.Targets, func(t sqlir.Target) bool { return t.Star }):
		t, ok := c.outTypes[q]
		return len(t), ok
	}
	return len(q.Targets), true
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
	if k, ok := e.(*sqlir.Const); ok {
		if n, ok := k.Value.(int64); ok {
			// A position past a * or in a set operation names a column
			// whose collation detest does not know, noted as nil.
			var out sqlir.Expr
			if !sc.star && n >= 1 && int(n) <= len(sc.outputList) {
				out = sc.outputList[n-1]
			}
			c.x.noteOutput(k, out)
		}
	}
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
		c.x.noteOutput(r, sc.outputExprs[r.Column])
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
				c.x.noteType(e, typ)
				if p, ok := e.Array.(*sqlir.Param); ok {
					c.params.array(p.Index, typ)
				}
				if sample := c.x.tx.db.typeSample(typ); sample != nil && e.ElemType == "" {
					if c.x.arrayTypes == nil {
						c.x.arrayTypes = map[*sqlir.ArrayCmp]any{}
					}
					c.x.arrayTypes[e] = sample
				}
				walk(reflect.ValueOf(e.X))
				return
			case *sqlir.Param:
				c.params.scalar(e.Index)
				if c.paramAt == nil {
					c.paramAt = map[*sqlir.Param]bool{}
				}
				c.paramAt[e] = true
				return
			case *sqlir.IsNull:
				if p, ok := e.X.(*sqlir.Param); ok {
					if !c.x.tx.db.kind.InnoDB() {
						// Postgres fails a parameter it meets first here,
						// as IS NULL gives it no type (42P18), and the
						// order it meets the uses in is not the
						// statement's: UPDATE reads WHERE before SET.
						err = c.x.unsupported(fmt.Sprintf("parameter $%d under IS NULL without a cast", p.Index+1))
						return
					}
					c.typedParam(p) // IS NULL gives it no type to differ from
				}
			case *sqlir.BinaryExpr:
				if what := concatTypeMismatch(e, sc.columnType); what != "" {
					err = c.x.unsupported(what)
					return
				}
				if what := dateDifferenceMismatch(e, sc.columnType); what != "" {
					err = c.x.unsupported(what)
					return
				}
				switch e.Op {
				case "=", "<>", "!=", "<", "<=", ">", ">=":
					if err = c.operandTypes(sc, e.L, e.R); err != nil {
						return
					}
					c.untypedOperands(sc, e.L, e.R)
				}
			case *sqlir.InExpr:
				for _, item := range e.List {
					if err = c.operandTypes(sc, e.X, item); err != nil {
						return
					}
					c.untypedOperands(sc, e.X, item)
				}
			case *sqlir.CaseExpr:
				if what := intervalBranchMismatch(caseBranches(e), sc.columnType, func(q *sqlir.SubQuery) string { return c.subqueryType(q, sc) }); what != "" {
					err = c.x.unsupported(what)
					return
				}
				if err = c.branchTypes(caseBranches(e), sc, "branches"); err != nil {
					return
				}
				// The branches take the type of the typed ones.
				typ := expressionTypeIn(e, sc.columnType, func(q *sqlir.SubQuery) string { return c.subqueryType(q, sc) })
				c.timeParams(typ, caseBranches(e)...)
				for _, b := range caseBranches(e) {
					c.inputAs(b, resolvedType(typ, caseBranches(e)))
				}
				c.x.noteType(e, typ)
				c.x.noteBranchType(e, resolvedType(typ, caseBranches(e)))
				if e.Arg != nil {
					for _, w := range e.Whens {
						if err = c.operandTypes(sc, e.Arg, w.When); err != nil {
							return
						}
						c.untypedOperands(sc, e.Arg, w.When)
					}
				}
			case *sqlir.FuncCall:
				if what := textArgumentMismatch(e, sc.columnType); what != "" {
					err = c.x.unsupported(what)
					return
				}
				if e.Name == "nullif" && len(e.Args) == 2 {
					c.untypedOperands(sc, e.Args[0], e.Args[1])
				}
				if what := intervalBranchMismatch(branchArgs(e), sc.columnType, func(q *sqlir.SubQuery) string { return c.subqueryType(q, sc) }); what != "" {
					err = c.x.unsupported(what)
					return
				}
				switch e.Name {
				case "coalesce", "greatest", "least", "nullif":
					if err = c.branchTypes(e.Args, sc, "branches"); err != nil {
						return
					}
					// The arguments take the type of the typed ones.
					typ := commonType(e.Args, sc.columnType, func(q *sqlir.SubQuery) string { return c.subqueryType(q, sc) })
					c.timeParams(typ, e.Args...)
					for _, a := range e.Args {
						c.inputAs(a, resolvedType(typ, e.Args))
					}
					c.x.noteType(e, typ)
					c.x.noteBranchType(e, resolvedType(typ, e.Args))
				case "date_trunc":
					if len(e.Args) == 2 {
						c.x.noteType(e.Args[1], expressionType(e.Args[1], sc.columnType))
					}
				case "lag", "lead":
					// The value and the default take one type, as the
					// arguments of COALESCE do.
					c.x.noteType(e, commonType(branchArgs(e), sc.columnType, nil))
				}
			case *sqlir.Collate:
				// Postgres fails a collation that does not exist when it
				// parses the statement (42704).
				if _, ok := c.x.tx.db.kind.NamedCollation(e.Name); !ok && !c.x.tx.db.kind.InnoDB() {
					err = c.x.unsupported(fmt.Sprintf("collation %q, which postgres.Collations does not declare", e.Name))
					return
				}
				c.x.collates = true
			case *sqlir.Cast:
				c.castInput(e)
				c.timeParams(e.Type, e.X)
				if p, ok := e.X.(*sqlir.Param); ok {
					// A cast of a parameter types it as well, so one cast
					// to two types is refused, as Postgres reads it by one
					// and casts it to the other at each row.
					c.params.comparedWith(p.Index, c.paramTypeFamily(e.Type))
					c.numberParam(p, e.Type)
					c.typedParam(p)
					c.readParam(p)
				}
				if typeCollation(e.Type) != "" {
					// A cast to name brings name's C collation in, as a
					// COLLATE does.
					c.x.collates = true
				}
			case *sqlir.ColumnRef:
				if err = c.resolve(e, sc); err == nil {
					c.x.noteCollation(e, sc)
				}
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

// untypedOperands reads a string literal or parameter compared with an
// operand of a type the statement shows, as Postgres reads it when it plans
// or binds the statement. untypedPair reads it again for each row, but a
// statement that reaches no row, over an empty table or behind a
// short-circuited OR, must still fail with the input's error.
func (c *columnChecker) untypedOperands(sc *colScope, a, b sqlir.Expr) {
	if untypedExpr(a) == untypedExpr(b) {
		return
	}
	if untypedExpr(b) {
		a, b = b, a
	}
	typ := expressionType(b, sc.columnType)
	if p, ok := a.(*sqlir.Param); ok {
		c.params.comparedWith(p.Index, c.paramTypeFamily(typ))
		c.timeParams(typ, a)
		c.numberParam(p, typ)
		if c.paramTypeFamily(typ) != "" {
			c.typedParam(p)
		}
	}
	sample := c.x.tx.db.typeSample(typ)
	if sample == nil {
		return
	}
	c.untyped = append(c.untyped, func() error {
		v, ok := c.untypedValue(a)
		if !ok {
			return nil
		}
		_, err := c.x.untyped(a, v, sample)
		return err
	})
}

// inputAs reads a string literal or a parameter that takes the type typ
// from where it stands rather than from a comparison, such as from the
// column it is assigned to, or from the other branches of a CASE, a
// COALESCE, a GREATEST, a LEAST or a set operation. Postgres reads it with
// the type's input function when it plans or binds the statement, which is
// what a cast of it does too, while a write that reaches no row or a branch
// the run does not take never reads it.
//
// It is read as a cast to typ, rather than by typeSample as a comparison
// is, because a comparison has only a value to go by and so cannot check an
// integer's width, a date or a fraction written to an integer, and refuses
// text with a fraction that a float takes.
func (c *columnChecker) inputAs(e sqlir.Expr, typ string) {
	if typ == "" || typ == "unresolved column type" || !untypedExpr(e) {
		return
	}
	if p, ok := e.(*sqlir.Param); ok {
		// Postgres gives the parameter this type for the whole statement,
		// as it does where the parameter is compared.
		c.params.comparedWith(p.Index, c.paramTypeFamily(typ))
		c.numberParam(p, typ)
		c.typedParam(p)
		c.readParam(p)
	}
	c.castInput(&sqlir.Cast{X: e, Type: typ})
}

// numberParam records the number type typ the parameter p takes. Postgres
// gives p the type of the use it meets first, in an order that is not the
// statement's (UPDATE reads WHERE before SET), and converts it to the
// others' at each row, so p's input is read by one type only. One given two
// number types is refused rather than following that order, since reading
// it by the other's input could fail it where Postgres does not, as 40000
// for a smallint beside an integer.
func (c *columnChecker) numberParam(p *sqlir.Param, typ string) {
	if numberRank[typ] == 0 {
		return
	}
	if c.paramNumbers == nil {
		c.paramNumbers = map[int]string{}
	}
	if first, ok := c.paramNumbers[p.Index]; ok && numberRank[first] != numberRank[typ] {
		c.numberConflict = fmt.Sprintf("parameter $%d typed as both %s and %s", p.Index+1, first, typ)
	} else if !ok {
		c.paramNumbers[p.Index] = typ
	}
}

func (c *columnChecker) typedParam(p *sqlir.Param) {
	if c.paramTyped == nil {
		c.paramTyped = map[*sqlir.Param]bool{}
	}
	c.paramTyped[p] = true
}

func (c *columnChecker) readParam(p *sqlir.Param) {
	if c.paramRead == nil {
		c.paramRead = map[int]bool{}
	}
	c.paramRead[p.Index] = true
}

// untrackedParam names a parameter whose input is read by a cast to a type
// before the run while another of its uses, such as an operand of
// arithmetic or a function's argument, types it in a way the checks do not
// follow. Postgres gives the parameter one type, which may be that use's,
// and the read would then fail input Postgres takes, so the statement is
// refused rather than each such context modeled.
func (c *columnChecker) untrackedParam() string {
	var idx []int
	for p := range c.paramAt {
		if !c.paramTyped[p] && c.paramRead[p.Index] {
			idx = append(idx, p.Index)
		}
	}
	if len(idx) == 0 {
		return ""
	}
	return fmt.Sprintf("parameter $%d read as a type beside a use whose type detest does not follow", slices.Min(idx)+1)
}

// untypedValue is the text of a string literal, or the value bound to a
// parameter, false for a parameter the statement is not given, which the
// run reports.
func (c *columnChecker) untypedValue(e sqlir.Expr) (any, bool) {
	switch e := e.(type) {
	case *sqlir.Const:
		return e.Value, true
	case *sqlir.Param:
		if e.Index < 0 || e.Index >= len(c.x.args) {
			return nil, false
		}
		return paramText(e, c.x.args[e.Index]), true
	}
	return nil, false
}

// castInput reads a string literal or a parameter cast to a type before
// the statement runs, as Postgres converts a literal when it plans the
// statement and a parameter when it binds it, so that 'bad'::uuid fails
// over an empty table too. The cast is evaluated as the run evaluates it,
// and its errors are raised here, the server's and the refusals of a form
// detest cannot run alike, as a refused cast would run with other semantics
// whether or not a row reaches it. An expression detest does not evaluate
// is left to the run, which refuses it where a row reaches it.
func (c *columnChecker) castInput(e *sqlir.Cast) {
	if !untypedExpr(e.X) {
		return
	}
	c.untyped = append(c.untyped, func() error {
		v, ok := c.untypedValue(e.X)
		if !ok {
			return nil
		}
		rank := numberRank[c.x.tx.db.baseType(e.Type)]
		if f, isFloat := derefValue(v).(float64); isFloat && rank > 0 && rank <= numberRank["int8"] && f != math.Trunc(f) {
			// A cast rounds the float, where the driver sends its digits
			// as integer input, which Postgres fails, with a message that
			// depends on the driver.
			return c.x.unsupported(fmt.Sprintf("a float with a fraction bound to a parameter taken as %s", e.Type))
		}
		if rank > 0 && isOtherNumberText(v) {
			// Postgres reads 0x, 0o and 0b prefixes and underscores in
			// integers, and fails them elsewhere; detest models neither,
			// and the run refuses such text where a row reaches it.
			return c.x.unsupported(fmt.Sprintf("number text in a form detest does not model taken as %s", e.Type))
		}
		if c.x.tx.db.baseType(e.Type) == "uuid" && isParam(e.X) && derefValue(v) != nil && !isText(v) {
			// The cast leaves a value of another type to the run, where the
			// driver refuses it or sends text Postgres fails as uuid input.
			return c.x.unsupported(fmt.Sprintf("a %T bound to a parameter taken as a uuid", derefValue(v)))
		}
		if base := c.x.tx.db.baseType(e.Type); (base == "timestamp" || base == "timestamptz") && isParam(e.X) && derefValue(v) != nil && !isText(v) {
			switch derefValue(v).(type) {
			case time.Time, instant:
			default:
				// castValue leaves such a value as it is, where the driver
				// refuses it or sends text Postgres fails as a timestamp.
				return c.x.unsupported(fmt.Sprintf("a %T bound to a parameter taken as %s", derefValue(v), e.Type))
			}
		}
		if _, isBool := derefValue(v).(bool); isBool && rank > 0 && isParam(e.X) {
			// A cast makes a boolean 1 or 0, where the driver sends it as
			// boolean text or refuses it, neither of which a number reads.
			return c.x.unsupported(fmt.Sprintf("a boolean bound to a parameter taken as %s", e.Type))
		}
		if _, err := c.x.eval(e, nil); err != nil {
			if u, unknown := errors.AsType[errUnknownExpr](err); !unknown {
				return err
			} else if isParam(e.X) && derefValue(v) != nil && !isText(v) {
				// A conversion the cast does not model leaves a value the
				// driver encodes in its own way, which the run refuses
				// where a row reaches it. Text, such as a date in a format
				// detest does not parse, stays unchecked as before.
				return c.x.unsupported(u.what)
			}
		}
		return nil
	})
}

// timeParams records the date or timestamp type, typ, that Postgres gives
// the parameters among exprs, for a parameter to be read as the driver sends
// a time for it (wallClock). Any other type leaves them as they are.
//
// Postgres types a parameter where it first meets it in the statement. A
// parameter given two date and time types, by casts or by the operands it is
// compared with, is refused instead of following that order; timestamptz,
// which keeps the time as bound, is recorded for that alone.
func (c *columnChecker) timeParams(typ string, exprs ...sqlir.Expr) {
	if typ != "date" && typ != "timestamp" && typ != "timestamptz" {
		return
	}
	for _, e := range exprs {
		p, ok := e.(*sqlir.Param)
		if !ok {
			continue
		}
		if c.x.paramTimeTypes == nil {
			c.x.paramTimeTypes = map[int]string{}
		}
		if first, ok := c.x.paramTimeTypes[p.Index]; ok && first != typ {
			c.timeConflict = fmt.Sprintf("parameter $%d typed as both %s and %s", p.Index+1, first, typ)
		} else if !ok {
			c.x.paramTimeTypes[p.Index] = typ
		}
	}
}

// assignedParams types the parameters written to table's columns, cols in
// order or the table's own without a column list, as Postgres types them
// by the column: the driver then sends a time for a date or a timestamp
// column as its own clock, and for a timestamptz column as the instant.
func (c *columnChecker) assignedParams(table string, cols []string, exprs []sqlir.Expr) {
	def := c.x.tx.db.defs[c.x.tx.db.resolve(table)]
	if def == nil {
		return
	}
	if len(cols) == 0 {
		cols = def.columns
	}
	for i, e := range exprs {
		if i >= len(cols) {
			continue
		}
		typ := def.types[cols[i]]
		c.timeParams(typ, e)
		if p, ok := e.(*sqlir.Param); ok {
			// A parameter written to a column takes its type for the
			// statement, in VALUES too, whose input checkTypes reads.
			c.params.comparedWith(p.Index, c.paramTypeFamily(typ))
			c.numberParam(p, typ)
			if c.paramTypeFamily(typ) != "" {
				c.typedParam(p)
			}
		}
	}
}

// assignedInput reads the string literals and parameters written to
// table's columns, cols in order or the table's own without a column list,
// as the columns' input. It is for the writes a row may never reach, as
// UPDATE's SET, INSERT ... SELECT and ON CONFLICT DO UPDATE are, and not
// for VALUES, whose rows are each written and checked by checkTypes.
func (c *columnChecker) assignedInput(table string, cols []string, exprs []sqlir.Expr) {
	def := c.x.tx.db.defs[c.x.tx.db.resolve(table)]
	if def == nil {
		return
	}
	if len(cols) == 0 {
		cols = def.columns
	}
	for i, e := range exprs {
		if i < len(cols) {
			c.inputAs(e, def.types[cols[i]])
		}
	}
}

// typeSample is a value of type typ, for untyped to read text as typ, or nil
// for a type whose input detest does not read up front.
func (db *DB) typeSample(typ string) any {
	if t := db.enumOf(typ); t != nil {
		return enumValue{typ: t}
	}
	return typeSample(typ)
}

func typeSample(typ string) any {
	switch typ {
	case "int2", "int4", "int8", "int", "integer", "smallint", "bigint":
		return int64(0)
	case "float4", "float8", "real", "double precision", "numeric", "decimal":
		return float64(0)
	case "bool", "boolean":
		return false
	case "uuid":
		return uuidValue("")
	case "interval":
		return pgInterval{}
	}
	return nil
}

// operandTypes refuses a comparison of two typed operands Postgres has no
// operator for, which untypedPair refuses only when it reaches a row.
func (c *columnChecker) operandTypes(sc *colScope, a, b sqlir.Expr) error {
	if untypedExpr(a) || untypedExpr(b) {
		return nil
	}
	fa, fb := valueFamily(expressionType(a, sc.columnType)), valueFamily(expressionType(b, sc.columnType))
	if fa == "" || fb == "" || fa == fb {
		return nil
	}
	return c.x.unsupported(fmt.Sprintf("a comparison of a %s with a %s", fa, fb))
}

// branchTypes refuses branches of CASE, arguments of COALESCE, GREATEST,
// LEAST or NULLIF, or rows of a VALUES column, of two types Postgres does not resolve to one,
// which it fails when it plans the statement (42804) and the run would
// return one of. Two types are taken as one only where valueFamily groups
// them, or as dates and timestamps, which commonType widens; another pair
// is refused even where Postgres resolves it, as for a set operation.
func (c *columnChecker) branchTypes(exprs []sqlir.Expr, sc *colScope, of string) error {
	family := func(typ string) string {
		if timeRank[typ] > 0 {
			return "time"
		}
		return valueFamily(typ)
	}
	first := ""
	for _, e := range exprs {
		typ := expressionTypeIn(e, sc.columnType, func(q *sqlir.SubQuery) string { return c.subqueryType(q, sc) })
		if typ == "" && nestedBranches(e) != nil && untypedBranch(e) {
			// Postgres resolves a nested CASE or COALESCE of untyped
			// branches alone, at any depth, to text before the branches
			// around it.
			typ = "text"
		}
		switch {
		case typ == "" || typ == "unresolved column type":
		case first == "":
			first = typ
		case typ != first && (family(typ) == "" || family(typ) != family(first)):
			return c.x.unsupported(fmt.Sprintf("%s of a %s and a %s, which Postgres does not resolve to one type", of, first, typ))
		}
	}
	return nil
}

// nestedBranches are the branches of e when it is a CASE, or the arguments
// when it is a COALESCE, a GREATEST, a LEAST or a NULLIF, nil otherwise.
func nestedBranches(e sqlir.Expr) []sqlir.Expr {
	switch e := e.(type) {
	case *sqlir.CaseExpr:
		return caseBranches(e)
	case *sqlir.FuncCall:
		switch e.Name {
		case "coalesce", "greatest", "least", "nullif":
			return e.Args
		}
	}
	return nil
}

// valueFamily groups the types Postgres compares or resolves to one
// another, "" for a type detest does not group.
func valueFamily(typ string) string {
	switch f := paramFamily(typ); f {
	case "integer", "float":
		return "number"
	case "":
		if typ == "timestamp" || typ == "timestamptz" {
			return "time"
		}
	default:
		return f
	}
	return ""
}

// target is the scope of the table a write targets.
func (c *columnChecker) target(table, alias string) (*colScope, colSet) {
	cols := c.tableColumns(table)
	sc := &colScope{items: map[string]colSet{targetAlias(table, alias): cols}, types: map[string]map[string]string{}}
	if def := c.x.tx.db.defs[c.x.tx.db.resolve(table)]; def != nil {
		sc.types[targetAlias(table, alias)] = def.types
		sc.noteCollations(targetAlias(table, alias), def)
	}
	return sc, cols
}

// noteCollations records the collations the columns of the base table def,
// in scope as alias, declare.
func (sc *colScope) noteCollations(alias string, def *tableDef) {
	if sc.colls == nil {
		sc.colls = map[string]map[string]string{}
	}
	sc.colls[alias] = def.collations
}

// columnCollation is the collation the column r refers to declares, empty
// for the database's, and false when r is the column of a derived item,
// whose collation detest does not know.
func (sc *colScope) columnCollation(r *sqlir.ColumnRef) (string, bool) {
	for s := sc; s != nil; s = s.outer {
		for alias, cols := range s.items {
			if r.Table != "" && alias != r.Table {
				continue
			}
			if has, _ := cols.has(r.Column); has {
				colls, ok := s.colls[alias]
				return colls[r.Column], ok
			}
		}
	}
	return "", false
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
		c.assignedParams(ins.Table, ins.Columns, row)
	}
	if ins.Select != nil {
		if _, err := c.query(ins.Select, nil); err != nil {
			return err
		}
		if err := c.insertWidth(ins); err != nil {
			return err
		}
		if err := c.setOpInsert(ins); err != nil {
			return err
		}
		if starQuery(ins.Select) && slices.ContainsFunc(ins.Select.Targets, func(t sqlir.Target) bool { return !t.Star && untypedExpr(t.Expr) }) {
			// The check does not place the targets beside a * by position,
			// which Postgres reads their input by the column of.
			return c.x.unsupported("INSERT ... SELECT of a * beside a string literal or a parameter")
		}
		if ins.Select.SetOp == "" && len(ins.Select.Values) == 0 && !slices.ContainsFunc(ins.Select.Targets, func(t sqlir.Target) bool { return t.Star }) {
			exprs := make([]sqlir.Expr, len(ins.Select.Targets))
			for i, t := range ins.Select.Targets {
				exprs[i] = t.Expr
			}
			c.assignedParams(ins.Table, ins.Columns, exprs)
			c.assignedInput(ins.Table, ins.Columns, exprs)
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
		c.assignedParams(ins.Table, assignedColumns(oc.Set), assignedValues(oc.Set))
		c.assignedInput(ins.Table, assignedColumns(oc.Set), assignedValues(oc.Set))
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

// setOpInsert refuses INSERT ... SELECT of a set operation with a column of
// untyped queries alone, which Postgres resolves to text and fails to
// assign to a column of another type (42804) when it plans the statement,
// and which the run would read as the column's input instead.
func (c *columnChecker) setOpInsert(ins *sqlir.InsertStmt) error {
	sel := ins.Select
	if sel.SetOp == "" {
		return nil
	}
	def := c.x.tx.db.defs[c.x.tx.db.resolve(ins.Table)]
	if def == nil {
		return nil
	}
	cols := ins.Columns
	if len(cols) == 0 {
		cols = def.columns
	}
	types := c.setOpTypes(sel)
	for i, col := range cols {
		exprs := setOpColumnExprs(sel, i)
		if i < len(types) && types[i] != "" || exprs == nil || resolvedType("", exprs) != "text" {
			continue
		}
		if typ := def.types[col]; typ != "" && c.paramTypeFamily(typ) != "text" {
			return c.x.unsupported(fmt.Sprintf("INSERT ... SELECT of a set operation of untyped values into the %s column %q", typ, col))
		}
	}
	return nil
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
	c.assignedParams(up.Table, assignedColumns(up.Set), assignedValues(up.Set))
	c.assignedInput(up.Table, assignedColumns(up.Set), assignedValues(up.Set))
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
		if colls, ok := own.colls[alias]; ok {
			if sc.colls == nil {
				sc.colls = map[string]map[string]string{}
			}
			sc.colls[alias] = colls
		}
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
