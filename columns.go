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
	// ordered are the types of a derived table's, a CTE's or a view's
	// columns by position, which a * over the item gives in that order, nil
	// where the check does not know each one.
	ordered map[string][]string
	// certain are the types of the columns a statement shows without a
	// guess, by item: a table's declared ones and a derived table's
	// certainItem, which the output goes by.
	certain map[string]map[string]string
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
	// query is the query whose scope this is, nil for a write's target
	// and the other scopes of a statement rather than a query.
	query *sqlir.SelectStmt
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
	// cteOrdered are the CTEs' orderedTypes, as ctes holds their names.
	cteOrdered []map[string][]string
	// outTypes are the types of a query's output columns by position, ""
	// where expressionType does not know one, for a FROM item, a CTE, a
	// view or a scalar subquery made of it. A row is "record", which
	// expressionType never gives.
	outTypes map[*sqlir.SelectStmt][]string
	// outNamed are the types of a query's output columns by name, which a
	// select list with a * has where outTypes has none; "" for a name two
	// columns give or of a type expressionType does not know.
	outNamed map[*sqlir.SelectStmt]map[string]string
	// outText are the positions and the names of a query's output columns
	// that are a string literal, a parameter or a NULL, which Postgres
	// resolves to text where a query around reads the column. outTypes
	// leaves them untyped, as a set operation, VALUES or an INSERT reading
	// the query directly gives them the type of what stands beside them.
	outText      map[*sqlir.SelectStmt]map[int]bool
	outTextNamed map[*sqlir.SelectStmt]map[string]bool
	params       paramUses
	// untyped holds the reads of string literals and parameters, run after
	// the walk, as Postgres types the parameters before it binds them.
	untyped []func() error
	// timeConflict names a parameter given two date and time types.
	timeConflict string
	// textConflict names a parameter bound to other than text in an
	// output Postgres resolves to text.
	textConflict string
	// textParamAt are the uses of parameters a query output types text.
	textParamAt map[*sqlir.Param]bool
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
	// outUnresolved are the positions of a query's output columns whose
	// type the check does not know, which outTypes holds as "" alike with
	// an untyped column's.
	outUnresolved map[*sqlir.SelectStmt]map[int]bool
	// outWidth are the numbers of output columns of a query with a *, where
	// the items the * covers have columns of known names but perhaps of no
	// known type, as a derived table's.
	outWidth map[*sqlir.SelectStmt]int
	// valuesTypes are the column types of a VALUES query, resolved over
	// its rows in the scope the walk checked them in, which setOpTypes has
	// no other way to see for a correlated column.
	valuesTypes map[*sqlir.SelectStmt][]string
	// numberParams are the parameters among number branches.
	numberParams map[int]bool
	// queries are the queries being checked, innermost last, and
	// correlated those that read a column of a query around them.
	queries    []*sqlir.SelectStmt
	visited    []*sqlir.SelectStmt
	correlated map[*sqlir.SelectStmt]bool
	// uncertain are the queries that read around themselves only if a
	// column whose owner is not known belongs to a query around them.
	uncertain map[*sqlir.SelectStmt]bool
	// rowSubqueries are the subqueries a row is compared with, by that
	// row, whose width they must have rather than one.
	rowSubqueries map[*sqlir.SubQuery]*sqlir.RowExpr
	// inSubqueries are the subqueries of IN (SELECT ...), by the value or
	// the row compared with their columns.
	inSubqueries map[*sqlir.SelectStmt]sqlir.Expr
	// outCertain are the certain types of a query's output columns by
	// position, and outCertainNamed by name (noteCertain).
	outCertain      map[*sqlir.SelectStmt][]string
	outCertainNamed map[*sqlir.SelectStmt]map[string]string
	// paramCount is the number of uses of each parameter in the statement.
	paramCount map[int]int
	// outExpanded are the names of a query's columns with its * expanded
	// (noteExpanded).
	outExpanded map[*sqlir.SelectStmt][]string
}

func (x *sqlExec) checkColumns(stmt sqlir.Statement) error {
	// A script runs its statements on one executor, so the types of an
	// earlier statement's output must not reach a later one's.
	x.selectTypes, x.selectNamed, x.returningTypes, x.returningStar = nil, nil, nil, nil
	if what := nestedCall(stmt); what != "" {
		return x.unsupported(what)
	}
	if what := misplacedCall(stmt); what != "" {
		return x.unsupported(what)
	}
	if x.tx.db.kind.InnoDB() {
		return nil
	}
	if err := x.checkSequenceCalls(stmt); err != nil {
		return err
	}
	c := &columnChecker{x: x, paramCount: map[int]int{}, outCertain: map[*sqlir.SelectStmt][]string{}, outCertainNamed: map[*sqlir.SelectStmt]map[string]string{}}
	for _, p := range sqlir.Params(stmt) {
		c.paramCount[p.Index]++
	}
	var err error
	switch st := stmt.(type) {
	case *sqlir.SelectStmt:
		if _, err = c.query(st, nil); err == nil {
			first := st
			for first.SetOp != "" && first.Larg != nil {
				first = first.Larg
			}
			x.selectTypes = c.certainOutput(st)
			if st.SetOp == "" {
				x.selectNamed = c.outCertainNamed[first]
			}
		}
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
	if c.textConflict != "" {
		return x.unsupported(c.textConflict)
	}
	if len(c.textParamAt) > 0 {
		typed := map[int]bool{}
		for p := range c.textParamAt {
			typed[p.Index] = true
		}
		for _, p := range sqlir.Params(stmt) {
			if typed[p.Index] && !c.textParamAt[p] {
				// Each other use would have to be checked as text, as a
				// comparison, an operand of arithmetic or a function's
				// argument, which the checks do not all do.
				return x.unsupported(fmt.Sprintf("parameter $%d typed text by a query output and used elsewhere", p.Index+1))
			}
		}
	}
	if c.numberConflict != "" {
		return x.unsupported(c.numberConflict)
	}
	if what := c.untrackedParam(); what != "" {
		return x.unsupported(what)
	}
	if len(c.numberParams) > 0 {
		uses := map[int]int{}
		for _, p := range sqlir.Params(stmt) {
			uses[p.Index]++
		}
		for _, i := range slices.Sorted(maps.Keys(c.numberParams)) {
			if uses[i] > 1 {
				// Postgres binds the parameter as the number the branches
				// resolve to, which every other use then reads, as $1 / 2
				// or $1::text. detest converts it on the branch only.
				return x.unsupported(fmt.Sprintf("parameter $%d among number branches and used again", i+1))
			}
		}
	}
	for _, read := range c.untyped {
		if err := read(); err != nil {
			return err
		}
	}
	for _, q := range c.visited {
		switch {
		case c.correlated[q]:
		case c.uncertain[q]:
			if x.uncertain == nil {
				x.uncertain = map[*sqlir.SelectStmt]bool{}
			}
			x.uncertain[q] = true
		default:
			if x.uncorrelated == nil {
				x.uncorrelated = map[*sqlir.SelectStmt]bool{}
			}
			x.uncorrelated[q] = true
		}
	}
	return nil
}

// checkDefault runs the column check over a column default, once per
// statement, for the types the run converts its values by. A default reads
// no column and holds no subquery or parameter, so only the walk over its
// expression and the reads of its string literals apply.
func (x *sqlExec) checkDefault(d sqlir.Expr) error {
	return x.checkKept(d, func() *colScope { return &colScope{items: map[string]colSet{}} })
}

// checkTableExpr runs the column check over a CHECK constraint or a
// generated column of table, once per statement, as checkDefault does over
// a default. The expression reads the row being written, so its scope is
// the table under the names the expression was written with, each with the
// type and the collation of the column it now refers to.
func (x *sqlExec) checkTableExpr(table string, t *tableCheck) error {
	return x.checkKept(t.Expr, func() *colScope {
		def := x.tx.db.defs[table]
		cols := namesSet(def.columns)
		types := maps.Clone(def.types)
		colls := maps.Clone(def.collations)
		for written, now := range t.alias {
			cols[written] = true
			if types == nil {
				types = map[string]string{}
			}
			types[written] = def.types[now]
			if colls == nil {
				colls = map[string]string{}
			}
			colls[written] = def.collations[now]
		}
		alias := relname(table)
		sc := &colScope{items: map[string]colSet{alias: cols}, types: map[string]map[string]string{alias: types}}
		sc.noteCertain(alias, types)
		sc.colls = map[string]map[string]string{alias: colls}
		return sc
	})
}

// forgetKept drops what the column check noted over the expressions tables
// keep, for a script's next statement, whose DDL before it may have changed
// the types of the columns they read. Each noted expression outlives the
// statement in the schema, unlike a statement's own, so a note the check
// would not make again, such as a division by a column altered from numeric
// to integer, would otherwise stay.
func (x *sqlExec) forgetKept() {
	for e := range x.checkedDefaults {
		for _, n := range sqlir.Exprs(e) {
			if b, ok := n.(*sqlir.BinaryExpr); ok {
				delete(x.numericDivs, b)
			}
			delete(x.exprTypes, n)
			delete(x.branchTypes, n)
		}
	}
	x.checkedDefaults = nil
}

// checkKept runs the column check over an expression a table keeps, in the
// scope scope builds, and keeps what it returned for the statement.
func (x *sqlExec) checkKept(e sqlir.Expr, scope func() *colScope) error {
	if x.tx.db.kind.InnoDB() {
		return nil
	}
	if err, ok := x.checkedDefaults[e]; ok {
		return err
	}
	c := &columnChecker{x: x, paramCount: map[int]int{}, outCertain: map[*sqlir.SelectStmt][]string{}, outCertainNamed: map[*sqlir.SelectStmt]map[string]string{}}
	err := c.exprs(e, scope())
	for _, read := range c.untyped {
		if err != nil {
			break
		}
		err = read()
	}
	if x.checkedDefaults == nil {
		x.checkedDefaults = map[sqlir.Expr]error{}
	}
	x.checkedDefaults[e] = err
	return err
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
		if t.Func.Name == "generate_series" {
			typed := false
			for _, a := range t.Func.Args {
				typed = typed || !isNullConst(a) && !untypedExpr(a)
				if untypedBranch(a) && !untypedExpr(a) {
					// A CASE or COALESCE of literals only is text.
					return c.x.unsupported("generate_series over a text")
				}
				if _, ok := c.x.untypedText(a); ok {
					// Postgres reads it as the integer bound before the
					// call, which functionRows skips on a NULL bound.
					return c.x.unsupported("generate_series over a string literal or parameter")
				}
				if p, ok := a.(*sqlir.Param); ok && p.Index >= 0 && p.Index < len(c.x.args) {
					if v := derefValue(c.x.args[p.Index]); v != nil {
						if _, isInt := integer(v); !isInt {
							return c.x.unsupported(fmt.Sprintf("generate_series over a %T parameter", v))
						}
					}
					// The integer overload types it, int8 beside a bigint
					// bound and int4 otherwise, and reads its value as that.
					c.inputAs(p, seriesOverload(t.Func, c.typer(sc)))
				}
			}
			if !typed {
				// Postgres fails the call as ambiguous (42725).
				return c.x.unsupported("generate_series of no typed argument")
			}
			for _, a := range t.Func.Args {
				// functionRows runs the integer overload only, and would
				// read whole numeric bounds as integers.
				typ := c.typer(sc).reduce(c.typer(sc).of(a))
				if r := numberRank[typ]; typ != "" && (r == 0 || r > numberRank["int8"]) {
					return c.x.unsupported("generate_series over a " + typ)
				}
				if _, isParam := a.(*sqlir.Param); typ == "" && !isParam && !isNullConst(a) && !untypedExpr(a) && !c.x.tx.checking {
					// The overload Postgres picks depends on the type the
					// check cannot show, as abs(NULL)'s float8.
					return c.x.unsupported("generate_series over a bound of a type detest does not know")
				}
			}
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
		if name := funcColumn(t, alias); name != "" {
			cols = namesSet([]string{name})
		}
	default:
		if alias == "" {
			alias = relname(t.Name)
		}
		if _, isCTE := c.cteTypesOf(t.Name); !isCTE {
			// Postgres resolves every relation when it analyzes the
			// statement, so a missing one fails it even in a subquery no
			// row reaches, which tableRows alone would never open.
			if err := c.x.tx.db.checkTable(c.x.tx.db.resolve(t.Name)); err != nil {
				return err
			}
		}
		cols = c.tableColumns(t.Name)
	}
	if len(t.Columns) > 0 && t.Func == nil {
		// AS alias(a, b) renames the first columns, which needs them in
		// order.
		names := c.expandedNames(t.Sub)
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
	// A derived item's columns have the types of its query's select list,
	// a *'s and a set operation's included, and a function's the type of
	// its values, where the check knows them, and an unresolved type
	// otherwise, so the checks that need a type refuse rather than guess.
	switch {
	case t.Func != nil:
		sc.types[alias] = map[string]string{}
		if name := funcColumn(t, alias); name != "" {
			if typ := seriesType(t.Func, c.typer(sc)); typ != "" {
				sc.types[alias][name] = typ
			}
		}
	case t.Sub != nil:
		sc.types[alias] = c.derivedTypes(t.Sub, t.Columns)
		sc.noteOrdered(alias, c.orderedTypes(t.Sub))
		sc.noteCertain(alias, c.certainItem(t.Sub, t.Columns))
	default:
		db := c.x.tx.db
		if types, ok := c.cteTypesOf(t.Name); ok {
			// A CTE shadows a table of the same name.
			if types == nil || len(t.Columns) > 0 {
				types = map[string]string{}
			}
			sc.types[alias] = types
			sc.noteOrdered(alias, c.cteOrderedOf(t.Name))
		} else if v := db.views[db.resolve(t.Name)]; v != nil {
			types, ordered, err := c.viewTypes(v)
			if err != nil {
				return err
			}
			if len(t.Columns) > 0 {
				types = map[string]string{}
			}
			sc.types[alias] = types
			sc.noteOrdered(alias, ordered)
		} else if def := db.defs[db.resolve(t.Name)]; def != nil && len(t.Columns) == 0 {
			sc.types[alias] = def.types
			sc.noteCertain(alias, def.types)
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
	defs := map[string]*sqlir.SelectStmt{}
	for _, cte := range with {
		defs[cte.Name] = cte.Select
	}
	for _, cte := range with {
		// Through the views and the earlier CTEs it reads too, whose
		// effects run as far as it is read.
		if !c.x.effectfulQuery(cte.Select, defs, map[string]bool{}) {
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
// item reads in a write's FROM or USING, or returns "", as partlyRunItem
// does for the CTE's query.
func (c *columnChecker) partlyRun(with []sqlir.CTE, item sqlir.TableRef, items []sqlir.TableRef, target, alias string, where sqlir.Expr) string {
	var sel *sqlir.SelectStmt
	for _, cte := range with {
		if cte.Name == item.Name {
			sel = cte.Select
		}
	}
	itemAlias := item.Alias
	if itemAlias == "" {
		itemAlias = item.Name
	}
	isCTE := func(name string) bool {
		return slices.ContainsFunc(with, func(cte sqlir.CTE) bool { return cte.Name == name })
	}
	return c.x.partlyRunItem(sel, isCTE, itemAlias, c.ctes[len(c.ctes)-1][item.Name], items, target, alias, where)
}

// partlyRunItem names why Postgres might not run all of sel, the query of
// a FROM or USING item with effects of a write known by itemAlias with the
// columns itemCols, or returns "". Postgres skips the rest of a join, the
// item included, when the other side has no rows, which side that is being
// its planner's choice. That cannot change what the item does when it
// reads only the target table and the WHERE only joins the two on their
// columns. The target then has rows whenever the item has, as both read
// the statement's snapshot, so the item runs in full under any plan. That
// is the shape a job queue claims a job with.
func (x *sqlExec) partlyRunItem(sel *sqlir.SelectStmt, isCTE func(string) bool, itemAlias string, itemCols colSet, items []sqlir.TableRef, target, alias string, where sqlir.Expr) string {
	if len(items) != 1 {
		return "joined with other FROM or USING items"
	}
	// Its rows must come from the target's rows alone, so that it has none
	// when the target has none. A table merely named in a branch, a nested
	// WITH or a subquery does not make that so, and an aggregate gives a row
	// over no rows.
	if f := sel.From; sel.With != nil || sel.SetOp != "" || sel.Values != nil || len(sel.Joins) > 0 ||
		f == nil || f.Sub != nil || f.Func != nil || x.tx.db.resolve(f.Name) != x.tx.db.resolve(target) || isCTE(f.Name) {
		return "reading other rows than the target's alone"
	}
	if len(sel.GroupBy) > 0 || sel.Having != nil || slices.ContainsFunc(sel.Targets, func(t sqlir.Target) bool { return hasAggregate(t.Expr) }) {
		return "aggregating the target's rows"
	}
	// LIMIT and OFFSET are evaluated before the first row is read.
	if hasEffects(sel.Limit) || hasEffects(sel.Offset) {
		return "with effects in its LIMIT or OFFSET"
	}
	targetCols := colSet{}
	for _, col := range x.tableCols(x.tx.db.resolve(target)) {
		targetCols[col] = true
	}
	side := func(e sqlir.Expr) string {
		ref, ok := e.(*sqlir.ColumnRef)
		if !ok {
			return ""
		}
		inTarget, _ := targetCols.has(ref.Column)
		inItem, _ := itemCols.has(ref.Column)
		switch {
		case ref.Table == alias || ref.Table == "" && inTarget && !inItem:
			return "target"
		case ref.Table == itemAlias || ref.Table == "" && inItem && !inTarget:
			return "item"
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
	m, tm, om := map[string]colSet{}, map[string]map[string]string{}, map[string][]string{}
	c.ctes = append(c.ctes, m)
	c.cteTypes = append(c.cteTypes, tm)
	c.cteOrdered = append(c.cteOrdered, om)
	pop = func() {
		c.ctes = c.ctes[:len(c.ctes)-1]
		c.cteTypes = c.cteTypes[:len(c.cteTypes)-1]
		c.cteOrdered = c.cteOrdered[:len(c.cteOrdered)-1]
	}
	for _, cte := range with {
		s, err := c.query(cte.Select, outer)
		if err != nil {
			pop()
			return nil, err
		}
		if outer != nil && (c.correlated[cte.Select] || c.uncertain[cte.Select]) {
			// Whether Postgres runs the subquery declaring such a CTE once
			// or for each row depends on whether it plans the CTE, which
			// it skips when nothing reads it and inlines or materializes
			// by its references, MATERIALIZED and volatility. detest does
			// not model that planning, so it refuses the CTE rather than
			// run the subquery either way on a guess.
			pop()
			return nil, c.x.unsupported(fmt.Sprintf("CTE %q reading a column of a query around the one declaring it", cte.Name))
		}
		m[cte.Name] = s
		tm[cte.Name] = c.derivedTypes(cte.Select, nil)
		om[cte.Name] = c.orderedTypes(cte.Select)
	}
	return pop, nil
}

func (c *columnChecker) query(sel *sqlir.SelectStmt, outer *colScope) (colSet, error) {
	if sel == nil {
		return nil, nil
	}
	if sel.Lock != nil {
		// Postgres refuses these when it analyzes the statement, which
		// planLocking refuses only where a run reaches the query.
		if isAggregate(sel) || sel.Distinct || len(sel.DistinctOn) > 0 || len(windowsOf(sel)) > 0 {
			return nil, c.x.unsupported("FOR UPDATE with GROUP BY, DISTINCT or window functions")
		}
	}
	c.queries = append(c.queries, sel)
	c.visited = append(c.visited, sel)
	defer func() { c.queries = c.queries[:len(c.queries)-1] }()
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
		sc := &colScope{items: map[string]colSet{}, outputs: out, anyOutput: out == nil, outer: outer, query: sel}
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
	sc := &colScope{items: map[string]colSet{}, outer: outer, query: sel}
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
			types[i] = c.typer(sc).common(col)
			c.timeParams(types[i], col...)
			for _, e := range col {
				c.inputAs(e, types[i])
			}
		}
		if c.valuesTypes == nil {
			c.valuesTypes = map[*sqlir.SelectStmt][]string{}
		}
		c.valuesTypes[sel] = types
		c.noteValuesCertain(sel, sc)
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
		if err := c.condition(j.On, sc, "JOIN/ON"); err != nil {
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
				if _, local := sc.items[t.Table]; !local {
					// The run expands a * over this query's items only, so
					// an outer query's would give no columns.
					return nil, c.x.unsupported(fmt.Sprintf("%s.* of an outer query's item", t.Table))
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
	if err := c.condition(sel.Where, sc, "WHERE"); err != nil {
		return nil, err
	}
	if err := c.exprs(sel.Having, sc); err != nil {
		return nil, err
	}
	if err := c.condition(sel.Having, sc, "HAVING"); err != nil {
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
	c.noteCertain(sel, sc)
	c.noteExpanded(sel)
	if out := outputColumns(sel); out != nil {
		return out, nil
	}
	return starColumns(sel, sc), nil
}

// starColumns are the columns of a select list with a *, which gives the
// columns of the items it covers, nil when one of those is not known.
func starColumns(sel *sqlir.SelectStmt, sc *colScope) colSet {
	out := colSet{}
	add := func(name string, once bool) {
		_, seen := out[name]
		out[name] = once && !seen
	}
	for _, t := range sel.Targets {
		if !t.Star {
			add(targetName(t), true)
			continue
		}
		for alias, cols := range sc.items {
			if t.Table != "" && alias != t.Table {
				continue
			}
			if cols == nil {
				return nil
			}
			for name, once := range cols {
				add(name, once)
			}
		}
	}
	return out
}

// starItemTypes are the types of the columns the * t of sel gives, in
// order, when it covers one derived table, CTE or view whose orderedTypes
// the scope holds, and nil otherwise.
func starItemTypes(sel *sqlir.SelectStmt, t sqlir.Target, sc *colScope) []string {
	alias := t.Table
	if alias == "" {
		from := sel.From
		if from == nil || len(sel.Joins) > 0 || from.Func != nil {
			return nil
		}
		alias = targetAlias(from.Name, from.Alias)
	}
	if _, local := sc.items[alias]; !local {
		return nil
	}
	return sc.ordered[alias]
}

// noteOrdered records the orderedTypes of the item alias, if any.
func (sc *colScope) noteOrdered(alias string, types []string) {
	if types == nil {
		return
	}
	if sc.ordered == nil {
		sc.ordered = map[string][]string{}
	}
	sc.ordered[alias] = types
}

// noteOutputTypes records the types of a query's select list items that
// expressionType knows, by position, and by name. A *'s are recorded by
// position only where starColumns or starItemTypes knows the order of the
// columns it gives, and by name otherwise. A set operation's are its
// queries', which agreedTypes reads.
func (c *columnChecker) noteOutputTypes(sel *sqlir.SelectStmt, sc *colScope) {
	if slices.ContainsFunc(sel.Targets, func(t sqlir.Target) bool { return t.Star }) {
		if w, ok := starWidth(sel, sc); ok {
			if c.outWidth == nil {
				c.outWidth = map[*sqlir.SelectStmt]int{}
			}
			c.outWidth[sel] = w
		}
	}
	var types []string
	named := map[string]string{}
	seen := map[string]bool{}
	textAt, textNamed := map[int]bool{}, map[string]bool{}
	note := func(name, typ string) {
		if seen[name] {
			typ = ""
			delete(textNamed, name)
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
			} else if cols := starItemTypes(sel, t, sc); cols != nil {
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
		typ := c.typer(sc).of(t.Expr)
		if typ == "unresolved column type" {
			typ = ""
			if c.outUnresolved == nil {
				c.outUnresolved = map[*sqlir.SelectStmt]map[int]bool{}
			}
			if c.outUnresolved[sel] == nil {
				c.outUnresolved[sel] = map[int]bool{}
			}
			c.outUnresolved[sel][len(types)] = true
		}
		if typ == "" && rowValued(t.Expr) {
			// Only here and not in typer, which leaves a row untyped for
			// the row comparisons and the run's branch checks to handle
			// as they do. A query around reads the column as the record
			// Postgres gives it, which no operator or function the checks
			// know takes.
			typ = "record"
		}
		if typ == "" && !untypedExpr(t.Expr) && !isNullConst(t.Expr) {
			// Postgres resolves a CASE, a COALESCE or an aggregate of
			// untyped values alone, such as max('1'), to text.
			if params, ok := untypedLeaves(t.Expr); ok {
				typ = "text"
				c.textParams(params, bytesUnread(t.Expr, false))
			}
		}
		textOut := typ == "" && (untypedExpr(t.Expr) || isNullConst(t.Expr))
		if textOut {
			textAt[len(types)] = true
		}
		types = append(types, typ)
		if textOut && !seen[targetName(t)] {
			textNamed[targetName(t)] = true
		}
		note(targetName(t), typ)
	}
	if c.outText == nil {
		c.outText = map[*sqlir.SelectStmt]map[int]bool{}
		c.outTextNamed = map[*sqlir.SelectStmt]map[string]bool{}
	}
	c.outText[sel], c.outTextNamed[sel] = textAt, textNamed
	if c.outTypes == nil {
		c.outTypes = map[*sqlir.SelectStmt][]string{}
		c.outNamed = map[*sqlir.SelectStmt]map[string]string{}
	}
	if !star || starKnown {
		c.outTypes[sel] = types
	}
	c.outNamed[sel] = named
}

// unknownPolymorphic names a window function that takes anyelement given a
// string literal, a parameter or a NULL alone for it, of which Postgres
// cannot determine the type (42804), and "" otherwise. LAG and LEAD with a
// default take anycompatible, which resolves such values to text.
func unknownPolymorphic(f *sqlir.FuncCall) string {
	switch f.Name {
	case "first_value", "last_value", "nth_value":
	case "lag", "lead":
		if len(f.Args) > 2 {
			return ""
		}
	default:
		return ""
	}
	if len(f.Args) == 0 || !untypedExpr(f.Args[0]) && !isNullConst(f.Args[0]) {
		return ""
	}
	return f.Name + " of a string literal, a parameter or NULL, whose type Postgres cannot determine"
}

// untypedLeaves are the parameters of e when e is made of string literals,
// parameters and NULLs alone, through the CASE branches and the arguments
// of the functions Postgres resolves to one type, which it resolves to
// text, and false when e is not. An aggregate or a window function inside
// another is not, as Postgres does not nest them (42803).
func untypedLeaves(e sqlir.Expr) ([]*sqlir.Param, bool) {
	return untypedLeavesIn(e, false)
}

func untypedLeavesIn(e sqlir.Expr, inAggregate bool) ([]*sqlir.Param, bool) {
	var args []sqlir.Expr
	switch e := e.(type) {
	case *sqlir.Param:
		return []*sqlir.Param{e}, true
	case *sqlir.Const:
		_, isText := e.Value.(string)
		return nil, isText || e.Value == nil
	case *sqlir.WindowFunc:
		if e.Func == nil || inAggregate {
			return nil, false
		}
		return untypedLeavesIn(e.Func, false)
	case *sqlir.CaseExpr:
		args = caseBranches(e)
	case *sqlir.FuncCall:
		switch e.Name {
		case "min", "max", "first_value", "last_value", "lag", "lead":
			if inAggregate {
				return nil, false
			}
			inAggregate = true
		}
		switch e.Name {
		case "min", "max":
			args = e.Args
		case "first_value", "last_value":
			if len(e.Args) > 0 {
				args = e.Args[:1]
			}
		default:
			args = branchArgs(e)
		}
	}
	if len(args) == 0 {
		return nil, false
	}
	var out []*sqlir.Param
	for _, a := range args {
		ps, ok := untypedLeavesIn(a, inAggregate)
		if !ok {
			return nil, false
		}
		out = append(out, ps...)
	}
	return out, true
}

// bytesUnread are the parameters among e's untyped leaves that no CASE,
// COALESCE, GREATEST, LEAST or NULLIF takes directly, whose evaluators
// read a []byte as the text it holds; elsewhere the run keeps it as bytea.
func bytesUnread(e sqlir.Expr, read bool) []*sqlir.Param {
	var args []sqlir.Expr
	switch e := e.(type) {
	case *sqlir.Param:
		if read {
			return nil
		}
		return []*sqlir.Param{e}
	case *sqlir.WindowFunc:
		if e.Func != nil {
			return bytesUnread(e.Func, false)
		}
	case *sqlir.CaseExpr:
		args, read = caseBranches(e), true
	case *sqlir.FuncCall:
		args, read = e.Args, false
		switch e.Name {
		case "coalesce", "greatest", "least", "nullif":
			read = true
		}
	}
	var out []*sqlir.Param
	for _, a := range args {
		out = append(out, bytesUnread(a, read)...)
	}
	return out
}

// textParams records params as typed text, which Postgres resolves them to
// as the leaves of an output it resolves to text, and refuses one bound to
// another value, as the run reads the value as bound rather than as that
// text, and one of unread bound to bytes, which the run keeps as bytea.
func (c *columnChecker) textParams(params, unread []*sqlir.Param) {
	for _, p := range params {
		if c.textParamAt == nil {
			c.textParamAt = map[*sqlir.Param]bool{}
		}
		c.textParamAt[p] = true
		if p.Index >= 0 && p.Index < len(c.x.args) {
			switch v := derefValue(c.x.args[p.Index]).(type) {
			case nil, string:
			case []byte:
				if slices.Contains(unread, p) && c.textConflict == "" {
					c.textConflict = fmt.Sprintf("a %T parameter $%d in a query output Postgres resolves to text", v, p.Index+1)
				}
			default:
				if c.textConflict == "" {
					c.textConflict = fmt.Sprintf("a %T parameter $%d in a query output Postgres resolves to text", v, p.Index+1)
				}
			}
		}
	}
}

// readOutputs types as text the parameters among sel's outputs of outText,
// where a query around sel reads them.
func (c *columnChecker) readOutputs(sel *sqlir.SelectStmt) {
	if len(c.outText[sel]) == 0 {
		return
	}
	for _, t := range sel.Targets {
		if p, ok := t.Expr.(*sqlir.Param); ok && !t.Star {
			c.textParams([]*sqlir.Param{p}, []*sqlir.Param{p})
		}
	}
}

// readValues types as text the parameters in the columns of the VALUES
// query sel that Postgres resolves to a text type, as it does a column of
// string literals, parameters and NULLs alone, where a query around sel
// reads them as a FROM item or a CTE.
func (c *columnChecker) readValues(sel *sqlir.SelectStmt) {
	for i, typ := range c.itemTypes(sel) {
		if c.x.tx.db.branchKind(typ) != "text" {
			continue
		}
		for _, e := range setOpColumnExprs(sel, i) {
			if params, ok := untypedLeaves(e); ok {
				c.textParams(params, bytesUnread(e, false))
			}
		}
	}
}

// readTypes are outTypes of sel as a query around it reads them, with the
// columns of outText resolved to text.
func (c *columnChecker) readTypes(sel *sqlir.SelectStmt) ([]string, bool) {
	types, ok := c.outTypes[sel]
	if !ok || len(c.outText[sel]) == 0 {
		return types, ok
	}
	types = slices.Clone(types)
	for i := range c.outText[sel] {
		if i < len(types) {
			types[i] = "text"
		}
	}
	return types, true
}

// starTable is the table the * t of sel expands when its one FROM item is
// a table the schema declares, the order of whose columns the check knows,
// and nil otherwise.
func (c *columnChecker) starTable(sel *sqlir.SelectStmt, t sqlir.Target) *tableDef {
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
	return db.defs[db.resolve(from.Name)]
}

// expandedNames are the names of sel's columns in order, as outputNames
// gives them, with a * over a table starTable finds expanded to its
// columns, and nil where a * gives columns of no known order.
func (c *columnChecker) expandedNames(sel *sqlir.SelectStmt) []string {
	if names := outputNames(sel); names != nil {
		return names
	}
	for sel.SetOp != "" && sel.Larg != nil {
		sel = sel.Larg
	}
	// A query's own CTEs are out of scope once it is walked, where a name
	// may resolve to a table they shadow, so the walk records them.
	return c.outExpanded[sel]
}

// noteExpanded records expandedNames' names of sel while its CTEs are in
// scope, nil where a * gives columns of no known order.
func (c *columnChecker) noteExpanded(sel *sqlir.SelectStmt) {
	if c.outExpanded == nil {
		c.outExpanded = map[*sqlir.SelectStmt][]string{}
	}
	c.outExpanded[sel] = c.expandNames(sel)
}

func (c *columnChecker) expandNames(sel *sqlir.SelectStmt) []string {
	var out []string
	for _, t := range sel.Targets {
		if !t.Star {
			out = append(out, targetName(t))
			continue
		}
		def := c.starTable(sel, t)
		if def == nil {
			return nil
		}
		out = append(out, def.columns...)
	}
	return out
}

// starColumns are the types of the columns the * t of sel gives, in order,
// when its one FROM item is a table the schema declares, the order of whose
// columns the check knows, and nil otherwise.
func (c *columnChecker) starColumns(sel *sqlir.SelectStmt, t sqlir.Target) []string {
	def := c.starTable(sel, t)
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
		if (c.unresolvedColumn(sel.Larg, i) && untypedColumn(sel.Rarg, i)) || (c.unresolvedColumn(sel.Rarg, i) && untypedColumn(sel.Larg, i)) {
			// A column of a type the check does not know, as of a derived
			// table's * over a join, which Postgres reads the other query's
			// input by.
			return c.x.unsupported("a set operation of a column of unresolved type against a string literal or a parameter")
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

// unresolvedColumn reports whether the i-th column of q, a query of a set
// operation or one nested in it, is of a type the check does not know.
func (c *columnChecker) unresolvedColumn(q *sqlir.SelectStmt, i int) bool {
	switch {
	case q == nil:
		return false
	case q.SetOp != "":
		return c.unresolvedColumn(q.Larg, i) || c.unresolvedColumn(q.Rarg, i)
	}
	return c.outUnresolved[q][i]
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
	if t, _ := c.readTypes(sel); len(t) == 1 {
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
				out[i] = typer{subquery: c.recordedSubqueryType, base: c.x.tx.db.baseType}.common(col)
			}
			if out[i] == "" && slices.ContainsFunc(col, func(e sqlir.Expr) bool { return nestedBranches(e) != nil && untypedBranch(e) }) {
				// A CASE or COALESCE of untyped branches alone is text,
				// as it is in a select list.
				out[i] = "text"
			}
			if out[i] == "" && slices.ContainsFunc(col, rowValued) {
				out[i] = "record" // as noteOutputTypes types a row
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
			// they do in typer's common.
			out[i] = t
		}
	}
	return out
}

// derivedTypes are the types of the columns of a FROM item, a CTE or a view
// made of sel, under the names an AS alias(a, b) or a view's column list
// gives the first ones. A *'s columns are typed by name, which needs no
// renaming, and a set operation's by the types its queries agree on.
func (c *columnChecker) derivedTypes(sel *sqlir.SelectStmt, names []string) map[string]string {
	if sel.SetOp != "" {
		// pairType leaves a VALUES query or a * unresolved, but a column of
		// rows is a record whatever the other queries give.
		types, rows := c.agreedTypes(sel), c.setOpTypes(sel)
		if len(types) < len(rows) {
			types = append(slices.Clone(types), make([]string, len(rows)-len(types))...)
		}
		for i, typ := range rows {
			if typ == "record" && types[i] == "" {
				types[i] = "record"
			}
		}
		return c.namedTypes(sel, types, names)
	}
	if len(sel.Values) > 0 {
		c.readValues(sel)
		return c.namedTypes(sel, c.itemTypes(sel), names)
	}
	c.readOutputs(sel)
	star := slices.ContainsFunc(sel.Targets, func(t sqlir.Target) bool { return t.Star })
	if types, ok := c.readTypes(sel); ok && (!star || len(names) > 0) {
		// A * the check expands has its types by position too, which an
		// alias list renames.
		return c.namedTypes(sel, types, names)
	}
	out := map[string]string{}
	if len(names) > 0 {
		return out
	}
	for n, t := range c.outNamed[sel] {
		if t == "" && c.outTextNamed[sel][n] {
			t = "text"
		}
		if t != "" {
			out[n] = t
		}
	}
	return out
}

// orderedTypes are the types of the columns of a FROM item, a CTE or a view
// made of sel, by position, nil unless the check knows each one, as a *
// over the item gives them in that order. A type the check does not know
// leaves the whole list out rather than standing as "", which a reader
// would take for no type rather than for one that may be an interval.
func (c *columnChecker) orderedTypes(sel *sqlir.SelectStmt) []string {
	var types []string
	switch {
	case sel.SetOp != "":
		types = c.agreedTypes(sel)
	case len(sel.Values) > 0:
		types = c.itemTypes(sel)
	default:
		types, _ = c.readTypes(sel)
	}
	if len(types) == 0 || slices.Contains(types, "") || slices.Contains(types, "unresolved column type") {
		return nil
	}
	return types
}

// agreedTypes are the types of the columns of the set operation sel by
// position, "" where the check does not know one. Postgres resolves a set
// operation pair by pair, from the innermost, so each pair is resolved as
// it would be. setOpTypes takes the first type a query gives instead, which
// is not the type of int UNION numeric.
func (c *columnChecker) agreedTypes(sel *sqlir.SelectStmt) []string {
	// knownWidth counts the columns of a * the check expands, which
	// outputNames does not.
	n, ok := c.knownWidth(sel)
	if !ok {
		return nil
	}
	out := make([]string, n)
	for i := range out {
		typ, ok := c.pairType(sel, i, n)
		if !ok {
			return nil
		}
		if typ == untypedOperand || typ == nullOperand {
			typ = "text"
		}
		out[i] = typ
	}
	return out
}

// The operands of a set operation Postgres has not given a type yet.
const (
	nullOperand    = "\x00null"
	untypedOperand = "\x00untyped"
)

// pairType is the type of the i-th of the n columns of sel: a query's own,
// or nullOperand or untypedOperand for a NULL, or a string literal or a
// parameter, which take the type of the other side of the pair, and, for a
// pair, the type Postgres resolves the two sides to. A string literal or a
// parameter beside a type other than text is left unresolved, as detest
// keeps it as the text it is where Postgres converts it, and two types
// that differ are too. false when a query's types are not known by
// position, as for a * or VALUES.
func (c *columnChecker) pairType(sel *sqlir.SelectStmt, i, n int) (string, bool) {
	if sel.SetOp != "" && sel.Larg != nil && sel.Rarg != nil {
		l, ok := c.pairType(sel.Larg, i, n)
		if !ok {
			return "", false
		}
		r, ok := c.pairType(sel.Rarg, i, n)
		if !ok {
			return "", false
		}
		if l == nullOperand {
			l, r = r, l
		}
		switch {
		case r == nullOperand:
			if l == nullOperand || l == untypedOperand {
				// Postgres resolves the pair to text.
				return "text", true
			}
			return l, true
		case l == untypedOperand && r == untypedOperand:
			return "text", true
		case l == untypedOperand || r == untypedOperand:
			other := l
			if other == untypedOperand {
				other = r
			}
			if other == "text" {
				return "text", true
			}
			return "", true
		case l == r:
			return l, true
		case numberCategory(l) == 1 && numberCategory(r) == 1:
			// Integers meet at the widest. evalSetOp does not convert the
			// rows of a pair that meets at a numeric, a float or a time
			// type, so such a pair is left unresolved.
			if numberRank[r] > numberRank[l] {
				return r, true
			}
			return l, true
		}
		return "", true
	}
	types, ok := c.outTypes[sel]
	if !ok || len(types) != n || len(sel.Values) > 0 {
		return "", false
	}
	if slices.ContainsFunc(sel.Targets, func(t sqlir.Target) bool { return t.Star }) {
		// A * gives more columns than targets, of its items' own types.
		return types[i], true
	}
	e := sel.Targets[i].Expr
	if k, isConst := e.(*sqlir.Const); isConst && k.Value == nil {
		return nullOperand, true
	}
	if p, isParam := e.(*sqlir.Param); isParam && p.Index >= 0 && p.Index < len(c.x.args) {
		switch derefValue(c.x.args[p.Index]).(type) {
		case nil, string, []byte:
		default:
			// The driver sends it as text that Postgres reads as the
			// column's type, which evalSetOp does not reproduce, so the
			// column is left unresolved.
			return "", true
		}
	}
	if untypedExpr(e) {
		return untypedOperand, true
	}
	return types[i], true
}

// funcColumn is the name of the one column of the function in FROM t,
// whose alias is alias: the name its column list gives it, or the alias.
// "" for a function detest does not run, or one with an ordinality column.
func funcColumn(t sqlir.TableRef, alias string) string {
	if t.Func.Name != "generate_series" || t.Ordinality || len(t.Columns) > 1 {
		return ""
	}
	if len(t.Columns) == 1 {
		return t.Columns[0]
	}
	return alias
}

// seriesOverload is the integer type of the generate_series overload
// Postgres picks for f: int8 where a bound is a bigint, int4 otherwise.
func seriesOverload(f *sqlir.FuncCall, t typer) string {
	for _, a := range f.Args {
		switch t.reduce(t.of(a)) {
		case "int8", "bigint":
			return "int8"
		}
	}
	return "int4"
}

// seriesType is the type of the values generate_series gives over the
// integers f's arguments are: bigint where one is a bigint, as Postgres
// picks the bigint form, and "" where one is of a type the check does not
// know, as of a parameter.
func seriesType(f *sqlir.FuncCall, t typer) string {
	typ := "int4"
	for _, a := range f.Args {
		switch t.reduce(t.of(a)) {
		case "int", "int2", "int4", "integer", "smallint":
		case "int8", "bigint":
			typ = "int8"
		default:
			return ""
		}
	}
	return typ
}

// itemTypes are the output types of sel, a query read as a FROM item, by
// position: a set operation's or a VALUES query's as setOpTypes resolves
// them, text for a column of untyped queries alone, as Postgres resolves it
// at the item's output.
func (c *columnChecker) itemTypes(sel *sqlir.SelectStmt) []string {
	if sel.SetOp == "" && len(sel.Values) == 0 {
		return c.outTypes[sel]
	}
	types := slices.Clone(c.setOpTypes(sel))
	for i := range types {
		types[i] = resolvedType(types[i], setOpColumnExprs(sel, i))
	}
	return types
}

// namedTypes are the output types of sel by the names its columns have,
// under those an AS alias(a, b) or a view's column list gives the first
// ones. A name two columns have is left unresolved, as its type is either.
func (c *columnChecker) namedTypes(sel *sqlir.SelectStmt, types []string, names []string) map[string]string {
	out := map[string]string{}
	// Without types, as of VALUES, the item's columns are of unresolved
	// type rather than unknown.
	cols := c.expandedNames(sel)
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
		if c.unsentOutputParam(q.Select) {
			return "unresolved column type"
		}
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
	if e := q.Select.Targets[0].Expr; untypedOutput(e) {
		if p, ok := e.(*sqlir.Param); ok {
			c.textParams([]*sqlir.Param{p}, []*sqlir.Param{p})
		}
		if p, ok := e.(*sqlir.Param); ok && p.Index >= 0 && p.Index < len(c.x.args) {
			switch derefValue(c.x.args[p.Index]).(type) {
			case nil, string:
			default:
				// The subquery returns the value as bound, not the text
				// Postgres reads it as.
				return "unresolved column type"
			}
		}
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
		types, ok := c.readTypes(q.Select)
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
	vc := &columnChecker{x: c.x, ctes: c.ctes, cteTypes: c.cteTypes, cteOrdered: c.cteOrdered, subTypes: c.subTypes, paramCount: c.paramCount, outCertain: c.outCertain, outCertainNamed: c.outCertainNamed}
	if _, err := vc.query(q.Select, sc); err != nil {
		return ""
	}
	types, ok := vc.readTypes(q.Select)
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
	vc := &columnChecker{x: c.x, ctes: c.ctes, cteTypes: c.cteTypes, cteOrdered: c.cteOrdered, subTypes: c.subTypes, paramCount: c.paramCount, outCertain: c.outCertain, outCertainNamed: c.outCertainNamed}
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
// statement that reads the view, or the check's refusal of the query, as
// Postgres expands the view into the statement and runs the query as one of
// its own. The checks the statement makes after its walk apply too, apart
// from those of parameters, which a view's query cannot hold.
func (c *columnChecker) viewTypes(v *sqlir.SchemaChange) (map[string]string, []string, error) {
	vc := &columnChecker{x: c.x}
	_, err := vc.query(v.View, nil)
	switch {
	case err != nil:
	case vc.timeConflict != "":
		err = c.x.unsupported(vc.timeConflict)
	case vc.numberConflict != "":
		err = c.x.unsupported(vc.numberConflict)
	default:
		for _, read := range vc.untyped {
			if err = read(); err != nil {
				break
			}
		}
	}
	if err != nil {
		if _, ok := errors.AsType[*ErrUnsupportedSQL](err); ok {
			return nil, nil, err
		}
		// Any other failure is one Postgres makes when the view is created,
		// so it cannot be the answer to a statement reading the view.
		return map[string]string{}, nil, nil
	}
	return vc.derivedTypes(v.View, v.ViewColumns), vc.orderedTypes(v.View), nil
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

// cteOrderedOf are the orderedTypes of the CTE name, the innermost of that
// name.
func (c *columnChecker) cteOrderedOf(name string) []string {
	for i, m := range slices.Backward(c.ctes) {
		if _, ok := m[name]; ok {
			return c.cteOrdered[i][name]
		}
	}
	return nil
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
		if queryHasEffects(ins.Select) {
			// The run compares the width only once it has run the
			// query, which would spend a sequence value or take a lock
			// Postgres never takes, as it fails the width when it plans
			// the statement.
			return c.x.unsupported("INSERT ... SELECT of a * whose width detest does not know, from a query that changes state")
		}
		return nil
	}
	return c.insertWidthOf(ins, w)
}

// insertWidthOf is insertWidth for a source of w columns.
func (c *columnChecker) insertWidthOf(ins *sqlir.InsertStmt, w int) error {
	db := c.x.tx.db
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

// insertRows reads the input INSERT ... VALUES writes as its columns' types
// before the statement runs, as Postgres converts each row when it analyzes
// the statement, so that an invalid value in a later row fails before an
// earlier one is written. Its structural errors come first, as there: rows
// of unequal lengths, then rows of the wrong width. Literals are read row by
// row where the walk stands, and parameters once it ends, as they are bound.
func (c *columnChecker) insertRows(ins *sqlir.InsertStmt) error {
	db := c.x.tx.db
	if len(ins.Rows) == 0 || db.kind.InnoDB() {
		return nil
	}
	for _, row := range ins.Rows[1:] {
		if len(row) != len(ins.Rows[0]) {
			return db.kind.Error(sqlir.SyntaxError, "VALUES lists must all be the same length", relname(ins.Table), "", "")
		}
	}
	if err := c.insertWidthOf(ins, len(ins.Rows[0])); err != nil {
		return err
	}
	def := db.defs[db.resolve(ins.Table)]
	if def == nil {
		return nil
	}
	cols := ins.Columns
	if len(cols) == 0 {
		cols = def.columns
	}
	for _, row := range ins.Rows {
		for i, e := range row {
			if i >= len(cols) {
				break
			}
			typ := def.types[cols[i]]
			if _, ok := e.(*sqlir.Const); !ok || typ == "" || typ == "unresolved column type" {
				c.inputAs(e, typ)
				continue
			}
			if err := c.literalInput(&sqlir.Cast{X: e, Type: typ}); err != nil {
				return err
			}
		}
	}
	return nil
}

// knownWidth is the number of columns q gives, false for a * whose columns
// the walk did not count, as of an item of columns it does not know.
func (c *columnChecker) knownWidth(q *sqlir.SelectStmt) (int, bool) {
	switch {
	case q == nil:
		return 0, false
	case q.SetOp != "":
		// Both queries' widths, as a width taken from the left alone
		// would leave an unknown right one to the run, which runs it
		// before it compares them.
		l, lok := c.knownWidth(q.Larg)
		r, rok := c.knownWidth(q.Rarg)
		return l, lok && rok && l == r
	case len(q.Values) > 0:
		return len(q.Values[0]), true
	case slices.ContainsFunc(q.Targets, func(t sqlir.Target) bool { return t.Star }):
		if t, ok := c.outTypes[q]; ok {
			return len(t), true
		}
		w, ok := c.outWidth[q]
		return w, ok
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
		if t.Table != "" {
			if _, local := sc.items[t.Table]; !local {
				// An outer query's item, whose columns this scope does
				// not hold.
				return 0, false
			}
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
	input, unknown := false, false
	for _, cols := range sc.items {
		if cols == nil {
			unknown = true
		} else if found, _ := cols.has(r.Column); found {
			input = true
		}
	}
	if found, once := sc.outputs.has(r.Column); !input && found && once && !sc.anyOutput {
		if call := blockCall(sc.outputExprs[r.Column], false, false); call != "" {
			// misplacedCall cannot tell this name from an input column,
			// and an item of unknown columns may lack it, as a view's *.
			if unknown {
				return c.x.unsupported(fmt.Sprintf("GROUP BY %q, an output name holding %s or a column of an item whose columns detest does not know", r.Column, call))
			}
			return c.x.unsupported(call + " in GROUP BY")
		}
	}
	if input || unknown {
		return c.resolve(r, sc)
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

// noteCorrelation marks the queries being checked that r reads outside of:
// those inside the query whose scope has its column. One of a write's
// target, which is around every query, marks them all.
func (c *columnChecker) noteCorrelation(r *sqlir.ColumnRef, sc *colScope) {
	var owner *sqlir.SelectStmt
	found := false
	if r.Table != "" {
		for s := sc; s != nil && !found; s = s.outer {
			_, ok := s.items[r.Table]
			found = ok || r.Table == "excluded" && s.hasExcluded
			if found {
				owner = s.query
			}
		}
	} else {
		// An item whose columns are not known, as a view or a derived table
		// over a *, may or may not have the column, so any scope from the
		// innermost that may have it to the first that has it may own it.
		var owners []*colScope
		for s := sc; s != nil; s = s.outer {
			has, maybe := false, false
			for _, cols := range s.items {
				if cols == nil {
					maybe = true
				} else if h, _ := cols.has(r.Column); h {
					has = true
				}
			}
			if has || maybe {
				owners = append(owners, s)
			}
			if has {
				break
			}
		}
		if len(owners) > 1 {
			// The queries inside the innermost possible owner read around
			// themselves whichever owns it. Those from it out to the
			// outermost do only if the outer one does, which detest cannot
			// tell, so they are noted for subquery to refuse rather than run
			// once or for each row on a guess.
			c.markCorrelated(owners[0].query != nil, owners[0].query)
			outermost := owners[len(owners)-1].query
			if c.uncertain == nil {
				c.uncertain = map[*sqlir.SelectStmt]bool{}
			}
			for _, q := range slices.Backward(c.queries) {
				if outermost != nil && q == outermost {
					break
				}
				c.uncertain[q] = true
			}
			return
		}
		if len(owners) == 1 {
			found, owner = owners[0].query != nil, owners[0].query
		}
	}
	c.markCorrelated(found, owner)
}

// markCorrelated marks the queries being checked inside owner, all of them
// when owner is not found.
func (c *columnChecker) markCorrelated(found bool, owner *sqlir.SelectStmt) {
	if c.correlated == nil {
		c.correlated = map[*sqlir.SelectStmt]bool{}
	}
	for _, q := range slices.Backward(c.queries) {
		if found && q == owner {
			return
		}
		c.correlated[q] = true
	}
}

// outerAggregate reports an aggregate whose arguments read columns, none
// of them surely of sc's own query, which Postgres may make an aggregate of
// an outer query (agglevelsup).
func outerAggregate(f *sqlir.FuncCall, sc *colScope) bool {
	refs := sqlir.BlockColumnRefs(f)
	if len(refs) == 0 {
		return false
	}
	return !slices.ContainsFunc(refs, func(r *sqlir.ColumnRef) bool { return ownColumn(r, sc) })
}

// ownColumn reports a column that resolves in sc's own query, or in no
// query, which resolve reports. An unqualified column an item of unknown
// columns in sc may have is sc's only where no outer query may have it
// too, as the item may lack it and Postgres would then take the outer one.
func ownColumn(r *sqlir.ColumnRef, sc *colScope) bool {
	if r.Table != "" {
		for s := sc; s != nil; s = s.outer {
			if _, ok := s.items[r.Table]; ok || r.Table == "excluded" && s.hasExcluded {
				return s == sc
			}
		}
		return true
	}
	may := func(s *colScope) (has, unknown bool) {
		for _, cols := range s.items {
			if cols == nil {
				unknown = true
			} else if h, _ := cols.has(r.Column); h {
				has = true
			}
		}
		return has, unknown
	}
	if has, _ := may(sc); has {
		return true
	}
	for s := sc.outer; s != nil; s = s.outer {
		if h, u := may(s); h || u {
			return false
		}
	}
	return true
}

// resolve reports a column reference no FROM item in scope has.
func (c *columnChecker) resolve(r *sqlir.ColumnRef, sc *colScope) error {
	c.noteCorrelation(r, sc)
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

// nestedCall describes the first aggregate or window function in the
// arguments of an aggregate or of a window function under n, and is empty
// when there is none. Both servers fail one in an aggregate whatever rows the
// statement reads (42803 in Postgres, 1111 in MySQL), while the evaluators
// meet it only where a row reaches it. One in a window function's arguments
// runs on the servers, but detest does not evaluate it.
func nestedCall(n any) string {
	var what string
	var walk func(v reflect.Value, enclosing *sqlir.FuncCall, windowed bool)
	nested := func(call string, windowed bool) {
		if windowed {
			what = call + " in the arguments of a window function"
		} else {
			what = call + " in the arguments of an aggregate, which the server refuses"
		}
	}
	walk = func(v reflect.Value, enclosing *sqlir.FuncCall, windowed bool) {
		if what != "" {
			return
		}
		switch v.Kind() {
		case reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem(), enclosing, windowed)
			}
		case reflect.Pointer:
			if v.IsNil() {
				return
			}
			switch e := v.Interface().(type) {
			case *sqlir.SelectStmt:
				// A subquery's aggregates are its own.
				enclosing = nil
			case *sqlir.WindowFunc:
				if enclosing != nil {
					nested("a window function", windowed)
					return
				}
				if e.Func != nil {
					walk(reflect.ValueOf(e.Func).Elem(), e.Func, true)
				}
				walk(reflect.ValueOf(e.Partition), nil, false)
				walk(reflect.ValueOf(e.Order), nil, false)
				return
			case *sqlir.FuncCall:
				if isAggregateCall(e) {
					if enclosing != nil {
						nested("an aggregate", windowed)
						return
					}
					enclosing, windowed = e, false
				}
			}
			walk(v.Elem(), enclosing, windowed)
		case reflect.Struct:
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i), enclosing, windowed)
				}
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i), enclosing, windowed)
			}
		}
	}
	walk(reflect.ValueOf(n), nil, false)
	return what
}

// misplacedCall describes the first aggregate or window function under n in
// a clause of its own query that allows none, such as WHERE or the SET of an
// UPDATE, and is empty when there is none. Both servers fail one there
// whatever rows the statement reads (42803 or 42P20 in Postgres, 1111 or
// 3593 in MySQL), while the evaluators meet it only where a row reaches it.
// A GROUP BY output name is resolved by the column check, as only the
// schema tells it from an input column, which wins in Postgres.
func misplacedCall(n any) string {
	var what string
	in := func(clause string, aggregates, windows bool, nodes ...any) {
		for _, node := range nodes {
			if what != "" {
				return
			}
			if call := blockCall(node, aggregates, windows); call != "" {
				what = call + " in " + clause
			}
		}
	}
	none := func(clause string, nodes ...any) { in(clause, false, false, nodes...) }
	fromFuncs := func(items ...sqlir.TableRef) {
		for _, t := range items {
			if t.Func != nil {
				none("a function in FROM", t.Func)
			}
		}
	}
	switch st := n.(type) {
	case *sqlir.InsertStmt:
		none("VALUES", st.Rows)
		if oc := st.OnConflict; oc != nil {
			none("ON CONFLICT", oc.Elems, oc.InferWhere, oc.Set, oc.Where)
		}
		none("RETURNING", st.Returning)
	case *sqlir.UpdateStmt:
		none("the SET of an UPDATE", st.Set)
		fromFuncs(st.From...)
		none("WHERE", st.Where)
		none("ORDER BY of an UPDATE", st.OrderBy)
		none("LIMIT", st.Limit)
		none("RETURNING", st.Returning)
	case *sqlir.DeleteStmt:
		fromFuncs(st.Using...)
		none("WHERE", st.Where)
		none("ORDER BY of a DELETE", st.OrderBy)
		none("LIMIT", st.Limit)
		none("RETURNING", st.Returning)
	}
	for _, sel := range sqlir.Selects(n) {
		none("VALUES", sel.Values)
		in("the select list", true, true, sel.Targets)
		if sel.From != nil {
			fromFuncs(*sel.From)
		}
		for _, j := range sel.Joins {
			fromFuncs(j.Table)
			none("JOIN conditions", j.On)
		}
		none("WHERE", sel.Where)
		for _, g := range sel.GroupBy {
			// A position names an item of the select list, which Postgres
			// and MySQL group by as the item's expression.
			if k, ok := g.(*sqlir.Const); ok {
				if i, ok := k.Value.(int64); ok && i >= 1 && int(i) <= len(sel.Targets) && !sel.Targets[i-1].Star {
					g = sel.Targets[i-1].Expr
				}
			}
			none("GROUP BY", g)
		}
		in("HAVING", true, false, sel.Having)
		in("DISTINCT ON", true, true, sel.DistinctOn)
		if sel.SetOp != "" {
			none("the ORDER BY of a set operation", sel.OrderBy)
		} else {
			in("ORDER BY", true, true, sel.OrderBy)
		}
		none("LIMIT", sel.Limit)
		none("OFFSET", sel.Offset)
	}
	return what
}

// blockCall describes the first aggregate, unless aggregates are allowed,
// or window function, unless windows are, of n's own query, and is empty
// when there is none. A window's PARTITION BY and ORDER BY allow no window
// function in either server.
func blockCall(n any, aggregates, windows bool) string {
	var what string
	inWindow := false
	var walk func(v reflect.Value, windows bool)
	walk = func(v reflect.Value, windows bool) {
		if what != "" {
			return
		}
		switch v.Kind() {
		case reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem(), windows)
			}
		case reflect.Pointer:
			if v.IsNil() {
				return
			}
			switch e := v.Interface().(type) {
			case *sqlir.SelectStmt:
				return
			case *sqlir.WindowFunc:
				if !windows {
					what = "a window function"
					if inWindow {
						what += " in a window definition"
					}
					return
				}
				// nestedCall refuses a call in the function's arguments.
				inWindow = true
				walk(reflect.ValueOf(e.Partition), false)
				walk(reflect.ValueOf(e.Order), false)
				inWindow = false
				return
			case *sqlir.FuncCall:
				if !aggregates && isAggregateCall(e) {
					what = "an aggregate"
					return
				}
			}
			walk(v.Elem(), windows)
		case reflect.Struct:
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i), windows)
				}
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i), windows)
			}
		}
	}
	walk(reflect.ValueOf(n), windows)
	return what
}

// exprs checks the column references under n, and the subqueries in it
// with their own scopes.
func (c *columnChecker) exprs(n any, sc *colScope) error {
	var err error
	// windowed are the functions windows wrap, which Postgres computes in
	// the window's own query whatever columns they read.
	windowed := map[*sqlir.FuncCall]bool{}
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
				typ := c.x.tx.db.baseType(c.typer(sc).of(e.X))
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
				// Only the operators compareRowSubquery takes compare a row
				// with a subquery. Postgres fails a row beside a subquery
				// under any other operator, so that subquery keeps the
				// scalar width check.
				switch e.Op {
				case "=", "<>", "!=", "<", "<=", ">", ">=":
					if row, ok := e.L.(*sqlir.RowExpr); ok {
						if q, ok := e.R.(*sqlir.SubQuery); ok {
							if c.rowSubqueries == nil {
								c.rowSubqueries = map[*sqlir.SubQuery]*sqlir.RowExpr{}
							}
							c.rowSubqueries[q] = row
						}
					}
				}
				subquery := func(q *sqlir.SubQuery) string {
					typ := c.subqueryType(q, sc)
					if typ == "unresolved column type" && c.x.tx.checking {
						// CheckSQL has no schema to type a set operation's
						// column by, so it is left to the run, which
						// refuses it beside || as it may be a jsonb or an
						// array.
						return ""
					}
					return typ
				}
				if what := concatTypeMismatch(e, c.typerWith(sc, subquery)); what != "" {
					err = c.x.unsupported(what)
					return
				}
				if what := dateDifferenceMismatch(e, c.typerWith(sc, subquery)); what != "" {
					err = c.x.unsupported(what)
					return
				}
				if e.Op == "/" && slices.ContainsFunc([]sqlir.Expr{e, e.L, e.R}, func(x sqlir.Expr) bool {
					// A numeric or a float operand divides as one whatever the
					// other is, even where the result's type is unresolved.
					return numberRank[c.x.tx.db.baseType(c.typer(sc).of(x))] > numberRank["int8"]
				}) {
					if c.x.numericDivs == nil {
						c.x.numericDivs = map[*sqlir.BinaryExpr]bool{}
					}
					c.x.numericDivs[e] = true
				}
				switch e.Op {
				case "AND", "OR":
					for _, x := range []sqlir.Expr{e.L, e.R} {
						if err = c.condition(x, sc, e.Op); err != nil {
							return
						}
					}
				case "=", "<>", "!=", "<", "<=", ">", ">=":
					if err = c.operandTypes(sc, e.L, e.R); err != nil {
						return
					}
					c.untypedOperands(sc, e.L, e.R)
				case "+", "-", "*", "/", "%":
					if err = c.arithmeticTypes(sc, e.Op, e.L, e.R); err != nil {
						return
					}
					if err = c.arithmeticOperand(sc, e); err != nil {
						return
					}
				}
			case *sqlir.UnaryExpr:
				if e.Op == "-" {
					if err = c.arithmeticTypes(sc, "-", e.X); err != nil {
						return
					}
				}
				if e.Op == "NOT" {
					if err = c.condition(e.X, sc, "NOT"); err != nil {
						return
					}
				}
			case *sqlir.InExpr:
				if e.Sub != nil {
					if c.inSubqueries == nil {
						c.inSubqueries = map[*sqlir.SelectStmt]sqlir.Expr{}
					}
					c.inSubqueries[e.Sub] = e.X
				}
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
				if err = c.branches(e, caseBranches(e), sc); err != nil {
					return
				}
				if e.Arg != nil {
					// Postgres compares a row argument with each WHEN as
					// records, whose operator it resolves only at a row, and
					// the run refuses the row there.
					if _, row := e.Arg.(*sqlir.RowExpr); !row {
						for _, w := range e.Whens {
							if err = c.operandTypes(sc, e.Arg, w.When); err != nil {
								return
							}
							c.untypedOperands(sc, e.Arg, w.When)
						}
					}
				} else {
					for _, w := range e.Whens {
						if err = c.condition(w.When, sc, "CASE/WHEN"); err != nil {
							return
						}
					}
				}
			case *sqlir.WindowFunc:
				windowed[e.Func] = true
			case *sqlir.FuncCall:
				if isAggregateCall(e) && sc.outer != nil && !windowed[e] && len(sqlir.Selects(e.Args)) > 0 {
					// A subquery's columns may be of any query out from it,
					// and the aggregate's level with them, which the check
					// below does not follow, as rowSequenceCalls refuses
					// the form outside a subquery.
					err = c.x.unsupported("a subquery in an aggregate's argument")
					return
				}
				if isAggregateCall(e) && sc.outer != nil && !windowed[e] && outerAggregate(e, sc) {
					// Postgres computes an aggregate whose arguments read
					// only outer columns in the outer query, which then
					// aggregates, while the evaluators compute it over the
					// subquery's rows.
					err = c.x.unsupported("an aggregate in a subquery of no column surely the subquery's, which Postgres may compute in the outer query")
					return
				}
				if what := textArgumentMismatch(e, c.typer(sc)); what != "" {
					err = c.x.unsupported(what)
					return
				}
				if what := aggregateArgumentMismatch(e, c.typer(sc), !c.x.tx.checking); what != "" {
					err = c.x.unsupported(what)
					return
				}
				// Postgres compares two rows under NULLIF as records, whose
				// operator it resolves only at a row, rather than field by
				// field as a row comparison does.
				if e.Name == "nullif" && len(e.Args) == 2 {
					if _, row := e.Args[0].(*sqlir.RowExpr); !row {
						c.untypedOperands(sc, e.Args[0], e.Args[1])
					}
				}
				if what := intervalBranchMismatch(branchArgs(e), sc.columnType, func(q *sqlir.SubQuery) string { return c.subqueryType(q, sc) }); what != "" {
					err = c.x.unsupported(what)
					return
				}
				if what := unknownPolymorphic(e); what != "" && !c.x.tx.db.kind.InnoDB() {
					err = c.x.unsupported(what)
					return
				}
				switch e.Name {
				case "coalesce", "greatest", "least", "nullif":
					if err = c.branchTypes(e.Args, sc, "branches"); err != nil {
						return
					}
					if e.Name == "coalesce" && slices.ContainsFunc(e.Args, func(a sqlir.Expr) bool {
						return expressionTypeIn(a, sc.columnType, func(q *sqlir.SubQuery) string { return c.subqueryType(q, sc) }) == "void"
					}) {
						// A void value is not NULL in Postgres, which stops
						// COALESCE there, while the evaluators hold it as NULL.
						err = c.x.unsupported("COALESCE of a void value, which detest holds as NULL")
						return
					}
					// The arguments take the type of the typed ones.
					if err = c.branches(e, e.Args, sc); err != nil {
						return
					}
				case "date_trunc":
					if len(e.Args) == 2 {
						c.x.noteType(e.Args[1], c.typer(sc).of(e.Args[1]))
					}
				case "lag", "lead":
					if len(e.Args) > 1 {
						// computeWindow reads an offset of another type as 1,
						// where Postgres takes an int4 only.
						off := e.Args[1]
						typ := c.typer(sc).of(off)
						if untypedBranch(off) && !untypedExpr(off) {
							typ = "text" // a CASE or COALESCE of literals only
						}
						if r := numberRank[typ]; typ != "" && (r == 0 || r > numberRank["int4"]) {
							err = c.x.unsupported(fmt.Sprintf("%s with a %s offset", e.Name, typ))
							return
						}
						if _, isParam := off.(*sqlir.Param); typ == "" && !isParam && !isNullConst(off) && !c.x.tx.checking {
							// Postgres takes an int4 offset only, which the
							// check cannot show of an expression it cannot type.
							err = c.x.unsupported(e.Name + " with an offset of a type detest does not know")
							return
						}
						if _, ok := c.x.untypedText(off); ok {
							err = c.x.unsupported(e.Name + " with a string literal or parameter offset")
							return
						}
						if p, ok := off.(*sqlir.Param); ok && p.Index >= 0 && p.Index < len(c.x.args) {
							if v := derefValue(c.x.args[p.Index]); v != nil {
								if _, isInt := integer(v); !isInt {
									err = c.x.unsupported(fmt.Sprintf("%s with a %T parameter offset", e.Name, v))
									return
								}
							}
							c.inputAs(p, "int4")
						}
					}
					// The value and the default take one type, as the
					// arguments of COALESCE do.
					if err = c.branches(e, branchArgs(e), sc); err != nil {
						return
					}
				}
			case *sqlir.Collate:
				// Postgres fails a collation that does not exist when it
				// parses the statement (42704).
				if _, ok := c.x.tx.db.kind.NamedCollation(e.Name); !ok && !c.x.tx.db.kind.InnoDB() {
					err = c.x.unsupported(fmt.Sprintf("collation %q, which postgres.Collations does not declare", e.Name))
					return
				}
				c.x.collates = true
				if !c.x.tx.db.kind.InnoDB() {
					// Postgres fails COLLATE on a type of no collation
					// (42804) before any row.
					typ := c.typer(sc).of(e.X)
					if typ != "" && typ != "unresolved column type" && !untypedExpr(e.X) && !c.x.tx.db.collatable(typ) {
						err = c.x.unsupported("COLLATE on a " + typ)
						return
					}
				}
				if p, ok := e.X.(*sqlir.Param); ok && p.Index >= 0 && p.Index < len(c.x.args) && !c.x.tx.db.kind.InnoDB() {
					switch v := derefValue(c.x.args[p.Index]).(type) {
					case nil, string, []byte:
					default:
						// COLLATE makes it text, which the driver sends and
						// detest does not convert the bound value to.
						err = c.x.unsupported(fmt.Sprintf("a %T parameter under COLLATE", v))
						return
					}
					// COLLATE types it text for its other uses.
					c.params.comparedWith(p.Index, c.paramTypeFamily("text"))
				}
			case *sqlir.Cast:
				if expressionTypeIn(e.X, sc.columnType, func(q *sqlir.SubQuery) string { return c.subqueryType(q, sc) }) == "void" {
					// Postgres casts a void value to the empty string,
					// which is not NULL, while the evaluators hold it as
					// NULL.
					err = c.x.unsupported("a cast of a void value, which detest holds as NULL")
					return
				}
				if err = c.literalInput(e); err != nil {
					return
				}
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
				if _, err = c.query(e, sc); err != nil {
					return
				}
				if x, ok := c.inSubqueries[e]; ok {
					want := 1
					if row, ok := x.(*sqlir.RowExpr); ok {
						want = len(row.Items)
					}
					// Postgres fails IN (SELECT ...) of other than the
					// width of what it compares when it analyzes the
					// statement.
					if w, ok := c.knownWidth(e); ok && w != want {
						err = c.x.unsupported("IN (subquery) whose columns do not match the left side")
						return
					}
					err = c.subqueryOperandTypes(sc, x, e)
				}
				return
			case *sqlir.SubQuery:
				if _, err = c.query(e.Select, sc); err != nil {
					return
				}
				if row, ok := c.rowSubqueries[e]; ok {
					if err = c.subqueryOperandTypes(sc, row, e.Select); err != nil {
						return
					}
				}
				// Postgres fails a subquery of other than the width its place
				// takes when it analyzes the statement, which the run refuses
				// only where a row reaches it.
				w, ok := c.knownWidth(e.Select)
				if !ok {
					return
				}
				if row, ok := c.rowSubqueries[e]; ok {
					if w != len(row.Items) {
						err = c.x.unsupported("a row compared with a subquery of another number of columns")
					}
				} else if w != 1 {
					err = c.x.unsupported("a scalar subquery of other than one column")
				}
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

// branches checks and records exprs, the branches of e, which Postgres
// resolves to one type: refusing types detest does not convert them to
// before it reads any literal as that type, as Postgres fails branches of
// two types before it converts one, and resolving subqueries among them in
// their own scope.
func (c *columnChecker) branches(e sqlir.Expr, exprs []sqlir.Expr, sc *colScope) error {
	subquery := func(q *sqlir.SubQuery) string { return c.subqueryType(q, sc) }
	err, afterInput := c.x.unconvertedBranches(exprs, c.typerWith(sc, subquery))
	if err != nil {
		return err
	}
	typ := c.typerWith(sc, subquery).of(e)
	if _, isCase := e.(*sqlir.CaseExpr); !isCase {
		// NULLIF's own type is its first argument's, but the arguments
		// meet at one type.
		typ = c.typerWith(sc, subquery).common(exprs)
	}
	c.timeParams(typ, exprs...)
	for _, b := range exprs {
		c.inputAs(b, resolvedType(typ, exprs))
	}
	if afterInput != nil {
		c.untyped = append(c.untyped, func() error { return afterInput })
	}
	if rt := resolvedType(typ, exprs); timeRank[rt] > 0 {
		// toTimeType converts the branch a row takes, and refuses text in
		// a format it does not parse, which inputAs leaves to the run.
		for _, b := range exprs {
			if text, ok := c.x.untypedText(b); ok {
				c.untyped = append(c.untyped, func() error {
					_, err := c.x.toTimeType(text, rt)
					return err
				})
			}
		}
	}
	if f, ok := e.(*sqlir.FuncCall); ok && f.Name == "nullif" {
		// NULLIF returns its first argument, of its own type, which a
		// reader of the result such as date_trunc goes by.
		c.x.noteType(e, c.typerWith(sc, subquery).of(e))
	} else {
		c.x.noteType(e, typ)
	}
	if err := c.x.noteBranchType(e, resolvedType(typ, exprs), exprs); err != nil {
		return err
	}
	c.branchParams(exprs, typ)
	return c.x.noteNumberBranches(e, exprs, c.typerWith(sc, subquery))
}

// branchParams records the parameters among number branches, whose
// other uses the statement check refuses. inputAs records their type.
func (c *columnChecker) branchParams(exprs []sqlir.Expr, typ string) {
	typ = resolvedType(typ, exprs)
	for _, e := range exprs {
		if p, ok := e.(*sqlir.Param); ok {
			if numberRank[typ] > 0 {
				if c.numberParams == nil {
					c.numberParams = map[int]bool{}
				}
				c.numberParams[p.Index] = true
			}
		}
	}
}

// untypedOperands reads a string literal or parameter compared with an
// operand of a type the statement shows, as Postgres reads it when it plans
// or binds the statement. untypedPair reads it again for each row, but a
// statement that reaches no row, over an empty table or behind a
// short-circuited OR, must still fail with the input's error.
func (c *columnChecker) untypedOperands(sc *colScope, a, b sqlir.Expr) {
	if ra, rb, ok := rowPair(a, b); ok {
		for i := range ra {
			c.untypedOperands(sc, ra[i], rb[i])
		}
		return
	}
	if untypedExpr(a) == untypedExpr(b) {
		return
	}
	if untypedExpr(b) {
		a, b = b, a
	}
	c.untypedAs(a, c.x.tx.db.baseType(c.typer(sc).of(b)))
}

// untypedAs reads a, a string literal or a parameter, as the type typ of
// what it is compared with, as untypedOperands does.
func (c *columnChecker) untypedAs(a sqlir.Expr, typ string) {
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

// arithmeticOperand reads a string literal or a parameter beside a number
// in arithmetic as that number's type, which Postgres gives it, as $1 + 0
// makes $1 an integer, so that text of no number fails before any row as
// it does on the server. Beside an operand of another type, such as a
// timestamp or an interval, Postgres picks among several operators, which
// is left to the run.
func (c *columnChecker) arithmeticOperand(sc *colScope, e *sqlir.BinaryExpr) error {
	a, b := e.L, e.R
	if untypedExpr(a) == untypedExpr(b) {
		return nil
	}
	if untypedExpr(b) {
		a, b = b, a
	}
	typ := c.x.tx.db.baseType(c.typer(sc).of(b))
	if typ == ambiguousDomain {
		// Postgres reads the input by the base type of the domain the
		// search path finds, which detest does not tell apart.
		return c.x.unsupported("a string literal or parameter in arithmetic beside " + ambiguousDomain)
	}
	if numberRank[typ] == 0 || e.Op == "%" && numberRank[typ] > numberRank["numeric"] {
		// Postgres has no % of floats, so it does not take the
		// operand's type there.
		return nil
	}
	if p, ok := a.(*sqlir.Param); ok && numberRank[typ] <= numberRank["int8"] && p.Index >= 0 && p.Index < len(c.x.args) && isFloat(c.x.args[p.Index]) {
		// Postgres binds the parameter as an integer, so $1 / 2 with 5.0
		// is 2, while the run computes with the float as bound.
		return c.x.unsupported(fmt.Sprintf("a float bound to parameter $%d taken as %s in arithmetic", p.Index+1, typ))
	}
	c.inputAs(a, typ)
	return nil
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

// typer types expressions in sc as Postgres resolves them, with a domain
// reduced to its base type where an operator or a function takes it.
func (c *columnChecker) typer(sc *colScope) typer {
	return c.typerWith(sc, func(q *sqlir.SubQuery) string { return c.subqueryType(q, sc) })
}

func (c *columnChecker) typerWith(sc *colScope, subquery func(*sqlir.SubQuery) string) typer {
	return typer{column: sc.columnType, subquery: subquery, base: c.x.tx.db.baseType}
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
	c.untyped = append(c.untyped, func() error { return c.readCast(e) })
}

// literalInput reads a string literal cast to a type where the walk stands,
// as Postgres converts it where it analyzes the cast, before the errors of
// what it analyzes later, such as a set operation's widths. A parameter is
// read once the walk ends, as Postgres binds it after analyzing the whole
// statement.
func (c *columnChecker) literalInput(e *sqlir.Cast) error {
	if _, ok := e.X.(*sqlir.Const); ok && untypedExpr(e.X) {
		return c.readCast(e)
	}
	c.castInput(e)
	return nil
}

// readCast is castInput's read of e.
func (c *columnChecker) readCast(e *sqlir.Cast) error {
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
// operator for, which untypedPair refuses only when it reaches a row. An
// expression of string literals and NULLs alone, such as COALESCE('1', NULL)
// or max('1'), is text, which Postgres resolves it to before it looks for the
// operator.
func (c *columnChecker) operandTypes(sc *colScope, a, b sqlir.Expr) error {
	t := c.typer(sc)
	t.untypedText = true
	// A column made of a row, which noteOutputTypes types as a record,
	// compares as the row itself.
	isRow := func(e sqlir.Expr) bool {
		_, ok := e.(*sqlir.RowExpr)
		return ok || t.of(e) == "record"
	}
	aRow, bRow := isRow(a), isRow(b)
	if aRow != bRow {
		other := b
		if bRow {
			other = a
		}
		if q, sub := other.(*sqlir.SubQuery); sub && c.rowSubqueries[q] != nil {
			return nil // subqueryOperandTypes types a row against its subquery
		}
		return c.rowWithValue(t, other)
	}
	if ra, rb, ok := rowPair(a, b); ok {
		// Postgres resolves the operator of each pair of fields.
		for i := range ra {
			if err := c.operandTypes(sc, ra[i], rb[i]); err != nil {
				return err
			}
		}
		return nil
	}
	if untypedExpr(a) || untypedExpr(b) {
		return nil
	}
	return c.familyTypes(t.reduce(t.of(a)), t.reduce(t.of(b)))
}

// familyTypes refuses a comparison of a value of type ta with one of type
// tb, where Postgres has no operator between their families.
func (c *columnChecker) familyTypes(ta, tb string) error {
	fa, fb := valueFamily(ta), valueFamily(tb)
	if fa == "" || fb == "" || fa == fb {
		return nil
	}
	return c.x.unsupported(fmt.Sprintf("a comparison of a %s with a %s", fa, fb))
}

// rowWithValue refuses a comparison of a row with other, which is not one,
// where Postgres has no operator between a record and other's type. A NULL
// takes the record type, and a value of a type the check does not know may
// be a record. Postgres reads a string literal there as a record's input,
// which it does not implement (0A000), and a parameter as a record, which
// detest does not model.
func (c *columnChecker) rowWithValue(t typer, other sqlir.Expr) error {
	if isNullConst(other) {
		return nil
	}
	if untypedExpr(other) {
		return c.x.unsupported("a string literal or a parameter compared with a row")
	}
	if f := valueFamily(t.reduce(t.of(other))); f != "" {
		return c.x.unsupported(fmt.Sprintf("a comparison of a row with a %s", f))
	}
	return nil
}

// rowPair is the fields of a and b when both are rows of one width, which
// Postgres compares pair by pair. A pair of fields that are both rows is
// left out, as Postgres compares them as records, whose operator it
// resolves only at a row.
func rowPair(a, b sqlir.Expr) ([]sqlir.Expr, []sqlir.Expr, bool) {
	ra, aok := a.(*sqlir.RowExpr)
	rb, bok := b.(*sqlir.RowExpr)
	if !aok || !bok || len(ra.Items) != len(rb.Items) {
		return nil, nil, false
	}
	var fa, fb []sqlir.Expr
	for i := range ra.Items {
		_, ia := ra.Items[i].(*sqlir.RowExpr)
		_, ib := rb.Items[i].(*sqlir.RowExpr)
		if !ia || !ib {
			fa, fb = append(fa, ra.Items[i]), append(fb, rb.Items[i])
		}
	}
	return fa, fb, true
}

// subqueryOperandTypes refuses a comparison of x, a value or a row, with
// the columns of sel, the subquery of IN (SELECT ...) or of a row
// comparison, once the walk has checked sel, where Postgres has no operator
// for a pair, and reads a string literal or a parameter among x's fields as
// the type of its column.
func (c *columnChecker) subqueryOperandTypes(sc *colScope, x sqlir.Expr, sel *sqlir.SelectStmt) error {
	fields := []sqlir.Expr{x}
	if row, ok := x.(*sqlir.RowExpr); ok {
		fields = row.Items
	}
	var types []string
	if sel.SetOp != "" || len(sel.Values) > 0 {
		// unsentOutputParam is not asked, as Postgres resolves a column
		// whatever a parameter in it is bound to, and typing the column can
		// only refuse the comparison.
		types = c.itemTypes(sel)
	} else {
		var ok bool
		if types, ok = c.readTypes(sel); !ok {
			return nil
		}
		// Postgres resolves a parameter among the columns to text, as it
		// does a string literal or a NULL there.
		c.readOutputs(sel)
	}
	if len(types) != len(fields) {
		return nil
	}
	t := c.typer(sc)
	t.untypedText = true
	for i, f := range fields {
		_, nested := f.(*sqlir.RowExpr)
		nested = nested || t.of(f) == "record"
		if types[i] == "record" && !nested {
			if err := c.rowWithValue(t, f); err != nil {
				return err
			}
			continue
		}
		if nested {
			// A row's type is a record, which Postgres has no operator
			// with any type the check knows.
			if f := valueFamily(t.reduce(types[i])); f != "" {
				return c.x.unsupported(fmt.Sprintf("a comparison of a row with a %s", f))
			}
			continue
		}
		if types[i] == "" {
			continue
		}
		if untypedExpr(f) {
			c.untypedAs(f, c.x.tx.db.baseType(types[i]))
			continue
		}
		if err := c.familyTypes(t.reduce(t.of(f)), t.reduce(types[i])); err != nil {
			return err
		}
	}
	return nil
}

// condition refuses e standing where Postgres wants a boolean, as the
// clause of, when e is of a type the check knows and that type is not
// boolean. Postgres converts only an untyped literal or parameter there,
// which the parser has already made a cast to boolean, and fails any other
// type when it plans the statement (42804), while the run reads the value
// at each row and returns nothing over no rows. An expression of string
// literals and NULLs alone, such as COALESCE('t', NULL), is text there.
func (c *columnChecker) condition(e sqlir.Expr, sc *colScope, clause string) error {
	if e == nil || c.x.tx.db.kind.InnoDB() {
		return nil // MySQL reads any value as a truth value
	}
	t := c.typer(sc)
	t.untypedText = true
	switch typ := t.reduce(t.of(e)); typ {
	case "", "bool", "boolean", "unresolved column type", ambiguousDomain, constrainedDomain:
		return nil
	default:
		if c.x.tx.checking && valueFamily(typ) == "" && !collatableType(typ) {
			// CheckSQL has no schema, so a type it does not know may be a
			// domain over boolean, which the run reduces.
			return nil
		}
		return c.x.unsupported(fmt.Sprintf("a %s as the argument of %s, which Postgres wants a boolean for", typ, clause))
	}
}

// arithmeticTypes refuses arithmetic over a boolean, a uuid, a record or a
// character type that Postgres has no operator for and fails when it plans
// the statement (42883), while the evaluators check the operands only where
// a row reaches them. operands are the left and the right operand of op, or
// the one operand of unary minus. A domain counts as its base type, and an
// expression of string literals and NULLs, such as COALESCE('1', NULL) or
// min('1'), as text, which Postgres resolves it to. No operator takes a
// boolean, a uuid or a record. The only one that takes a character type is
// jsonb - text, so a character type is refused anywhere but on the right of
// a binary minus, and there only beside a type detest classifies, which
// cannot be jsonb, rather than beside a string literal, a parameter, a bare
// NULL or a type whose operators detest does not list, as in '{}' - s.
func (c *columnChecker) arithmeticTypes(sc *colScope, op string, operands ...sqlir.Expr) error {
	t := c.typer(sc)
	t.untypedText = true
	untyped := func(e sqlir.Expr) bool { return untypedExpr(e) || isNullConst(e) }
	typeOf := func(e sqlir.Expr) string { return operandType(t, e) }
	if len(operands) == 1 && untyped(operands[0]) {
		// Several prefix operators take a number, and Postgres picks
		// none for an operand of no type (42725), while the run would
		// negate the value bound. Postgres accepts -$1 when a use it
		// analyses earlier types the parameter, as in $1 = n, -$1, but
		// that order is not the statement's (UPDATE reads WHERE before
		// SET) and detest does not follow it, as numberParam does not,
		// so -$1 is refused wherever it stands and -$1::int is the form
		// that runs.
		return c.x.unsupported("unary minus over a string literal, a parameter or NULL, for which Postgres picks no operator")
	}
	for i, o := range operands {
		if untyped(o) {
			continue
		}
		typ := typeOf(o)
		switch {
		case typ == "bool" || typ == "boolean" || typ == "uuid" || typ == "record":
		case collatableType(typ):
			if len(operands) == 2 && i == 1 && op == "-" {
				l := operands[0]
				if untyped(l) {
					continue
				}
				if lt := typeOf(l); valueFamily(lt) == "" && lt != "date" && !collatableType(lt) {
					continue
				}
			}
		default:
			continue
		}
		return c.x.unsupported(fmt.Sprintf("arithmetic over a %s, for which Postgres has no operator", typ))
	}
	return nil
}

// branchTypes refuses branches of CASE, arguments of COALESCE, GREATEST,
// LEAST or NULLIF, or rows of a VALUES column, of two types Postgres does not resolve to one,
// which it fails when it plans the statement (42804) and the run would
// return one of. Two types are taken as one only where valueFamily groups
// them, or as dates and timestamps, which typer's common widens; another pair
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
		typ := c.typer(sc).of(e)
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
		sc.noteCertain(targetAlias(table, alias), def.types)
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
	c.x.returningTypes, c.x.returningStar = make([]string, len(ts)), sc.certain[target]
	for i, t := range ts {
		if !t.Star {
			if err := c.exprs(t.Expr, sc); err != nil {
				return err
			}
			c.x.returningTypes[i] = c.certainType(t.Expr, sc)
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
		if err := c.textAssigned(ins.Table, ins.Columns, row, values); err != nil {
			return err
		}
		c.assignedParams(ins.Table, ins.Columns, row)
	}
	if err := c.insertRows(ins); err != nil {
		return err
	}
	if ins.Select != nil {
		if _, err := c.query(ins.Select, nil); err != nil {
			return err
		}
		if err := c.insertWidth(ins); err != nil {
			return err
		}
		if err := c.textInsert(ins); err != nil {
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
		up.noteCertain("excluded", sc.certain[targetAlias(ins.Table, ins.Alias)])
		if err := c.exprs(assignedValues(oc.Set), up); err != nil {
			return err
		}
		if err := c.textAssigned(ins.Table, assignedColumns(oc.Set), assignedValues(oc.Set), up); err != nil {
			return err
		}
		if err := c.exprs(oc.Where, up); err != nil {
			return err
		}
		if err := c.condition(oc.Where, up, "WHERE"); err != nil {
			return err
		}
	}
	return c.returning(ins.Returning, sc, targetAlias(ins.Table, ins.Alias))
}

// textInsert refuses INSERT ... SELECT of a column of text into a column of
// another type, which Postgres fails when it plans the statement (42804),
// as text has no assignment cast to it, while the run would convert the
// text as input at each row and write nothing over no rows. A column of
// text is one of a text type, a CASE or COALESCE of untyped branches alone,
// or one of a set operation of untyped queries alone. An untyped literal is
// not: Postgres converts it as the column's input.
func (c *columnChecker) textInsert(ins *sqlir.InsertStmt) error {
	sel := ins.Select
	def := c.x.tx.db.defs[c.x.tx.db.resolve(ins.Table)]
	if def == nil || c.x.tx.db.kind.InnoDB() {
		return nil
	}
	cols := ins.Columns
	if len(cols) == 0 {
		cols = def.columns
	}
	var types []string
	switch {
	case sel.SetOp != "":
		types = c.setOpTypes(sel)
	case len(sel.Values) > 0:
		types = c.valuesTypes[sel]
	default:
		types = c.outTypes[sel]
	}
	for i, col := range cols {
		typ := ""
		if i < len(types) {
			typ = types[i]
		}
		if sel.SetOp != "" {
			typ = resolvedType(typ, setOpColumnExprs(sel, i))
		}
		if typ == "" || c.paramTypeFamily(typ) != "text" {
			continue
		}
		if target := def.types[col]; target != "" && c.paramTypeFamily(target) != "text" {
			return c.x.unsupported(fmt.Sprintf("INSERT ... SELECT of a %s column into the %s column %q", typ, target, col))
		}
	}
	return nil
}

// textAssigned refuses an UPDATE's SET, an INSERT's VALUES row or an ON
// CONFLICT DO UPDATE's SET that writes an expression of a text type, or of
// name, to cols, a column of a type neither has an assignment cast to,
// which Postgres fails when it plans the statement (42804), as textInsert
// does for INSERT ... SELECT. The run would read the text as the column's
// input at each row, and write nothing over no rows. A string literal, a
// parameter or a NULL is not text here, as Postgres reads it as the
// column's input, while a CASE or COALESCE of those alone, or a scalar
// subquery returning one, is, as Postgres resolves those to text first.
func (c *columnChecker) textAssigned(table string, cols []string, exprs []sqlir.Expr, sc *colScope) error {
	db := c.x.tx.db
	def := db.defs[db.resolve(table)]
	if def == nil || db.kind.InnoDB() {
		return nil
	}
	if len(cols) == 0 {
		cols = def.columns
	}
	t := c.typer(sc)
	t.untypedText = true
	for i, e := range exprs {
		if i >= len(cols) {
			break
		}
		if col, ok := e.(*sqlir.Collate); ok && (untypedExpr(col.X) || isNullConst(col.X)) {
			// The typer takes a literal under COLLATE as text, as Postgres
			// does in a comparison, but an assignment leaves it for the
			// column's input to read.
			continue
		}
		typ := t.reduce(t.of(e))
		if c.paramTypeFamily(typ) != "text" && typ != "name" {
			continue
		}
		if target := def.types[cols[i]]; noTextAssignment(db, target) {
			return c.x.unsupported(fmt.Sprintf("an assignment of a %s expression to the %s column %q", typ, target, cols[i]))
		}
	}
	return nil
}

// noTextAssignment reports whether typ is a type text has no assignment
// cast to. It lists the types detest checks rather than taking every type
// but text, as Postgres casts text to some others on assignment, such as
// name and a domain over text.
func noTextAssignment(db *DB, typ string) bool {
	base := db.baseType(typ)
	if db.enumOf(base) != nil || timeRank[base] > 0 {
		return true
	}
	switch paramFamily(base) {
	case "integer", "float", "bool", "boolean", "uuid", "interval":
		return true
	}
	return base == "json" || base == "jsonb"
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
	if err := c.textAssigned(up.Table, assignedColumns(up.Set), assignedValues(up.Set), sc); err != nil {
		return err
	}
	c.assignedParams(up.Table, assignedColumns(up.Set), assignedValues(up.Set))
	c.assignedInput(up.Table, assignedColumns(up.Set), assignedValues(up.Set))
	if err := c.exprs(up.Where, sc); err != nil {
		return err
	}
	if err := c.condition(up.Where, sc, "WHERE"); err != nil {
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
		sc.noteCertain(alias, own.certain[alias])
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
	if err := c.condition(del.Where, sc, "WHERE"); err != nil {
		return err
	}
	return c.returning(del.Returning, sc, targetAlias(del.Table, del.Alias))
}

// unsentOutputParam reports a set operation or VALUES query whose output
// is a parameter bound to a value the query returns as bound, where
// Postgres reads the text the driver sends.
func (c *columnChecker) unsentOutputParam(sel *sqlir.SelectStmt) bool {
	for _, q := range sqlir.Selects(sel) {
		var outs []sqlir.Expr
		for _, t := range q.Targets {
			outs = append(outs, t.Expr)
		}
		for _, row := range q.Values {
			outs = append(outs, row...)
		}
		for _, e := range outs {
			p, ok := e.(*sqlir.Param)
			if !ok || p.Index < 0 || p.Index >= len(c.x.args) {
				continue
			}
			switch derefValue(c.x.args[p.Index]).(type) {
			case nil, string:
			default:
				return true
			}
		}
	}
	return false
}
