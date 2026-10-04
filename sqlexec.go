package detest

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/k1LoW/detest/internal/sqlir"
)

// This file executes detest's SQL IR against the abstract DB: single and joined
// SELECT with CTEs, aggregation, DISTINCT, ORDER BY, LIMIT/OFFSET and row
// locking; INSERT with ON CONFLICT DO NOTHING / DO UPDATE and RETURNING;
// UPDATE ... FROM and DELETE ... USING; subqueries in expressions. Statements
// the IR cannot express never get here: the dialect frontend rejects them.

type sqlResult struct {
	cols     []string
	rows     [][]driver.Value
	affected int64
	// lastID is the AUTO_INCREMENT value an InnoDB insert generated first.
	lastID    int64
	hasLastID bool
}

// sqlExec is the per-statement executor state.
type sqlExec struct {
	tx    *Tx
	query string
	args  []driver.Value
	ctes  map[string][]Row
	start time.Time // when the statement began, for statement_timestamp()
	// bounds are the OFFSET and LIMIT of the queries being evaluated, taken
	// before each query runs anything.
	bounds map[*sqlir.SelectStmt][2]int
	// consistent makes the statement's table reads InnoDB consistent reads,
	// from the transaction's snapshot: a plain SELECT at Repeatable Read.
	consistent bool
	// blocks counts the query blocks a SELECT statement has begun, so that
	// a nested one reads by its own locking clause.
	blocks   int
	inSelect bool
	// selectStmt is set for a SELECT statement, as opposed to the query of
	// an INSERT ... SELECT or a CREATE TABLE ... SELECT.
	selectStmt bool
}

// env is the evaluation context of an expression: the rows of the tables in
// scope by alias, their merged columns, the enclosing query's context for
// correlated subqueries, and the proposed row of ON CONFLICT DO UPDATE.
type env struct {
	tables   map[string]Row
	merged   Row
	outer    *env
	excluded Row
	win      map[*sqlir.WindowFunc]any // the window function values of the row
}

func (e *env) lookup(table, col string) (any, bool) {
	for cur := e; cur != nil; cur = cur.outer {
		if table != "" {
			if table == "excluded" && cur.excluded != nil {
				v, ok := cur.excluded[col]
				return v, ok
			}
			if r, ok := cur.tables[table]; ok {
				v, ok := r[col]
				return v, ok
			}
			continue
		}
		if v, ok := cur.merged[col]; ok {
			return v, true
		}
	}
	return nil, false
}

// jrow is a row of a FROM clause: the rows of each table by alias plus the
// merged view used for unqualified columns.
type jrow struct {
	by     map[string]Row
	merged Row
	base   Row // the base table's row, for locking and writes (nil when derived)
}

func newJrow(alias string, r Row) jrow {
	j := jrow{by: map[string]Row{alias: r}, merged: Row{}, base: r}
	maps.Copy(j.merged, r)
	return j
}

func (j jrow) with(alias string, r Row) jrow {
	n := jrow{by: make(map[string]Row, len(j.by)+1), merged: make(Row, len(j.merged)+len(r)), base: j.base}
	maps.Copy(n.by, j.by)
	maps.Copy(n.merged, j.merged)
	if r != nil {
		n.by[alias] = r
		for k, v := range r {
			if _, exists := n.merged[k]; !exists {
				n.merged[k] = v
			}
		}
	} else {
		n.by[alias] = nil
	}
	return n
}

func (j jrow) env(outer *env) *env { return &env{tables: j.by, merged: j.merged, outer: outer} }

// rebind replaces the base table's row (after a lock granted a newer version)
// and rebuilds the merged view with the base row's columns taking precedence.
func (j jrow) rebind(alias string, cur Row) jrow {
	n := newJrow(alias, cur)
	for a, r := range j.by {
		if a == alias || r == nil {
			if a != alias {
				n.by[a] = nil
			}
			continue
		}
		n.by[a] = r
		for k, v := range r {
			if _, exists := n.merged[k]; !exists {
				n.merged[k] = v
			}
		}
	}
	return n
}

func (s *parsedStatement) exec(tx *Tx, args []driver.Value) (*sqlResult, error) {
	if sp, ok := s.stmt.(*sqlir.SavepointStmt); ok {
		// ROLLBACK TO is how an aborted transaction continues, so it skips the
		// aborted check the other statements take.
		tx.yieldf("%s: %s %s", tx.db.name, strings.ReplaceAll(sp.Op, "_", " "), sp.Name)
		return &sqlResult{}, tx.savepoint(sp.Op, sp.Name)
	}
	if err := tx.check(); err != nil {
		return nil, err
	}
	x := &sqlExec{tx: tx, query: s.query, args: args, ctes: map[string][]Row{}, start: time.Now()}
	if !tx.block && !tx.start.IsZero() {
		x.start = tx.start // the statement is its own transaction, begun at the same instant
	}
	if err := x.collationCheck(s.stmt); err != nil {
		return nil, err
	}
	res, err := x.execStatement(s.stmt)
	if err != nil {
		// Every path that evaluates an expression ends here, so an
		// expression detest cannot evaluate that no path named on its own
		// still leaves as ErrUnsupportedSQL, which callers and CheckSQL
		// branch on.
		return nil, x.unsupportedExpr(err, "in the statement")
	}
	return res, nil
}

// ddlInTransaction refuses MySQL DDL inside a transaction block: MySQL
// commits the transaction before it, which detest does not do, so a later
// rollback would undo the transaction's writes while the schema change
// stays.
func (x *sqlExec) ddlInTransaction() error {
	if x.tx.db.kind.InnoDB() && x.tx.block && !x.tx.checking {
		return x.unsupported("DDL inside a transaction")
	}
	return nil
}

func (x *sqlExec) execStatement(stmt sqlir.Statement) (*sqlResult, error) {
	tx := x.tx
	switch st := stmt.(type) {
	case *sqlir.Script:
		res := &sqlResult{}
		for _, sub := range st.Stmts {
			x.ctes = map[string][]Row{}
			r, err := x.execStatement(sub)
			if err != nil {
				return nil, err
			}
			res = r
		}
		return res, nil
	case *sqlir.CreateTableAsStmt:
		if err := x.ddlInTransaction(); err != nil {
			return nil, err
		}
		return x.execCreateTableAs(st)
	case *sqlir.RefreshStmt:
		return x.execRefresh(st)
	case *sqlir.SetStmt:
		if st.Name == "lock_timeout" {
			// Any positive timeout makes every lock wait of the transaction a
			// choice between waiting and timing out; the duration does not
			// matter, as detest's waits have no length.
			x.tx.lockTimeout = st.Value != "" && st.Value != "0" && st.Value != "0ms" && st.Value != "0s"
		}
		if st.Name == "no_auto_value_on_zero" {
			x.tx.noAutoZero = st.Value == "true"
		}
		if st.Name == "foreign_key_checks" {
			x.tx.noFKChecks = st.Value == "false"
		}
		if st.Name == "database" && !strings.EqualFold(st.Value, x.tx.db.kind.SearchPath()[0]) {
			// The current database is the server's, not the connection's,
			// so a script that switches to another would run against the
			// wrong one.
			return nil, x.unsupported("USE of a database other than " + x.tx.db.kind.SearchPath()[0])
		}
		return &sqlResult{}, nil
	case *sqlir.SetConstraintsStmt:
		return &sqlResult{}, x.setConstraints(st)
	case *sqlir.SelectStmt:
		return x.execSelect(st)
	case *sqlir.InsertStmt:
		res, err := x.execInsert(st)
		if de, ok := errors.AsType[*sqlir.DBError](err); ok && x.tx.db.kind.InnoDB() && st.OnConflict != nil && st.OnConflict.DoNothing {
			switch de.Kind {
			case sqlir.UniqueViolation, sqlir.Deadlock, sqlir.LockWaitTimeout, sqlir.LockNotAvailable:
			default:
				// INSERT IGNORE turns this error into a warning and stores
				// the row coerced or skips it, by rules detest does not
				// follow, where it would otherwise fail the statement.
				return nil, x.unsupported("INSERT IGNORE of a row MySQL would store or skip with a warning")
			}
		}
		return res, err
	case *sqlir.UpdateStmt:
		return x.execUpdate(st)
	case *sqlir.DeleteStmt:
		if !st.Truncate {
			return x.execDelete(st)
		}
		if x.tx.block && !x.tx.checking {
			// MySQL's TRUNCATE commits the transaction first, which detest
			// does not do, so running it as a DELETE would let a rollback
			// undo it.
			return nil, x.unsupported("TRUNCATE inside a transaction")
		}
		res, err := x.execDelete(st)
		if err == nil {
			x.tx.db.moveAutoInc(x.tx.db.resolve(st.Table), "")
		}
		return res, err
	case *sqlir.SchemaStmt:
		if err := x.ddlInTransaction(); err != nil {
			return nil, err
		}
		for _, ch := range st.Changes {
			// Postgres refuses to drop a column a generated column depends
			// on, or drops both with CASCADE. The generated columns are the
			// table's and those the same statement adds.
			if exists := tx.db.defs[tx.db.resolve(ch.Table)] != nil; ch.Create == exists {
				// A CREATE of a table that exists is a no-op or a duplicate,
				// and a change to one that does not is an undefined table,
				// which applySchema reports.
				continue
			}
			gens := map[string]tableCheck{}
			var order []string
			if def := tx.db.defs[tx.db.resolve(ch.Table)]; def != nil {
				for _, col := range def.columns {
					if g := def.generated[col]; g != nil {
						gens[col] = *g
						order = append(order, col)
					}
				}
			}
			for _, c := range ch.Columns {
				if c.Generated != nil {
					gens[c.Name] = tableCheck{CheckDef: sqlir.CheckDef{Name: c.Name, Expr: c.Generated}}
					order = append(order, c.Name)
				}
				// SET or DROP DEFAULT and ADD IDENTITY name a column without
				// a type; Postgres refuses them on a generated column.
				if _, generated := gens[c.Name]; generated && !ch.Create && c.Generated == nil && c.Type == "" && !c.TypeOnly {
					return nil, x.unsupported(fmt.Sprintf("a default or identity on generated column %q", c.Name))
				}
			}
			// The columns the table has once the statement is applied.
			cols := map[string]bool{}
			if def := tx.db.defs[tx.db.resolve(ch.Table)]; def != nil && !ch.Create {
				for _, c := range def.columns {
					cols[c] = true
				}
			}
			for _, c := range ch.Columns {
				cols[c.Name] = true
			}
			for _, c := range ch.DropColumns {
				delete(cols, c)
			}
			for _, col := range order { // in declaration order, for a stable error
				if slices.Contains(ch.DropColumns, col) {
					continue
				}
				for _, dep := range gens[col].columns() {
					if !cols[dep] && !slices.Contains(ch.DropColumns, dep) {
						// Postgres refuses it; detest would read it as NULL.
						return nil, x.unsupported(fmt.Sprintf("generated column %q, which refers to column %q the table does not have", col, dep))
					}
					if _, generated := gens[dep]; generated {
						// Postgres refuses it; detest would compute it in
						// declaration order.
						return nil, x.unsupported(fmt.Sprintf("generated column %q, which refers to generated column %q", col, dep))
					}
					if slices.Contains(ch.DropColumns, dep) {
						return nil, x.unsupported(fmt.Sprintf("dropping column %q, which generated column %q depends on", dep, col))
					}
				}
			}
			if ch.Create || len(tx.selectNoYield(ch.Table, nil)) == 0 {
				continue
			}
			// Postgres computes the new column for the rows already there,
			// which detest does not do.
			if slices.ContainsFunc(ch.Columns, func(c sqlir.ColumnDef) bool { return c.Generated != nil }) {
				return nil, x.unsupported("adding a generated column to a table with rows")
			}
		}
		if err := tx.db.applySchema(st, tx); err != nil {
			return nil, err
		}
		return &sqlResult{}, nil
	}
	return nil, unsupported(fmt.Sprintf("%T", stmt), x.query)
}

// execCreateTableAs declares the table, without keys, and fills it with the
// query's rows.
func (x *sqlExec) execCreateTableAs(st *sqlir.CreateTableAsStmt) (*sqlResult, error) {
	selCols, rows, err := x.evalSelect(st.Select, nil)
	if err != nil {
		return nil, err
	}
	cols := selCols
	if len(st.Columns) > 0 {
		cols = st.Columns
	}
	ch := sqlir.SchemaChange{Table: st.Table, Create: true, IfNotExists: st.IfNotExists}
	for _, c := range cols {
		ch.Columns = append(ch.Columns, sqlir.ColumnDef{Name: c})
	}
	if err := x.tx.db.applySchema(&sqlir.SchemaStmt{Changes: []sqlir.SchemaChange{ch}}, x.tx); err != nil {
		return nil, err
	}
	table := x.tx.db.resolve(st.Table)
	if st.Materialized {
		x.tx.db.matviews[table] = st
	}
	return x.fill(table, st, selCols, rows)
}

// execRefresh replaces a materialized view's rows with its query's.
func (x *sqlExec) execRefresh(st *sqlir.RefreshStmt) (*sqlResult, error) {
	table := x.tx.db.resolve(st.Table)
	mv := x.tx.db.matviews[table]
	if mv == nil {
		return nil, x.tx.db.kind.Error(sqlir.WrongObjectType, fmt.Sprintf("%q is not a materialized view", relname(table)), relname(table), "", "")
	}
	for _, r := range x.tx.selectNoYield(table, nil) {
		lk := lockKey{table, r.Key()}
		if err := x.tx.lock(lk); err != nil {
			return nil, err
		}
		delete(x.tx.writes, lk)
		x.tx.deleted[lk] = true
	}
	if st.NoData {
		return &sqlResult{}, nil
	}
	selCols, rows, err := x.evalSelect(mv.Select, nil)
	if err != nil {
		return nil, err
	}
	return x.fill(table, mv, selCols, rows)
}

// fill inserts the rows of a CREATE TABLE AS or materialized view query.
func (x *sqlExec) fill(table string, st *sqlir.CreateTableAsStmt, selCols []string, rows []Row) (*sqlResult, error) {
	cols := selCols
	if len(st.Columns) > 0 {
		cols = st.Columns
	}
	out := &sqlResult{}
	if st.NoData {
		return out, nil
	}
	for _, sr := range rows {
		row := Row{}
		for i, c := range cols {
			if i < len(selCols) {
				row[c] = sr[selCols[i]]
			}
		}
		if err := x.tx.db.assignKey(table, row); err != nil {
			return nil, err
		}
		lk := lockKey{table, row.Key()}
		if err := x.tx.lock(lk); err != nil {
			return nil, err
		}
		x.tx.writes[lk] = row
		out.affected++
	}
	return out, nil
}

func (x *sqlExec) unsupported(what string) error { return unsupported(what, x.query) }

// functionRows evaluates a set-returning function in FROM.
func (x *sqlExec) functionRows(t sqlir.TableRef, outer *env) ([]Row, error) {
	en := outer
	if en == nil {
		en = &env{}
	}
	args := make([]any, len(t.Func.Args))
	for i, a := range t.Func.Args {
		v, err := x.eval(a, en)
		if err != nil {
			return nil, err
		}
		args[i] = derefValue(v)
	}
	col := t.Func.Name
	switch {
	case len(t.Columns) > 0:
		col = t.Columns[0]
	case t.Alias != "":
		col = t.Alias
	}
	ordCol := "ordinality"
	if len(t.Columns) > 1 {
		ordCol = t.Columns[1]
	}
	var vals []any
	switch t.Func.Name {
	case "generate_series":
		if len(args) < 2 || len(args) > 3 {
			return nil, x.unsupported("generate_series with other than two or three arguments")
		}
		for _, a := range args {
			if a == nil {
				return nil, nil // generate_series with a NULL bound returns no rows
			}
		}
		start, ok1 := toInt64(args[0])
		stop, ok2 := toInt64(args[1])
		step := int64(1)
		ok3 := true
		if len(args) == 3 {
			step, ok3 = toInt64(args[2])
		}
		if !ok1 || !ok2 || !ok3 || step == 0 {
			return nil, x.unsupported("generate_series over other than integers")
		}
		for v := start; (step > 0 && v <= stop) || (step < 0 && v >= stop); v += step {
			vals = append(vals, v)
		}
	default:
		return nil, x.unsupported("set-returning function " + t.Func.Name + " in FROM")
	}
	rows := make([]Row, len(vals))
	for i, v := range vals {
		rows[i] = Row{col: v}
		if t.Ordinality {
			rows[i][ordCol] = int64(i + 1)
		}
	}
	return rows, nil
}

// --- SELECT ---

func (x *sqlExec) execSelect(sel *sqlir.SelectStmt) (*sqlResult, error) {
	if x.tx.db.kind.InnoDB() {
		if sel.Lock == nil && x.tx.iso == Serializable && x.tx.block {
			// InnoDB's Serializable turns the plain reads of a transaction
			// block into locking reads in share mode.
			locked := *sel
			locked.Lock = &sqlir.LockClause{Strength: "share"}
			sel = &locked
		}
		x.consistent = sel.Lock == nil && x.tx.consistent()
	}
	x.inSelect, x.selectStmt = true, true
	x.tx.yieldf("%s: %s", x.tx.db.name, lazyString(func() string { return x.summarize(sel) }))
	cols, rows, err := x.evalSelect(sel, nil)
	if err != nil {
		return nil, err
	}
	out := &sqlResult{cols: cols, affected: int64(len(rows))}
	for _, row := range rows {
		vals := make([]driver.Value, len(cols))
		for i, c := range cols {
			vals[i] = toDriverValue(row[c])
		}
		out.rows = append(out.rows, vals)
	}
	return out, nil
}

func (x *sqlExec) summarize(sel *sqlir.SelectStmt) string {
	table := "(no table)"
	if sel.From != nil {
		table = x.tableLabel(*sel.From)
		for _, j := range sel.Joins {
			table += " join " + x.tableLabel(j.Table)
		}
	}
	desc := fmt.Sprintf("select %s where %s", table, x.exprString(sel.Where))
	if sel.Lock != nil {
		desc += " for update"
		if sel.Lock.SkipLocked {
			desc += " skip locked"
		}
	}
	return desc
}

func (x *sqlExec) tableLabel(t sqlir.TableRef) string {
	if t.Sub != nil {
		return "(subquery)"
	}
	return t.Name
}

func (x *sqlExec) withCTEs(ctes []sqlir.CTE, outer *env) error {
	for _, cte := range ctes {
		cols, rows, err := x.evalSelect(cte.Select, outer)
		if err != nil {
			return err
		}
		_ = cols
		x.ctes[cte.Name] = rows
	}
	return nil
}

// tableRows returns the rows of a FROM item with the alias they are known by.
func (x *sqlExec) tableRows(t sqlir.TableRef, outer *env) (alias string, rows []Row, base bool, err error) {
	alias = t.Alias
	if t.Sub != nil {
		cols, rows, err := x.evalSelect(t.Sub, outer)
		if err == nil && len(t.Columns) > 0 {
			rows = renameColumns(rows, cols, t.Columns) // AS alias(a, b)
		}
		return alias, rows, false, err
	}
	if t.Func != nil {
		if alias == "" {
			alias = t.Func.Name
		}
		rows, err := x.functionRows(t, outer)
		return alias, rows, false, err
	}
	if alias == "" {
		alias = relname(t.Name)
	}
	if rows, ok := x.ctes[t.Name]; ok {
		return alias, rows, false, nil
	}
	if err := x.tx.db.checkTable(x.tx.db.resolve(t.Name)); err != nil {
		return "", nil, false, err
	}
	if v := x.tx.db.views[x.tx.db.resolve(t.Name)]; v != nil {
		cols, rows, err := x.evalSelect(v.View, nil)
		if err == nil && len(v.ViewColumns) > 0 {
			rows = renameColumns(rows, cols, v.ViewColumns)
		}
		return alias, rows, false, err
	}
	if x.consistent {
		return alias, x.tx.snapshotRows(t.Name), true, nil
	}
	return alias, x.tx.selectNoYield(t.Name, nil), true, nil
}

// fromItems describes the FROM items of a query, for locking reads.
type fromItems struct {
	aliases  []string          // in FROM order, the first being the base item
	tables   map[string]string // alias -> table, for the items that are tables
	kinds    map[string]string // alias -> "subquery", "view", "function" or "cte" for the others
	nullable map[string]bool   // the right side of a LEFT JOIN
	lateral  bool              // some item depends on the rows before it
}

// fromItemsOf describes the FROM items of sel without evaluating them or its
// WITH queries, so a locking clause is checked before the query has any
// effect, such as advancing a sequence.
func (x *sqlExec) fromItemsOf(sel *sqlir.SelectStmt) fromItems {
	from := fromItems{tables: map[string]string{}, kinds: map[string]string{}, nullable: map[string]bool{}}
	if sel.From == nil {
		return from
	}
	items := []sqlir.TableRef{*sel.From}
	for _, j := range sel.Joins {
		items = append(items, j.Table)
	}
	for i, t := range items {
		alias := t.Alias
		switch {
		case alias != "":
		case t.Func != nil:
			alias = t.Func.Name
		default:
			alias = relname(t.Name)
		}
		from.aliases = append(from.aliases, alias)
		_, isCTE := x.ctes[t.Name]
		isCTE = isCTE || slices.ContainsFunc(sel.With, func(c sqlir.CTE) bool { return c.Name == t.Name })
		switch {
		case t.Sub != nil:
			from.kinds[alias] = "subquery"
		case t.Func != nil:
			from.kinds[alias] = "function"
		case isCTE:
			from.kinds[alias] = "cte"
		case x.tx.db.views[x.tx.db.resolve(t.Name)] != nil:
			from.kinds[alias] = "view"
		default:
			from.tables[alias] = x.tx.db.resolve(t.Name)
		}
		if i == 0 {
			continue
		}
		if t.Lateral || t.Func != nil {
			from.lateral = true
		}
		if sel.Joins[i-1].Kind == sqlir.LeftJoin {
			from.nullable[alias] = true
		}
	}
	return from
}

// scan produces the FROM rows of a query: the base table joined with each
// JOIN item by nested loops.
func (x *sqlExec) scan(sel *sqlir.SelectStmt, outer *env) ([]jrow, error) {
	if sel.From == nil {
		return []jrow{{by: map[string]Row{}, merged: Row{}}}, nil
	}
	alias, rows, isBase, err := x.tableRows(*sel.From, outer)
	if err != nil {
		return nil, err
	}
	var out []jrow
	for _, r := range rows {
		j := newJrow(alias, r)
		if !isBase {
			j.base = nil
		}
		out = append(out, j)
	}
	for _, join := range sel.Joins {
		jalias, jrows, _, err := x.tableRows(join.Table, outer)
		if err != nil {
			return nil, err
		}
		var next []jrow
		switch join.Kind {
		case sqlir.InnerJoin, sqlir.CrossJoin, sqlir.LeftJoin:
			for _, l := range out {
				if join.Table.Lateral {
					// A lateral item sees the row it joins to.
					if _, jrows, _, err = x.tableRows(join.Table, l.env(outer)); err != nil {
						return nil, err
					}
				}
				matched := false
				for _, r := range jrows {
					cand := l.with(jalias, r)
					ok := true
					if join.On != nil {
						if ok, err = x.evalBool(join.On, cand.env(outer)); err != nil {
							return nil, err
						}
					}
					if ok {
						matched = true
						next = append(next, cand)
					}
				}
				if !matched && join.Kind == sqlir.LeftJoin {
					next = append(next, l.with(jalias, nil))
				}
			}
		default:
			return nil, x.unsupported("right or full join")
		}
		out = next
	}
	return out, nil
}

func (x *sqlExec) order(keys []sqlir.OrderKey, rows []jrow, outer *env) error {
	if len(keys) == 0 {
		return nil
	}
	vals := make([][]any, len(rows))
	for i, r := range rows {
		vals[i] = make([]any, len(keys))
		for j, k := range keys {
			v, err := x.eval(k.Expr, r.env(outer))
			if err != nil {
				return x.unsupportedExpr(err, "in ORDER BY")
			}
			vals[i][j] = v
		}
	}
	idx := make([]int, len(rows))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return orderedBefore(keys, vals[idx[a]], vals[idx[b]]) })
	sorted := make([]jrow, len(rows))
	for i, j := range idx {
		sorted[i] = rows[j]
	}
	copy(rows, sorted)
	return nil
}

// offsetLimit returns the OFFSET and LIMIT evalSelect took for sel before
// running it, evaluating them if it has not.
func (x *sqlExec) offsetLimit(sel *sqlir.SelectStmt, outer *env) (offset, limit int, err error) {
	if b, ok := x.bounds[sel]; ok {
		return b[0], b[1], nil
	}
	return x.evalBounds(sel, outer)
}

// evalBounds evaluates OFFSET and LIMIT. limit is -1 without a LIMIT.
// Postgres refuses a negative one of either.
func (x *sqlExec) evalBounds(sel *sqlir.SelectStmt, outer *env) (offset, limit int, err error) {
	limit = -1
	// OFFSET first, as Postgres evaluates them.
	if sel.Offset != nil {
		if offset, err = x.evalBound(sel.Offset, "OFFSET", outer); err != nil {
			return 0, 0, err
		}
		offset = max(offset, 0) // OFFSET NULL is OFFSET 0
	}
	if sel.Limit != nil {
		if limit, err = x.evalBound(sel.Limit, "LIMIT", outer); err != nil {
			return 0, 0, err
		}
	}
	return offset, limit, nil
}

// evalBound evaluates an OFFSET or LIMIT, a bigint that may not be negative.
// -1 is NULL, which is no LIMIT. An integer stays one: a float cannot hold
// the largest bigint.
func (x *sqlExec) evalBound(e sqlir.Expr, what string, outer *env) (int, error) {
	if x.tx.db.kind.InnoDB() {
		return x.count(e, what, outer) // MySQL's rules for a count, NULL as 0
	}
	v, err := x.eval(e, &env{outer: outer})
	if err != nil {
		return 0, err
	}
	switch n := derefValue(v).(type) {
	case nil:
		return -1, nil
	case int64:
		if n < 0 {
			return 0, x.unsupported("a negative " + what)
		}
		return int(min(n, math.MaxInt)), nil // no slice is longer than MaxInt
	case string:
		// An untyped literal such as '2' reads as a bigint.
		i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		if err != nil {
			return 0, x.unsupported("an " + what + " that is not a bigint")
		}
		if i < 0 {
			return 0, x.unsupported("a negative " + what)
		}
		return int(min(i, math.MaxInt)), nil
	}
	f, ok := toFloat(derefValue(v))
	switch {
	case !ok || f != math.Trunc(f):
		// detest does not know how Postgres would coerce it to a bigint.
		return 0, x.unsupported("an " + what + " that is not a bigint")
	case f < 0:
		return 0, x.unsupported("a negative " + what)
	case math.IsNaN(f) || f >= math.MaxInt64:
		return 0, x.unsupported("an " + what + " out of the bigint range")
	case f >= math.MaxInt:
		return math.MaxInt, nil
	}
	return int(f), nil
}

func (x *sqlExec) project(sel *sqlir.SelectStmt, rows []jrow, outer *env) ([]string, []Row, error) {
	var cols []string
	starCols := func(t sqlir.Target) []string {
		set := map[string]bool{}
		for _, r := range rows {
			src := r.merged
			if t.Table != "" {
				src = r.by[t.Table]
			}
			for k := range src {
				if k != "_key" {
					set[k] = true
				}
			}
		}
		var all []string
		for k := range set {
			all = append(all, k)
		}
		sort.Strings(all)
		return all
	}
	for _, t := range sel.Targets {
		if t.Star {
			cols = append(cols, starCols(t)...)
			continue
		}
		cols = append(cols, targetName(t))
	}
	var out []Row
	for _, r := range rows {
		o := Row{}
		for _, t := range sel.Targets {
			if t.Star {
				src := r.merged
				if t.Table != "" {
					src = r.by[t.Table]
				}
				for k, v := range src {
					if k != "_key" {
						o[k] = v
					}
				}
				continue
			}
			v, err := x.eval(t.Expr, r.env(outer))
			if err != nil {
				return nil, nil, err
			}
			o[targetName(t)] = v
		}
		if !sel.Distinct || !seenDistinct(out, o, cols) {
			out = append(out, o)
		}
	}
	return cols, out, nil
}

type aggEnv struct {
	env  *env
	rows []jrow
	x    *sqlExec
}

func hasAggregate(e sqlir.Expr) bool {
	switch v := e.(type) {
	case nil:
		return false
	case *sqlir.FuncCall:
		switch v.Name {
		case "count", "sum", "min", "max", "avg":
			return true
		}
		if sqlir.OtherAggregates[v.Name] {
			return true
		}
		if slices.ContainsFunc(v.Args, hasAggregate) {
			return true
		}
	case *sqlir.BinaryExpr:
		return hasAggregate(v.L) || hasAggregate(v.R)
	case *sqlir.UnaryExpr:
		return hasAggregate(v.X)
	case *sqlir.Cast:
		return hasAggregate(v.X)
	case *sqlir.IsNull:
		return hasAggregate(v.X)
	case *sqlir.InExpr:
		return hasAggregate(v.X) || slices.ContainsFunc(v.List, hasAggregate)
	case *sqlir.RowExpr:
		return slices.ContainsFunc(v.Items, hasAggregate)
	case *sqlir.CaseExpr:
		if hasAggregate(v.Arg) {
			return true
		}
		for _, w := range v.Whens {
			if hasAggregate(w.When) || hasAggregate(w.Then) {
				return true
			}
		}
		return hasAggregate(v.Else)
	}
	return false
}

func (x *sqlExec) evalBoolAgg(e sqlir.Expr, g *aggEnv) (bool, error) {
	v, err := x.evalAgg(e, g)
	if err != nil {
		return false, err
	}
	b, _ := derefValue(v).(bool)
	return b, nil
}

// evalAgg evaluates an expression over a group: aggregates fold the group's
// rows, other expressions use the group's sample row.
func (x *sqlExec) evalAgg(e sqlir.Expr, g *aggEnv) (any, error) {
	switch v := e.(type) {
	case *sqlir.WindowFunc:
		return x.eval(v, g.env)
	case *sqlir.FuncCall:
		switch v.Name {
		case "count", "sum", "min", "max", "avg":
			if v.Star {
				return int64(len(g.rows)), nil
			}
			var vals []any
			seen := map[string]bool{}
			for _, r := range g.rows {
				val, err := x.eval(v.Args[0], r.env(g.env.outer))
				if err != nil {
					return nil, err
				}
				if derefValue(val) == nil {
					continue
				}
				if v.Distinct {
					k := fmt.Sprint(derefValue(val))
					if seen[k] {
						continue
					}
					seen[k] = true
				}
				vals = append(vals, x.aggOperand(v.Name, val))
			}
			return foldAggregate(v.Name, false, vals, len(g.rows)), nil
		}
		if sqlir.OtherAggregates[v.Name] {
			return nil, x.unsupported("aggregate " + v.Name)
		}
		if err := x.checkArity(v); err != nil {
			return nil, err
		}
		args := make([]any, len(v.Args))
		for i, a := range v.Args {
			val, err := x.evalAgg(a, g)
			if err != nil {
				return nil, err
			}
			args[i] = val
		}
		if v.Name == "nullif" {
			var err error
			if args[0], args[1], err = x.untypedPair(v.Args[0], args[0], v.Args[1], args[1]); err != nil {
				return nil, err
			}
		}
		return x.callFunc(v.Name, args)
	case *sqlir.BinaryExpr:
		l, err := x.evalAgg(v.L, g)
		if err != nil {
			return nil, err
		}
		r, err := x.evalAgg(v.R, g)
		if err != nil {
			return nil, err
		}
		switch v.Op {
		case "=", "<>", "!=", "<", "<=", ">", ">=":
			if l, r, err = x.untypedPair(v.L, l, v.R, r); err != nil {
				return nil, err
			}
		}
		return x.binary(v.Op, l, r)
	case *sqlir.Cast:
		val, err := x.evalAgg(v.X, g)
		if err != nil {
			return nil, err
		}
		return castValue(val, v.Type), nil
	}
	if hasAggregate(e) {
		// eval would take the aggregate for a function of one row.
		return nil, x.unsupported("an aggregate inside NOT, CASE, IS NULL, IN or a row")
	}
	return x.eval(e, g.env)
}

// --- INSERT ---

func (x *sqlExec) execInsert(ins *sqlir.InsertStmt) (*sqlResult, error) {
	table := x.tx.db.resolve(ins.Table)
	if err := x.tx.db.checkTable(table); err != nil {
		return nil, err
	}
	cols := ins.Columns
	for _, exprs := range ins.Rows {
		// Postgres refuses VALUES lists of different lengths before any of
		// them is evaluated.
		if len(exprs) != len(ins.Rows[0]) {
			return nil, x.tx.db.kind.Error(sqlir.SyntaxError, "VALUES lists must all be the same length", relname(ins.Table), "", "")
		}
	}
	if def := x.tx.db.defs[table]; len(cols) == 0 && def != nil {
		cols = def.columns // INSERT INTO t VALUES (...): the columns in table order
		if len(ins.Rows) > 0 && len(ins.Rows[0]) < len(cols) {
			cols = cols[:len(ins.Rows[0])] // or the first N, for N values
		}
	}
	if err := x.insertsGenerated(table, ins, cols); err != nil {
		return nil, err
	}
	var rows []Row
	switch {
	case ins.Select != nil:
		src := ins.Select
		if x.tx.db.kind.InnoDB() && src.Lock == nil && (x.tx.iso == RepeatableRead || x.tx.iso == Serializable) {
			if src.SetOp != "" || len(src.With) > 0 {
				// The shared locks would cover the top query block only.
				return nil, x.unsupported("INSERT ... SELECT from a set operation or a CTE")
			}
			// InnoDB's INSERT ... SELECT reads its source with shared
			// next-key locks, so a concurrent write to the rows it copies
			// waits.
			locked := *src
			locked.Lock = &sqlir.LockClause{Strength: "share"}
			src = &locked
		}
		// Its nested query blocks read by their own locking clauses, as a
		// SELECT's do.
		x.inSelect = true
		scols, srows, err := x.evalSelect(src, nil)
		if err != nil {
			return nil, err
		}
		for _, sr := range srows {
			row := Row{}
			for i, c := range cols {
				if i < len(scols) {
					row[c] = sr[scols[i]]
				}
			}
			rows = append(rows, row)
		}
	default:
		for _, exprs := range ins.Rows {
			if len(exprs) != len(cols) {
				msg := "INSERT has more expressions than target columns"
				if len(exprs) < len(cols) {
					msg = "INSERT has more target columns than expressions"
				}
				return nil, x.tx.db.kind.Error(sqlir.SyntaxError, msg, relname(ins.Table), "", "")
			}
			row := Row{}
			for i, e := range exprs {
				v, err := x.eval(e, &env{})
				if err != nil {
					return nil, x.unsupportedExpr(err, "in VALUES")
				}
				if _, isDefault := e.(*sqlir.Default); isDefault {
					continue
				}
				row[cols[i]] = v
			}
			rows = append(rows, row)
		}
	}
	alias := ins.Alias
	if alias == "" {
		alias = relname(ins.Table)
	}
	all := cols // RETURNING * is every column of the table, not only those written
	if def := x.tx.db.defs[table]; def != nil {
		all = def.columns
	}
	out := &sqlResult{cols: x.returningCols(ins.Returning, all)}
	type autoID struct {
		id        int64
		generated bool
	}
	ids := make([]autoID, len(rows))
	// InnoDB reserves the values of an INSERT ... VALUES, whose rows it
	// knows, before inserting any, so they are consecutive however other
	// inserts interleave, and a row an error stops still used its value. An
	// INSERT ... SELECT takes one at a time.
	reserved := x.tx.db.kind.InnoDB() && ins.Select == nil
	if reserved {
		for i, row := range rows {
			ids[i].id, ids[i].generated = x.autoIncrement(table, row)
		}
	}
	for i, row := range rows {
		// The counter moves even when the row then collides and is not
		// inserted, as MySQL's does; only an inserted row's value is reported.
		if !reserved {
			ids[i].id, ids[i].generated = x.autoIncrement(table, row)
		}
		generatedID, generated := ids[i].id, ids[i].generated
		inserted := func() {
			if generated && !out.hasLastID {
				out.lastID, out.hasLastID = generatedID, true
			}
		}
		if err := x.applyDefaults(table, row); err != nil {
			return nil, err
		}
		if err := x.checkRow(table, row); err != nil {
			return nil, err
		}
		if err := x.tx.db.assignKey(table, row); err != nil {
			return nil, err
		}
		if x.tx.db.ignored[table] {
			out.affected++
			inserted()
			if err := x.appendReturning(out, ins.Returning, row); err != nil {
				return nil, err
			}
			continue
		}
		x.tx.yieldf("%s: insert %s %s", x.tx.db.name, ins.Table, row)
		var existing, cur Row
		var lk lockKey
		for ins.OnConflict != nil {
			var err error
			if existing, err = x.findConflict(table, ins.OnConflict.Columns, row); err != nil {
				return nil, err
			}
			if existing == nil {
				break
			}
			// The existing row is locked and re-read, following it if it
			// moved. If it went meanwhile, the insert is tried again, as
			// Postgres does. DO NOTHING and INSERT IGNORE wait as well, with
			// the weakest lock that waits for a writer.
			mode := x.tx.db.updateLock(table, assignedColumns(ins.OnConflict.Set))
			if ins.OnConflict.DoNothing {
				mode = lockKeyShare
				if x.tx.db.kind.InnoDB() {
					mode = lockShare // InnoDB's duplicate check takes a shared lock
				}
			}
			key, c, ok, err := x.tx.lockLatest(table, existing.Key(), func(lk lockKey) error { return x.tx.lockMode(lk, mode) })
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			// The row may no longer conflict after the wait, when its
			// arbiter value changed: then the insert is tried again too.
			again, err := x.findConflict(table, ins.OnConflict.Columns, row)
			if err != nil {
				return nil, err
			}
			if again != nil && again.Key() == key {
				cur, lk = c, lockKey{table, key}
				break
			}
		}
		if existing != nil {
			if ins.OnConflict.DoNothing {
				if x.tx.p != nil {
					x.tx.p.r.note(x.tx.p, "on conflict do nothing: row skipped")
				}
				continue
			}
			// DO UPDATE: apply SET to the locked row with EXCLUDED bound to
			// the proposed row.
			e := &env{tables: map[string]Row{alias: cur}, merged: cur, excluded: row}
			if ins.OnConflict.Where != nil {
				ok, err := x.evalBool(ins.OnConflict.Where, e)
				if err != nil {
					return nil, err
				}
				if !ok {
					if x.tx.p != nil {
						x.tx.p.r.note(x.tx.p, "on conflict do update: WHERE false, row skipped")
					}
					continue
				}
			}
			updated := cur.clone()
			if x.tx.db.kind.InnoDB() {
				// ON DUPLICATE KEY UPDATE assigns left to right, as MySQL's
				// UPDATE does.
				e = &env{tables: map[string]Row{alias: updated}, merged: updated, excluded: row}
			}
			for _, a := range ins.OnConflict.Set {
				v, err := x.assignedValue(table, a, e, "in ON CONFLICT DO UPDATE SET")
				if err != nil {
					return nil, err
				}
				updated[a.Column] = v
			}
			if err := x.touchOnUpdate(table, cur, updated, ins.OnConflict.Set); err != nil {
				return nil, err
			}

			if err := x.checkRow(table, updated); err != nil {
				return nil, err
			}
			if err := x.checkUniques(table, updated, lk.key, cur); err != nil {
				return nil, err
			}
			if err := x.checkParents(table, updated, cur); err != nil {
				return nil, err
			}
			if err := x.onParentUpdate(table, cur, updated); err != nil {
				return nil, err
			}
			lk, err := x.rekey(table, lk, updated)
			if err != nil {
				return nil, err
			}
			// Last before the write, as the checks above may wait while
			// another transaction takes a gap lock the new value falls into.
			if err := x.tx.moveIntention(table, updated, cur); err != nil {
				return nil, err
			}
			x.tx.writes[lk] = updated
			if x.tx.p != nil {
				x.tx.p.r.note(x.tx.p, "on conflict do update: %s", updated)
			}
			switch {
			case !x.tx.db.kind.InnoDB():
				out.affected++
			case !sameRow(cur, updated):
				// MySQL counts an ON DUPLICATE KEY UPDATE that changed the
				// row twice, and one that did not as none.
				out.affected += 2
			}
			if err := x.appendReturning(out, ins.Returning, updated); err != nil {
				return nil, err
			}
			continue
		}
		if err := x.tx.insertIntention(table, row); err != nil {
			return nil, err
		}
		lk = lockKey{table, row.Key()}
		noPK := false
		if def := x.tx.db.defs[table]; def != nil && len(def.pk) == 0 {
			noPK = true
		}
		// Waiting on a key made from the row's values would block where
		// Postgres, with no key to wait on, takes both rows. A row a unique
		// index takes is waited on there too, on the same transaction, so
		// it waits here as Postgres does, but only when the key is made
		// from every value: a key made from an id column alone can match a
		// row with other unique values, which Postgres would not wait on.
		_, keyedByID := row["id"]
		if noPK && x.tx.heldByOther(lk, lockUpdate) && (keyedByID || !x.inUniqueIndex(table, row)) {
			return nil, x.unsupported("a row equal to one another transaction is writing in a table without a primary key")
		}
		if !noPK {
			dup, err := x.sharedDuplicate(table, lk.key, "")
			if err != nil {
				return nil, err
			}
			if dup != nil {
				return nil, x.tx.db.duplicateKey(table, x.tx.db.pkConstraint(table))
			}
		}
		if err := x.tx.lock(lk); err != nil {
			return nil, err
		}
		// The row lock may have waited, and a gap lock taken meanwhile
		// covers the row as much as one taken before.
		if err := x.tx.insertIntention(table, row); err != nil {
			return nil, err
		}
		if _, exists := x.tx.view(table, row.Key()); exists {
			// A table without a primary key tells its rows apart by their
			// values, as detest has no row identity of its own, so a second
			// equal row is refused rather than reported as a duplicate key
			// Postgres would not raise, unless a unique constraint of the
			// table rejects it, as Postgres does.
			if noPK {
				if err := x.checkUniques(table, row, "", nil); err != nil {
					return nil, err
				}
				return nil, x.unsupported("a row equal to one already in a table without a primary key")
			}
			return nil, x.tx.db.duplicateKey(table, x.tx.db.pkConstraint(table))
		}
		if err := x.checkUniques(table, row, "", nil); err != nil {
			return nil, err
		}
		if err := x.checkParents(table, row, nil); err != nil {
			return nil, err
		}
		// Last before the write, as the duplicate checks above may wait
		// while another transaction takes a gap lock the row falls into.
		if err := x.tx.insertIntention(table, row); err != nil {
			return nil, err
		}
		delete(x.tx.deleted, lk)
		x.tx.writes[lk] = row
		out.affected++
		inserted()
		if err := x.appendReturning(out, ins.Returning, row); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// findConflict returns the existing row the proposed row collides with: on
// the conflict columns when given, otherwise on the key.
func (x *sqlExec) findConflict(table string, cols []string, row Row) (Row, error) {
	if x.tx.db.defs[table] != nil {
		targets, err := x.conflictTargets(table, cols)
		if err != nil {
			return nil, err
		}
		for i := range targets {
			ex, err := x.claimUnique(table, &targets[i], row, "")
			if err != nil || ex != nil {
				return ex, err
			}
		}
		return nil, nil
	}
	// Without a declared schema: the row's key, or a scan on the target columns.
	if len(cols) == 0 {
		if ex, ok := x.tx.view(table, row.Key()); ok {
			return ex, nil
		}
		return nil, nil
	}
	for _, ex := range x.tx.selectNoYield(table, nil) {
		same := true
		for _, c := range cols {
			if !sameValue(ex[c], row[c]) {
				same = false
				break
			}
		}
		if same {
			return ex, nil
		}
	}
	return nil, nil
}

func (x *sqlExec) returningCols(ret []sqlir.Target, all []string) []string {
	var cols []string
	for _, t := range ret {
		if t.Star {
			cols = append(cols, all...)
			continue
		}
		cols = append(cols, targetName(t))
	}
	return cols
}

func (x *sqlExec) appendReturning(out *sqlResult, ret []sqlir.Target, row Row) error {
	if len(ret) == 0 {
		return nil
	}
	vals := make([]driver.Value, 0, len(out.cols))
	e := &env{merged: row}
	for _, t := range ret {
		if t.Star {
			for _, c := range out.cols {
				vals = append(vals, toDriverValue(row[c]))
			}
			break
		}
		v, err := x.eval(t.Expr, e)
		if err != nil {
			return x.unsupportedExpr(err, "in RETURNING")
		}
		vals = append(vals, toDriverValue(v))
	}
	out.rows = append(out.rows, vals)
	return nil
}

// --- UPDATE / DELETE ---

// writeCandidates pairs each row of the target table with the FROM/USING rows
// that satisfy WHERE (or just the row when there are none).
func (x *sqlExec) writeCandidates(table, alias string, extra []sqlir.TableRef, where sqlir.Expr) ([]jrow, error) {
	if err := x.tx.db.checkTable(x.tx.db.resolve(table)); err != nil {
		return nil, err
	}
	if alias == "" {
		alias = relname(table)
	}
	var rows []jrow
	for _, r := range x.tx.selectNoYield(table, nil) {
		rows = append(rows, newJrow(alias, r))
	}
	for _, t := range extra {
		ealias, erows, _, err := x.tableRows(t, nil)
		if err != nil {
			return nil, err
		}
		var next []jrow
		for _, l := range rows {
			for _, r := range erows {
				next = append(next, l.with(ealias, r))
			}
		}
		rows = next
	}
	if where == nil {
		return rows, nil
	}
	var kept []jrow
	for _, r := range rows {
		ok, err := x.evalBool(where, r.env(nil))
		if err != nil {
			return nil, err
		}
		if ok {
			kept = append(kept, r)
		}
	}
	return kept, nil
}

// lockJoinedReads takes the shared locks InnoDB's multi-table UPDATE takes on
// the rows it reads from the tables joined to the target, for one row it
// updates, and returns the row with the joined rows as the locks left them.
// ok is false when one of them is gone. As for a locking read over a join,
// the joined tables get row locks only. Postgres's UPDATE ... FROM reads them
// without locks.
func (x *sqlExec) lockJoinedReads(from []sqlir.TableRef, c jrow) (jrow, bool, error) {
	if !x.tx.db.kind.InnoDB() {
		return c, true, nil
	}
	by := maps.Clone(c.by)
	for _, t := range from {
		if t.Name == "" || x.isCTE(t.Name) || x.tx.db.views[x.tx.db.resolve(t.Name)] != nil {
			continue
		}
		alias := t.Alias
		if alias == "" {
			alias = relname(t.Name)
		}
		if row := c.by[alias]; row != nil {
			table := x.tx.db.resolve(t.Name)
			if err := x.tx.lockMode(lockKey{table, row.Key()}, lockShare); err != nil {
				return c, false, err
			}
			cur, ok := x.tx.view(table, row.Key())
			if !ok {
				return c, false, nil
			}
			by[alias] = cur
		}
	}
	// The target's rebind rebuilds the merged columns from these.
	return jrow{by: by, merged: c.merged, base: c.base}, true, nil
}

// writeLimit orders the candidates of an UPDATE or DELETE by MySQL's ORDER BY
// and returns its LIMIT, or -1 without one. The limit counts the rows written,
// which a candidate re-checked after a lock wait may no longer be.
func (x *sqlExec) writeLimit(cands []jrow, order []sqlir.OrderKey, limit sqlir.Expr) (int, error) {
	if err := x.order(order, cands, nil); err != nil {
		return 0, err
	}
	if limit == nil {
		return -1, nil
	}
	return x.count(limit, "LIMIT", nil)
}

func (x *sqlExec) execUpdate(up *sqlir.UpdateStmt) (*sqlResult, error) {
	table := x.tx.db.resolve(up.Table)
	if x.tx.db.ignored[table] {
		return &sqlResult{cols: x.returningCols(up.Returning, nil)}, nil // nothing to update
	}
	alias := up.Alias
	if alias == "" {
		alias = relname(up.Table)
	}
	if def := x.tx.db.defs[table]; def != nil && len(up.From) > 0 && x.tx.db.kind.InnoDB() {
		for _, a := range up.Set {
			if !slices.Contains(def.columns, a.Column) {
				// MySQL updates the joined table that has the column, where
				// detest writes the first table only.
				return nil, x.unsupported("UPDATE of a column of another table than the first")
			}
		}
	}
	// Validate the predicate and preview SET for the trace.
	if err := x.writesGenerated(table, assignedColumns(up.Set), assignedValues(up.Set)); err != nil {
		return nil, err // before WHERE is evaluated, which may have effects
	}
	if _, err := x.writeCandidates(up.Table, up.Alias, nil, up.Where); err != nil && len(up.From) == 0 {
		return nil, err
	}
	preview := lazyString(func() string {
		row := Row{}
		for _, a := range up.Set {
			v, err := x.eval(a.Value, &env{})
			if err != nil {
				v = sqlir.Unknown
			}
			row[a.Column] = v
		}
		return row.String()
	})
	x.tx.yieldf("%s: update %s set %s where %s", x.tx.db.name, up.Table, preview, lazyString(func() string { return x.exprString(up.Where) }))
	var stop *scanStop
	if len(up.From) == 0 {
		var err error
		if stop, err = x.stopAt(up.OrderBy, up.Limit, nil); err != nil {
			return nil, err
		}
	}
	if err := x.nextKeyLocks(table, alias, up.Where, lockUpdate, nil, stop); err != nil {
		return nil, err
	}
	cands, err := x.writeCandidates(up.Table, up.Alias, up.From, up.Where)
	if err != nil {
		return nil, err
	}
	if err := x.inScanOrder(table, alias, up.Where, stop, cands); err != nil {
		return nil, err
	}
	limit, err := x.writeLimit(cands, up.OrderBy, up.Limit)
	if err != nil {
		return nil, err
	}
	out := &sqlResult{cols: x.returningCols(up.Returning, nil)}
	// MySQL's single-table UPDATE assigns left to right, each assignment
	// seeing the ones before it, where Postgres evaluates them all against the
	// old row.
	sequential := x.tx.db.kind.InnoDB() && len(up.From) == 0
	done := map[string]bool{}
	var matched int64 // LIMIT counts the rows matched, changed or not
	for _, c := range cands {
		key := c.base.Key()
		if done[key] {
			continue
		}
		if limit >= 0 && matched >= int64(limit) {
			break
		}
		c, joined, err := x.lockJoinedReads(up.From, c)
		if err != nil {
			return nil, err
		}
		if !joined {
			continue // a joined row it waited for is gone
		}
		mode := x.tx.db.updateLock(table, assignedColumns(up.Set))
		key, cur, ok, err := x.tx.lockLatest(table, key, func(lk lockKey) error { return x.tx.lockMode(lk, mode) })
		if err != nil {
			return nil, err
		}
		if !ok || done[key] {
			continue
		}
		lk := lockKey{table, key}
		// Re-evaluate the predicate on the version visible after the lock, as
		// Postgres Read Committed does.
		c2 := c.rebind(alias, cur)
		if up.Where != nil {
			ok, err := x.evalBool(up.Where, c2.env(nil))
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		done[key] = true
		updated := cur.clone()
		for _, a := range up.Set {
			v, err := x.assignedValue(table, a, c2.env(nil), "in SET")
			if err != nil {
				return nil, err
			}
			updated[a.Column] = v
			if sequential {
				c2 = c2.rebind(alias, updated)
			}
		}
		if err := x.touchOnUpdate(table, cur, updated, up.Set); err != nil {
			return nil, err
		}

		if err := x.checkRow(table, updated); err != nil {
			return nil, err
		}
		if err := x.checkUniques(table, updated, key, cur); err != nil {
			return nil, err
		}
		if err := x.checkParents(table, updated, cur); err != nil {
			return nil, err
		}
		if err := x.onParentUpdate(table, cur, updated); err != nil {
			return nil, err
		}
		lk, err = x.rekey(table, lk, updated)
		if err != nil {
			return nil, err
		}
		// Last before the write, as the checks above may wait while another
		// transaction takes a gap lock the new value falls into.
		if err := x.tx.moveIntention(table, updated, cur); err != nil {
			return nil, err
		}
		x.tx.writes[lk] = updated
		matched++
		if !x.tx.db.kind.InnoDB() || !sameRow(cur, updated) {
			// MySQL counts the rows an UPDATE changed, as go-sql-driver
			// reports them without clientFoundRows.
			out.affected++
		}
		if err := x.appendReturning(out, up.Returning, updated); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// touchOnUpdate sets the ON UPDATE CURRENT_TIMESTAMP columns of an update
// of cur to updated that changes the row and does not set them itself.
func (x *sqlExec) touchOnUpdate(table string, cur, updated Row, set []sqlir.Assignment) error {
	def := x.tx.db.defs[table]
	if def == nil || len(def.onUpdate) == 0 {
		return nil
	}
	// The row changes by what it stores, after conversion: 1.2 into an
	// integer column holding 1 changes nothing.
	stored := updated.clone()
	if err := x.checkTypes(table, stored); err != nil {
		return err
	}
	if sameRow(cur, stored) {
		return nil
	}
	for col, e := range def.onUpdate {
		if slices.ContainsFunc(set, func(a sqlir.Assignment) bool { return a.Column == col }) {
			continue
		}
		v, err := x.eval(e, &env{})
		if err != nil {
			return err
		}
		updated[col] = v
	}
	return nil
}

func (x *sqlExec) execDelete(del *sqlir.DeleteStmt) (*sqlResult, error) {
	if x.tx.db.isIgnored(del.Table) {
		return &sqlResult{cols: x.returningCols(del.Returning, nil)}, nil // nothing to delete
	}
	x.tx.yieldf("%s: delete %s where %s", x.tx.db.name, del.Table, lazyString(func() string { return x.exprString(del.Where) }))
	delAlias := del.Alias
	if delAlias == "" {
		delAlias = relname(del.Table)
	}
	var stop *scanStop
	if len(del.Using) == 0 {
		var err error
		if stop, err = x.stopAt(del.OrderBy, del.Limit, nil); err != nil {
			return nil, err
		}
	}
	if err := x.nextKeyLocks(x.tx.db.resolve(del.Table), delAlias, del.Where, lockUpdate, nil, stop); err != nil {
		return nil, err
	}
	cands, err := x.writeCandidates(del.Table, del.Alias, del.Using, del.Where)
	if err != nil {
		return nil, err
	}
	if err := x.inScanOrder(x.tx.db.resolve(del.Table), delAlias, del.Where, stop, cands); err != nil {
		return nil, err
	}
	limit, err := x.writeLimit(cands, del.OrderBy, del.Limit)
	if err != nil {
		return nil, err
	}
	table := x.tx.db.resolve(del.Table)
	alias := del.Alias
	if alias == "" {
		alias = relname(del.Table)
	}
	out := &sqlResult{cols: x.returningCols(del.Returning, nil)}
	done := map[string]bool{}
	for _, c := range cands {
		key := c.base.Key()
		if done[key] {
			continue
		}
		if limit >= 0 && out.affected >= int64(limit) {
			break
		}
		key, cur, ok, err := x.tx.lockLatest(table, key, x.tx.lock)
		if err != nil {
			return nil, err
		}
		if !ok || done[key] {
			continue
		}
		lk := lockKey{table, key}
		if del.Where != nil {
			c2 := c.rebind(alias, cur)
			ok, err := x.evalBool(del.Where, c2.env(nil))
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		done[key] = true
		delete(x.tx.writes, lk)
		x.tx.deleted[lk] = true
		if err := x.onParentDelete(table, cur); err != nil {
			return nil, err
		}
		out.affected++
		if err := x.appendReturning(out, del.Returning, cur); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// --- expressions ---

type errUnknownExpr struct{ what string }

func (e errUnknownExpr) Error() string { return "detest: cannot evaluate SQL expression: " + e.what }

// inUniqueIndex reports whether row has an entry in one of the table's
// unique indexes, so that an equal row is decided by that index.
func (x *sqlExec) inUniqueIndex(table string, row Row) bool {
	def := x.tx.db.defs[table]
	if def == nil {
		return false
	}
	for i := range def.uniques {
		if _, ok, err := x.uniqueValues(table, &def.uniques[i], row); err == nil && ok {
			return true
		}
	}
	return false
}

// unsupportedExpr turns an expression detest cannot evaluate into
// ErrUnsupportedSQL, naming where in the statement it stood. A written value
// or a sort key the application observes cannot be stood in for: a
// placeholder value would be read back as the column's, and a dropped sort
// key would return other rows under LIMIT. Any other error passes through.
func (x *sqlExec) unsupportedExpr(err error, where string) error {
	if u, ok := errors.AsType[errUnknownExpr](err); ok {
		return x.unsupported("an expression " + where + " detest cannot evaluate (" + u.what + ")")
	}
	return err
}

func (x *sqlExec) evalBool(e sqlir.Expr, en *env) (bool, error) {
	v, err := x.eval(e, en)
	if err != nil {
		return false, err
	}
	b, _ := derefValue(v).(bool)
	return b, nil
}

func (x *sqlExec) eval(e sqlir.Expr, en *env) (any, error) {
	switch v := e.(type) {
	case nil:
		return true, nil
	case *sqlir.ColumnRef:
		val, _ := en.lookup(v.Table, v.Column)
		return val, nil
	case *sqlir.Param:
		if v.Index < 0 || v.Index >= len(x.args) {
			return nil, x.tx.db.kind.Error(sqlir.UndefinedParameter, fmt.Sprintf("there is no parameter $%d", v.Index+1), "", "", "")
		}
		return x.args[v.Index], nil
	case *sqlir.Const:
		return v.Value, nil
	case *sqlir.WindowFunc:
		for cur := en; cur != nil; cur = cur.outer {
			if val, ok := cur.win[v]; ok {
				return val, nil
			}
		}
		return nil, x.unsupported("window function outside the select list, ORDER BY or DISTINCT ON")
	case *sqlir.Default:
		return nil, nil
	case *sqlir.Unconverted:
		return nil, errUnknownExpr{"an expression detest could not convert"}
	case *sqlir.Cast:
		val, err := x.eval(v.X, en)
		if err != nil {
			return nil, err
		}
		return castValue(val, v.Type), nil
	case *sqlir.UnaryExpr:
		val, err := x.eval(v.X, en)
		if err != nil {
			return nil, err
		}
		switch v.Op {
		case "NOT":
			b, ok := derefValue(val).(bool)
			if !ok {
				return nil, nil // NOT NULL is NULL
			}
			return !b, nil
		case "-":
			if f, ok := toFloat(derefValue(val)); ok {
				return numeric(-f), nil
			}
		}
		return nil, errUnknownExpr{"unary " + v.Op}
	case *sqlir.BinaryExpr:
		switch v.Op {
		case "AND", "OR":
			// Three-valued: false decides AND and true decides OR, whatever
			// the other side; otherwise a NULL side makes the result NULL.
			decides := v.Op == "OR"
			l, err := x.eval(v.L, en)
			if err != nil {
				return nil, err
			}
			lb, lok := derefValue(l).(bool)
			if lok && lb == decides {
				return decides, nil
			}
			r, err := x.eval(v.R, en)
			if err != nil {
				return nil, err
			}
			rb, rok := derefValue(r).(bool)
			switch {
			case rok && rb == decides:
				return decides, nil
			case !lok || !rok:
				return nil, nil
			}
			return !decides, nil
		}
		l, err := x.eval(v.L, en)
		if err != nil {
			return nil, err
		}
		r, err := x.eval(v.R, en)
		if err != nil {
			return nil, err
		}
		switch v.Op {
		case "=", "<>", "!=", "<", "<=", ">", ">=":
			lr, lok := v.L.(*sqlir.RowExpr)
			rr, rok := v.R.(*sqlir.RowExpr)
			if lok || rok {
				if !lok || !rok || len(lr.Items) != len(rr.Items) {
					return nil, x.unsupported("row comparison")
				}
				return x.compareRows(v.Op, lr.Items, l, rr.Items, r)
			}
			if l, r, err = x.untypedPair(v.L, l, v.R, r); err != nil {
				return nil, err
			}
		}
		return x.binary(v.Op, l, r)
	case *sqlir.IsNull:
		val, err := x.eval(v.X, en)
		if err != nil {
			return nil, err
		}
		isNull := derefValue(val) == nil
		if v.Not {
			return !isNull, nil
		}
		return isNull, nil
	case *sqlir.InExpr:
		l, err := x.eval(v.X, en)
		if err != nil {
			return nil, err
		}
		in, sawNull := false, false
		if v.Sub != nil {
			cols, rows, err := x.evalSelect(v.Sub, en)
			if err != nil {
				return nil, err
			}
			var lhs []any
			lhsExprs := []sqlir.Expr{v.X}
			if row, ok := v.X.(*sqlir.RowExpr); ok {
				if lhs, ok = l.([]any); !ok {
					return nil, x.unsupported("row comparison")
				}
				lhsExprs = row.Items
			} else {
				lhs = []any{l}
			}
			if len(cols) != len(lhs) {
				return nil, x.unsupported("IN (subquery) whose columns do not match the left side")
			}
			for _, r := range rows {
				vals := make([]any, len(cols))
				for i, c := range cols {
					vals[i] = r[c]
				}
				eq, err := x.compareRows("=", lhsExprs, lhs, make([]sqlir.Expr, len(cols)), vals)
				if err != nil {
					return nil, err
				}
				if eq == nil {
					sawNull = true
				} else if b, _ := eq.(bool); b {
					in = true
					break
				}
			}
		} else {
			lr, isRow := v.X.(*sqlir.RowExpr)
			for _, it := range v.List {
				val, err := x.eval(it, en)
				if err != nil {
					return nil, err
				}
				if isRow {
					ir, ok := it.(*sqlir.RowExpr)
					if !ok || len(ir.Items) != len(lr.Items) {
						return nil, x.unsupported("row comparison")
					}
					eq, err := x.compareRows("=", lr.Items, l, ir.Items, val)
					if err != nil {
						return nil, err
					}
					if eq == nil {
						sawNull = true
					} else if b, _ := eq.(bool); b {
						in = true
						break
					}
					continue
				}
				if _, ok := it.(*sqlir.RowExpr); ok {
					return nil, x.unsupported("row comparison")
				}
				if _, row := derefValue(val).([]any); row {
					return nil, x.unsupported("IN of a value among rows")
				}
				if derefValue(val) == nil || derefValue(l) == nil {
					sawNull = true
					continue // NULL equals nothing, itself included
				}
				lt, vt, err := x.untypedPair(v.X, l, it, val)
				if err != nil {
					return nil, err
				}
				if equalValues(x.comparable(lt, vt)) {
					in = true
					break
				}
			}
		}
		// x IN (...) is NULL for a NULL x, or when it matches nothing and
		// the list holds a NULL; NOT IN negates only a known answer. A
		// subquery with no rows is false whatever x is.
		if !in && (sawNull || v.Sub == nil && derefValue(l) == nil) {
			return nil, nil
		}
		if v.Not {
			return !in, nil
		}
		return in, nil
	case *sqlir.Exists:
		_, rows, err := x.evalSelect(v.Select, en)
		if err != nil {
			return nil, err
		}
		if v.Not {
			return len(rows) == 0, nil
		}
		return len(rows) > 0, nil
	case *sqlir.SubQuery:
		cols, rows, err := x.evalSelect(v.Select, en)
		if err != nil {
			return nil, err
		}
		if len(cols) != 1 {
			return nil, x.unsupported("a scalar subquery of other than one column")
		}
		if len(rows) > 1 {
			return nil, x.tx.db.kind.Error(sqlir.CardinalityViolation, "more than one row returned by a subquery used as an expression", "", "", "")
		}
		if len(rows) == 0 {
			return nil, nil
		}
		return rows[0][cols[0]], nil
	case *sqlir.RowExpr:
		vals := make([]any, len(v.Items))
		for i, it := range v.Items {
			val, err := x.eval(it, en)
			if err != nil {
				return nil, err
			}
			vals[i] = val
		}
		return vals, nil
	case *sqlir.CaseExpr:
		var arg any
		if v.Arg != nil {
			a, err := x.eval(v.Arg, en)
			if err != nil {
				return nil, err
			}
			arg = a
		}
		for _, w := range v.Whens {
			cond, err := x.eval(w.When, en)
			if err != nil {
				return nil, err
			}
			hit := false
			if v.Arg != nil {
				// SQL equality, where NULL matches nothing.
				at, ct, err := x.untypedPair(v.Arg, arg, w.When, cond)
				if err != nil {
					return nil, err
				}
				hit = derefValue(at) != nil && derefValue(ct) != nil && equalValues(x.comparable(at, ct))
			} else {
				hit, _ = derefValue(cond).(bool)
			}
			if hit {
				return x.eval(w.Then, en)
			}
		}
		if v.Else != nil {
			return x.eval(v.Else, en)
		}
		return nil, nil
	case *sqlir.FuncCall:
		if err := x.checkArity(v); err != nil {
			return nil, err
		}
		if v.Name == "mysql_nullif" && volatile(v.Args[0]) {
			// MySQL evaluates the first argument again to return it, which
			// a function such as UUID() answers with another value.
			return nil, x.unsupported("NULLIF of a function that returns another value each time")
		}
		args := make([]any, len(v.Args))
		for i, a := range v.Args {
			val, err := x.eval(a, en)
			if err != nil {
				return nil, err
			}
			args[i] = val
		}
		if v.Name == "nullif" {
			var err error
			if args[0], args[1], err = x.untypedPair(v.Args[0], args[0], v.Args[1], args[1]); err != nil {
				return nil, err
			}
		}
		return x.callFunc(v.Name, args)
	}
	return nil, errUnknownExpr{fmt.Sprintf("%T", e)}
}

// untypedPair resolves the operands of a comparison as Postgres resolves an
// untyped string literal or a parameter: to the type of the other operand
// when that is a number, so '01' = 1 holds. Any other text compared with a
// number is unsupported, as Postgres has no operator for it and fails the
// statement, which detest cannot do before it reaches a row.
func (x *sqlExec) untypedPair(le sqlir.Expr, l any, re sqlir.Expr, r any) (any, any, error) {
	if x.tx.db.kind.InnoDB() {
		return l, r, nil // MySQL compares a string with a number as the number (mysqlOperands)
	}
	// Rows are compared pair by pair in compareRows; one reaching here comes
	// from a context that does not, such as HAVING, CASE or NULLIF.
	if _, ok := l.([]any); ok {
		return nil, nil, x.unsupported("row comparison")
	}
	if _, ok := r.([]any); ok {
		return nil, nil, x.unsupported("row comparison")
	}
	// With neither side typed, as in $1 = '01', Postgres compares text, so a
	// number the parameter holds is compared as the text it is sent as.
	if untypedExpr(le) && untypedExpr(re) {
		return asText(l), asText(r), nil
	}
	l, err := x.untyped(le, l, r)
	if err != nil {
		return nil, nil, err
	}
	r, err = x.untyped(re, r, l)
	if err != nil {
		return nil, nil, err
	}
	_, lp := le.(*sqlir.Param)
	_, rp := re.(*sqlir.Param)
	// A parameter compared with text takes the text type, so only a value of
	// another expression makes the comparison an error.
	if !lp && !rp && (isText(l) && isNumber(r) || isNumber(l) && isText(r)) {
		return nil, nil, x.unsupported("a comparison of text with a number")
	}
	// Postgres sorts NaN above every number, which the comparisons do not.
	if isNaN(l) || isNaN(r) {
		return nil, nil, x.unsupported("a comparison with NaN")
	}
	// Postgres has no operator comparing a number with a boolean or a time
	// either, and a parameter holding one fails to be sent as a number.
	if isNumber(l) && isOther(r) || isOther(l) && isNumber(r) {
		return nil, nil, x.unsupported("a comparison of a number with a value of another type")
	}
	return l, r, nil
}

// compareRows compares two rows as Postgres does: = and <> pair by pair in
// three-valued logic, and an ordering by the first pair that is not equal,
// which is NULL when that pair holds a NULL.
func (x *sqlExec) compareRows(op string, le []sqlir.Expr, lv any, re []sqlir.Expr, rv any) (any, error) {
	l, lok := lv.([]any)
	r, rok := rv.([]any)
	if !lok || !rok || len(l) != len(le) || len(r) != len(re) {
		return nil, x.unsupported("row comparison")
	}
	eq := op == "=" || op == "<>" || op == "!="
	if !eq && x.tx.db.kind.InnoDB() {
		return nil, x.unsupported("ordered row comparison")
	}
	sawNull := false
	for i := range l {
		li, ri, err := x.untypedPair(le[i], l[i], re[i], r[i])
		if err != nil {
			return nil, err
		}
		li, ri = x.comparable(li, ri)
		if derefValue(li) == nil || derefValue(ri) == nil {
			if !eq {
				return nil, nil
			}
			sawNull = true
			continue
		}
		if equalValues(li, ri) {
			continue
		}
		if eq {
			return op != "=", nil
		}
		return x.binary(op, li, ri)
	}
	if sawNull {
		return nil, nil
	}
	return op == "=" || op == "<=" || op == ">=", nil
}

func untypedExpr(e sqlir.Expr) bool {
	switch e := e.(type) {
	case *sqlir.Param:
		return true
	case *sqlir.Const:
		_, ok := e.Value.(string)
		return ok
	}
	return false
}

func asText(v any) any {
	if isNumber(v) {
		return fmt.Sprint(derefValue(v))
	}
	return v
}

func isNaN(v any) bool {
	f, ok := toFloat(derefValue(v))
	return ok && math.IsNaN(f)
}

// isOther reports whether v is a value that is neither NULL, text nor a
// number, such as a boolean or a time.
func isOther(v any) bool {
	return derefValue(v) != nil && !isText(v) && !isNumber(v)
}

func isText(v any) bool {
	switch derefValue(v).(type) {
	case string, []byte:
		return true
	}
	return false
}

func isNumber(v any) bool {
	_, ok := toFloat(derefValue(v))
	return ok
}

func (x *sqlExec) untyped(e sqlir.Expr, v, other any) (any, error) {
	if x.tx.db.kind.InnoDB() {
		return v, nil // MySQL compares a string with a number as the number (mysqlOperands)
	}
	var s string
	switch e := e.(type) {
	case *sqlir.Const:
		k, ok := e.Value.(string)
		if !ok {
			return v, nil
		}
		s = k
	case *sqlir.Param:
		switch k := derefValue(v).(type) {
		case string:
			s = k
		case []byte:
			s = string(k)
		default:
			return v, nil
		}
	default:
		return v, nil
	}
	if !isNumber(other) {
		return v, nil
	}
	t := strings.TrimSpace(s)
	if n, err := strconv.ParseInt(t, 10, 64); err == nil {
		return n, nil
	}
	if _, err := strconv.ParseFloat(t, 64); err == nil {
		// Postgres refuses '1.5' for an integer and compares it with a
		// float8 or numeric, but values do not carry their column's type,
		// and a float8 column keeps a whole number written as one as an
		// integer.
		return nil, x.unsupported("a string literal or parameter with a fraction compared with a number")
	}
	typ := "integer"
	if _, ok := derefValue(other).(float64); ok {
		typ = "double precision"
	}
	return nil, x.tx.db.kind.Error(sqlir.InvalidTextRepresentation, fmt.Sprintf("invalid input syntax for type %s: %q", typ, s), "", "", "")
}

func (x *sqlExec) binary(op string, l, r any) (any, error) {
	switch op {
	case "=", "<>", "!=", "<", "<=", ">", ">=", "LIKE", "ILIKE", "NOT LIKE", "NOT ILIKE":
		if derefValue(l) == nil || derefValue(r) == nil {
			return nil, nil // a comparison with NULL is NULL
		}
	}
	switch op {
	case "=", "<>", "!=", "<", "<=", ">", ">=", "<=>":
		l, r = x.comparable(l, r)
	}
	if _, ok := derefValue(l).([]any); ok && (op == "<" || op == "<=" || op == ">" || op == ">=") {
		return nil, x.unsupported("ordered row comparison")
	}
	_, lRow := derefValue(l).([]any)
	if _, rRow := derefValue(r).([]any); rRow && !lRow {
		return nil, x.unsupported("comparison of a value with a row")
	}
	if la, ok := derefValue(l).([]any); ok && op == "<=>" {
		ra, ok := derefValue(r).([]any)
		if !ok || len(la) != len(ra) {
			return nil, x.unsupported("row comparison of different shapes")
		}
		for i := range la {
			eq, err := x.binary("<=>", la[i], ra[i])
			if err != nil {
				return nil, err
			}
			if eq != true {
				return false, nil
			}
		}
		return true, nil
	}
	if la, ok := derefValue(l).([]any); ok && (op == "=" || op == "<>" || op == "!=") {
		ra, ok := derefValue(r).([]any)
		if !ok || len(la) != len(ra) {
			return nil, x.unsupported("row comparison of different shapes")
		}
		eq, unknown := x.rowsEqual(la, ra)
		if unknown {
			return nil, nil
		}
		return eq == (op == "="), nil
	}
	switch op {
	case "<=>":
		// MySQL's null-safe equality, each operand evaluated once.
		if derefValue(l) == nil || derefValue(r) == nil {
			return derefValue(l) == nil && derefValue(r) == nil, nil
		}
		return equalValues(l, r), nil
	case "=":
		return equalValues(l, r), nil
	case "<>", "!=":
		return !equalValues(l, r), nil
	case "<", "<=", ">", ">=":
		c, ok := compareValues(l, r)
		if !ok {
			return false, nil
		}
		switch op {
		case "<":
			return c < 0, nil
		case "<=":
			return c <= 0, nil
		case ">":
			return c > 0, nil
		default:
			return c >= 0, nil
		}
	case "LIKE", "ILIKE", "NOT LIKE", "NOT ILIKE":
		m, dangling := likeMatch(l, r, strings.HasSuffix(op, "ILIKE"))
		if dangling && !x.tx.db.kind.InnoDB() {
			return nil, x.unsupported("LIKE pattern ending with an escape, which Postgres refuses")
		}
		if strings.HasPrefix(op, "NOT") {
			return !m, nil
		}
		return m, nil
	case "+", "-", "*", "/", "%":
		if x.tx.db.kind.InnoDB() {
			// MySQL's arithmetic takes a string as the number it starts with.
			l, r = mysqlArithOperand(l), mysqlArithOperand(r)
		}
		v, err := arith(op, l, r)
		if ke := (kindError{}); errors.As(err, &ke) && ke.kind == sqlir.DivisionByZero && x.selectStmt && x.tx.db.kind.InnoDB() {
			// MySQL's strict mode refuses a division by zero in a write
			// only; a SELECT gets NULL and a warning.
			return nil, nil
		}
		if ke := (kindError{}); errors.As(err, &ke) {
			return nil, x.tx.db.kind.Error(ke.kind, ke.msg, "", "", "")
		}
		return v, err
	case "||":
		if derefValue(l) == nil || derefValue(r) == nil {
			return nil, nil // NULL || x is NULL
		}
		return fmt.Sprint(derefValue(l)) + fmt.Sprint(derefValue(r)), nil
	}
	return nil, errUnknownExpr{"operator " + op}
}

// arith handles numeric arithmetic and timestamp/interval arithmetic
// (intervals are time.Duration).
func arith(op string, l, r any) (any, error) {
	l, r = derefValue(l), derefValue(r)
	if l == nil || r == nil {
		return nil, nil
	}
	if t, ok := l.(time.Time); ok {
		switch rv := r.(type) {
		case time.Duration:
			if op == "+" {
				return t.Add(rv), nil
			}
			if op == "-" {
				return t.Add(-rv), nil
			}
		case time.Time:
			if op == "-" {
				return t.Sub(rv), nil
			}
		}
		return nil, errUnknownExpr{"timestamp arithmetic " + op}
	}
	if d, ok := l.(time.Duration); ok {
		switch rv := r.(type) {
		case time.Duration:
			if op == "+" {
				return d + rv, nil
			}
			if op == "-" {
				return d - rv, nil
			}
		case time.Time:
			if op == "+" {
				return rv.Add(d), nil
			}
		}
		if f, ok := toFloat(r); ok && op == "*" {
			return time.Duration(float64(d) * f), nil
		}
		return nil, errUnknownExpr{"interval arithmetic " + op}
	}
	if il, ok := integer(l); ok {
		if ir, ok := integer(r); ok {
			return intArith(op, il, ir)
		}
	}
	fl, okl := toFloat(l)
	fr, okr := toFloat(r)
	if !okl || !okr {
		return nil, errUnknownExpr{"arithmetic on non-numeric values"}
	}
	var v float64
	switch op {
	case "+":
		v = fl + fr
	case "-":
		v = fl - fr
	case "*":
		v = fl * fr
	case "/":
		if fr == 0 {
			return nil, kindError{sqlir.DivisionByZero, "division by zero"}
		}
		v = fl / fr
	case "%":
		if fr == 0 {
			return nil, kindError{sqlir.DivisionByZero, "division by zero"}
		}
		v = math.Mod(fl, fr)
	}
	return numeric(v), nil
}

func numeric(v float64) any {
	if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
		return int64(v)
	}
	return v
}

func castValue(v any, typ string) any {
	v = derefValue(v)
	if v == nil {
		return nil
	}
	switch typ {
	case "int", "int4", "int8", "bigint", "integer", "smallint":
		if f, ok := toFloat(v); ok {
			return int64(f)
		}
	case "float8", "float4", "double precision", "numeric", "real":
		if f, ok := toFloat(v); ok {
			return f
		}
	case "text", "varchar", "bpchar":
		return fmt.Sprint(v)
	case "bool", "boolean":
		if b, ok := v.(bool); ok {
			return b
		}
		return fmt.Sprint(v) == "true"
	case "interval":
		if s, ok := v.(string); ok {
			if d, err := time.ParseDuration(strings.ReplaceAll(strings.ReplaceAll(s, " seconds", "s"), " second", "s")); err == nil {
				return d
			}
		}
	}
	return v
}

// rowsEqual compares two rows of values with SQL equality: unequal if any
// pair differs, unknown if none does but a NULL is among them.
func (x *sqlExec) rowsEqual(a, b []any) (eq, unknown bool) {
	for i := range a {
		l, r := x.comparable(a[i], b[i])
		switch {
		case derefValue(l) == nil || derefValue(r) == nil:
			unknown = true
		case !equalValues(l, r):
			return false, false
		}
	}
	return !unknown, unknown
}

// callFunc evaluates the scalar functions that appear on control paths.
// pg_try_advisory_xact_lock is a non-blocking lock held until the end of the
// transaction, modeled in the DB's lock table.
// strictFuncs are the functions detest evaluates that Postgres declares
// strict, given NULL, they return NULL without being called, with the
// numbers of arguments their signatures take.
var strictFuncs = map[string][]int{
	"lower": {1}, "upper": {1}, "length": {1}, "char_length": {1}, "hashtext": {1},
	"abs": {1}, "floor": {1}, "ceil": {1}, "ceiling": {1}, "round": {1, 2}, "power": {2}, "pow": {2},
	"nextval": {1}, "setval": {2, 3}, "pg_advisory_xact_lock": {1, 2}, "pg_try_advisory_xact_lock": {1, 2},
	"octet_length": {1}, "left": {2}, "mysql_signed": {1}, "mysql_double": {1},
}

// mysqlNullIfAnyNull are MySQL's functions of any number of arguments that
// are NULL when one is, where the executor's Postgres ones skip NULLs.
var mysqlNullIfAnyNull = map[string]bool{"mysql_concat": true, "mysql_greatest": true, "mysql_least": true}

// roundDecimal is round(x, n) on the decimal x was written as, half away from
// zero as Postgres's numeric rounds: on the float, f*10^n is off by a
// little, so round(-81.865, 2) would come out -81.86.
func roundDecimal(f float64, n int) float64 {
	// Past these, a float64 keeps its value or becomes 0, which also bounds
	// the power of ten below for a scale taken from SQL.
	switch {
	case n > 30:
		return f
	case n < -330:
		return 0
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'f', -1, 64))
	if !ok {
		return f
	}
	scale := new(big.Rat).SetFrac(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(max(n, -n))), nil), big.NewInt(1))
	if n >= 0 {
		r.Mul(r, scale)
	} else {
		r.Quo(r, scale)
	}
	// Half away from zero: add or subtract 1/2, then truncate.
	half := big.NewRat(1, 2)
	if r.Sign() < 0 {
		r.Sub(r, half)
	} else {
		r.Add(r, half)
	}
	q := new(big.Int).Quo(r.Num(), r.Denom())
	r.SetInt(q)
	if n >= 0 {
		r.Quo(r, scale)
	} else {
		r.Mul(r, scale)
	}
	out, _ := r.Float64()
	return out
}

// checkArity refuses a call to a function detest knows that no signature of
// it takes, before its arguments are evaluated, as Postgres resolves the
// signature when it plans the statement.
// otherArity are the numbers of arguments of functions detest evaluates
// that are not strict. current_timestamp takes the one precision the
// converter passes for CURRENT_TIMESTAMP(p), which SQL cannot call itself.
var otherArity = map[string][]int{
	"now": {0}, "clock_timestamp": {0}, "transaction_timestamp": {0}, "statement_timestamp": {0},
	"current_timestamp": {1}, "random": {0}, "nullif": {2},
	"gen_random_uuid": {0}, "uuid_generate_v4": {0},
}

func (x *sqlExec) checkArity(f *sqlir.FuncCall) error {
	arity, known := strictFuncs[f.Name]
	if !known {
		arity, known = otherArity[f.Name]
	}
	if known && !slices.Contains(arity, len(f.Args)) {
		return x.unsupported(fmt.Sprintf("%s with %d arguments", f.Name, len(f.Args)))
	}
	// round with a scale exists only for numeric; detest keeps no type for
	// its argument but can see a cast to a float or a call of random.
	// Refusing every argument not proven numeric would refuse round(price,
	// 2) on a numeric column too.
	if f.Name == "round" && len(f.Args) == 2 {
		c, cast := f.Args[0].(*sqlir.Cast)
		r, call := f.Args[0].(*sqlir.FuncCall)
		if cast && (c.Type == "float4" || c.Type == "float8") || call && r.Name == "random" {
			return x.unsupported("round of a float with a scale")
		}
	}
	return nil
}

func (x *sqlExec) callFunc(name string, args []any) (any, error) {
	// database/sql lets a string argument come as []byte, which the
	// functions would otherwise print as a list of numbers.
	for i, a := range args {
		if b, ok := derefValue(a).([]byte); ok {
			args[i] = string(b)
		}
	}
	if _, strict := strictFuncs[name]; (strict || mysqlNullIfAnyNull[name]) && slices.ContainsFunc(args, func(a any) bool { return derefValue(a) == nil }) {
		return nil, nil // a strict function of NULL is NULL
	}
	if x.tx.db.kind.InnoDB() {
		switch name {
		case "mysql_greatest", "mysql_least":
			// MySQL compares a mix of numbers and strings as strings, in
			// its own formatting of the numbers, which the executor's
			// pairwise comparison does not follow.
			str := slices.ContainsFunc(args, func(a any) bool { _, ok := derefValue(a).(string); return ok })
			num := slices.ContainsFunc(args, func(a any) bool { _, ok := toFloat(derefValue(a)); return ok })
			if str && num {
				return nil, x.unsupported(strings.ToUpper(strings.TrimPrefix(name, "mysql_")) + " of numbers and strings")
			}
			return x.callFunc(strings.TrimPrefix(name, "mysql_"), args)
		case "mysql_concat":
			// MySQL's, NULL when an argument is (strictFuncs), and
			// otherwise the executor's.
			return x.callFunc(strings.TrimPrefix(name, "mysql_"), args)
		case "abs", "floor", "ceil", "power":
			for i, a := range args {
				args[i] = mysqlArithOperand(a) // a string as the number it converts to
			}
		case "round":
			if _, ok := derefValue(args[0]).(string); ok {
				// The string converts to a DOUBLE, which MySQL rounds half
				// to even, where the executor rounds half away from zero.
				return nil, x.unsupported("ROUND of a string")
			}
		case "mysql_nullif":
			// NULLIF compares as MySQL's = does.
			if derefValue(args[0]) == nil || derefValue(args[1]) == nil {
				return args[0], nil
			}
			if equalValues(x.comparable(args[0], args[1])) {
				return nil, nil
			}
			return args[0], nil
		case "mysql_signed":
			return mysqlSigned(derefValue(args[0])), nil
		case "mysql_double":
			if f, ok := toFloat(derefValue(mysqlArithOperand(args[0]))); ok {
				return f, nil
			}
			return nil, x.unsupported("CAST to DOUBLE of a " + fmt.Sprintf("%T", derefValue(args[0])))
		case "mysql_truth":
			return mysqlTruth(derefValue(args[0])), nil
		case "last_insert_id":
			if x.tx.lastInsertID == nil {
				return int64(0), nil
			}
			return *x.tx.lastInsertID, nil
		}
	}
	d := func(i int) any {
		if i < len(args) {
			return derefValue(args[i])
		}
		return nil
	}
	switch name {
	case "coalesce":
		for i := range args {
			if d(i) != nil {
				return args[i], nil
			}
		}
		return nil, nil
	case "nullif":
		if len(args) == 2 && equalValues(args[0], args[1]) {
			return nil, nil
		}
		return args[0], nil
	case "greatest", "least":
		var best any
		for i := range args {
			v := d(i)
			if v == nil {
				continue
			}
			if best == nil {
				best = v
				continue
			}
			c, _ := compareValues(v, best)
			if (name == "greatest" && c > 0) || (name == "least" && c < 0) {
				best = v
			}
		}
		return best, nil
	case "left":
		if d(0) == nil || d(1) == nil {
			return nil, nil
		}
		n, ok := integer(d(1))
		if !ok {
			if !x.tx.db.kind.InnoDB() {
				return nil, x.unsupported("left with a length other than an integer")
			}
			n, _ = integer(mysqlSigned(d(1))) // MySQL converts the length
		}
		r := []rune(fmt.Sprint(d(0)))
		if n < 0 && !x.tx.db.kind.InnoDB() {
			n += int64(len(r)) // Postgres: all but the last -n characters
		}
		if int(n) < len(r) {
			r = r[:max(n, 0)]
		}
		return string(r), nil
	case "lower":
		return strings.ToLower(fmt.Sprint(d(0))), nil
	case "upper":
		return strings.ToUpper(fmt.Sprint(d(0))), nil
	case "length", "char_length":
		return int64(len([]rune(fmt.Sprint(d(0))))), nil
	case "octet_length":
		return int64(len(fmt.Sprint(d(0)))), nil
	case "concat":
		var b strings.Builder
		for i := range args {
			if v := d(i); v != nil {
				fmt.Fprint(&b, v)
			}
		}
		return b.String(), nil
	case "hashtext":
		return fmt.Sprint(d(0)), nil
	case "nextval":
		// A sequence is not transactional: a rolled back nextval stays used.
		if len(args) != 1 {
			return nil, x.unsupported("nextval with other than one argument")
		}
		return x.tx.db.nextval(fmt.Sprint(derefValue(args[0]))), nil
	case "setval":
		if len(args) < 2 || len(args) > 3 {
			return nil, x.unsupported("setval with other than two or three arguments")
		}
		if derefValue(args[0]) == nil || derefValue(args[1]) == nil {
			return nil, nil // strict: NULL in, NULL out
		}
		v, ok := toInt64(derefValue(args[1]))
		if !ok {
			return nil, x.unsupported("setval to a non-integer")
		}
		called := true
		if len(args) == 3 {
			called, _ = derefValue(args[2]).(bool)
		}
		x.tx.db.setval(fmt.Sprint(derefValue(args[0])), v, called)
		return v, nil
	case "gen_random_uuid", "uuid_generate_v4":
		return x.tx.db.newUUID(), nil
	case "now", "clock_timestamp", "current_timestamp", "transaction_timestamp", "statement_timestamp":
		// Postgres fixes now() at the start of the transaction, which a
		// transaction that starts early and commits late depends on.
		ts := x.tx.start
		if x.tx.db.kind.InnoDB() {
			ts = x.start // MySQL's NOW() is the statement's time
		}
		switch name {
		case "clock_timestamp":
			ts = time.Now()
		case "statement_timestamp":
			ts = x.start
		}
		if ts.IsZero() {
			ts = time.Now()
		}
		ts = ts.Truncate(time.Microsecond) // a Postgres timestamp has six fractional digits
		if x.tx.db.kind.InnoDB() {
			// MySQL's NOW(fsp) keeps fsp fractional digits, none without one.
			p := 0.0
			if len(args) > 0 {
				f, ok := toFloat(d(0))
				if !ok || f < 0 || f > 6 || f != math.Trunc(f) {
					return nil, x.unsupported(name + " with a precision other than 0 to 6")
				}
				p = f
			}
			return ts.Truncate(time.Duration(math.Pow10(9 - int(p)))), nil
		}
		if len(args) == 0 {
			return ts, nil
		}
		// Only CURRENT_TIMESTAMP(p) and LOCALTIMESTAMP(p) take an argument,
		// which the converter passes as current_timestamp's.
		p, ok := toFloat(d(0))
		if name != "current_timestamp" || len(args) > 1 || !ok || p < 0 {
			return nil, x.unsupported(name + " with these arguments")
		}
		// Postgres reduces a precision above 6 to 6, with a warning.
		return ts.Round(time.Duration(math.Pow10(9 - int(min(p, 6))))), nil
	case "random":
		return 0.5, nil
	case "abs":
		if f, ok := toFloat(d(0)); ok {
			return numeric(math.Abs(f)), nil
		}
	case "floor":
		if f, ok := toFloat(d(0)); ok {
			return numeric(math.Floor(f)), nil
		}
	case "ceil", "ceiling":
		if f, ok := toFloat(d(0)); ok {
			return numeric(math.Ceil(f)), nil
		}
	case "round":
		f, ok := toFloat(d(0))
		if !ok {
			break
		}
		if len(args) == 1 {
			return numeric(math.Round(f)), nil
		}
		places, ok := toFloat(d(1))
		if !ok {
			break
		}
		if places != math.Trunc(places) || places < math.MinInt32 || places > math.MaxInt32 {
			// round(numeric, integer) is the only signature with two, and
			// its scale is an int4.
			return nil, x.unsupported("round with a scale that is not an integer")
		}
		return roundDecimal(f, int(places)), nil
	case "power", "pow":
		a, oka := toFloat(d(0))
		b, okb := toFloat(d(1))
		if oka && okb {
			return math.Pow(a, b), nil
		}
	case "make_interval":
		// make_interval(secs => x) is the form that appears in backoff SQL; a
		// single argument is taken as seconds.
		if f, ok := toFloat(d(0)); ok {
			return time.Duration(f * float64(time.Second)), nil
		}
	case "pg_try_advisory_xact_lock", "pg_advisory_xact_lock":
		var kb strings.Builder
		for i := range args {
			fmt.Fprintf(&kb, "%v|", d(i))
		}
		lk := lockKey{"__advisory__", kb.String()}
		if x.tx.heldByOther(lk, lockUpdate) && name == "pg_try_advisory_xact_lock" {
			return false, nil
		}
		if err := x.tx.lock(lk); err != nil {
			return nil, err
		}
		if name == "pg_advisory_xact_lock" {
			return nil, nil
		}
		return true, nil
	}
	return nil, errUnknownExpr{name + "(...)"}
}

func targetName(t sqlir.Target) string {
	if t.Alias != "" {
		return t.Alias
	}
	switch v := t.Expr.(type) {
	case *sqlir.ColumnRef:
		return v.Column
	case *sqlir.FuncCall:
		return v.Name
	case *sqlir.WindowFunc:
		return v.Func.Name
	case *sqlir.Exists:
		return "exists"
	}
	return "?column?"
}

func seenDistinct(rows []Row, o Row, cols []string) bool {
	for _, r := range rows {
		same := true
		for _, c := range cols {
			if !sameValue(r[c], o[c]) {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

// exprString renders a predicate for the trace with parameter values filled in.
func (x *sqlExec) exprString(e sqlir.Expr) string {
	switch v := e.(type) {
	case nil:
		return "true"
	case *sqlir.ColumnRef:
		if v.Table != "" {
			return v.Table + "." + v.Column
		}
		return v.Column
	case *sqlir.Param:
		if v.Index >= 0 && v.Index < len(x.args) {
			return fmt.Sprint(x.args[v.Index])
		}
		return "?"
	case *sqlir.Const:
		return fmt.Sprint(v.Value)
	case *sqlir.BinaryExpr:
		return x.exprString(v.L) + " " + strings.ToLower(v.Op) + " " + x.exprString(v.R)
	case *sqlir.UnaryExpr:
		return strings.ToLower(v.Op) + " " + x.exprString(v.X)
	case *sqlir.IsNull:
		if v.Not {
			return x.exprString(v.X) + " is not null"
		}
		return x.exprString(v.X) + " is null"
	case *sqlir.InExpr:
		var items []string
		for _, it := range v.List {
			items = append(items, x.exprString(it))
		}
		rhs := "(" + strings.Join(items, ", ") + ")"
		if v.Sub != nil {
			rhs = "(subquery)"
		}
		if v.Not {
			return x.exprString(v.X) + " not in " + rhs
		}
		return x.exprString(v.X) + " in " + rhs
	case *sqlir.Cast:
		return x.exprString(v.X)
	case *sqlir.FuncCall:
		return v.Name + "(...)"
	case *sqlir.Exists:
		return "exists (subquery)"
	case *sqlir.SubQuery:
		return "(subquery)"
	case *sqlir.RowExpr:
		var items []string
		for _, it := range v.Items {
			items = append(items, x.exprString(it))
		}
		return "(" + strings.Join(items, ", ") + ")"
	}
	return "..."
}

func toDriverValue(v any) driver.Value {
	v = derefValue(v)
	switch x := v.(type) {
	case nil, int64, float64, bool, []byte, string, time.Time:
		return x
	case time.Duration:
		return x.String()
	}
	if f, ok := toFloat(v); ok {
		return int64(f)
	}
	return fmt.Sprint(v)
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case float64:
		if n == float64(int64(n)) {
			return int64(n), true
		}
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

// renameColumns renames rows' columns by position, as a column alias list
// does.
func renameColumns(rows []Row, cols, names []string) []Row {
	for i, r := range rows {
		m := Row{}
		for j, c := range cols {
			if j < len(names) {
				m[names[j]] = r[c]
			} else {
				m[c] = r[c]
			}
		}
		rows[i] = m
	}
	return rows
}

func assignedColumns(set []sqlir.Assignment) []string {
	cols := make([]string, len(set))
	for i, a := range set {
		cols[i] = a.Column
	}
	return cols
}

func assignedValues(set []sqlir.Assignment) []sqlir.Expr {
	vals := make([]sqlir.Expr, len(set))
	for i, a := range set {
		vals[i] = a.Value
	}
	return vals
}

// kindError is a database error raised where the server is not at hand, such
// as in arithmetic; eval turns it into the server's DBError.
type kindError struct {
	kind sqlir.DBErrorKind
	msg  string
}

func (e kindError) Error() string { return "detest: " + e.msg }

func integer(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int16:
		return int64(n), true
	}
	return 0, false
}

// intArith is integer arithmetic as Postgres does it on bigint: overflow is an
// error rather than a wrap, and division truncates toward zero.
func intArith(op string, l, r int64) (any, error) {
	overflow := kindError{sqlir.ArithmeticOutOfRange, "bigint out of range"}
	switch op {
	case "+":
		v := l + r
		if (r > 0 && v < l) || (r < 0 && v > l) {
			return nil, overflow
		}
		return v, nil
	case "-":
		v := l - r
		if (r < 0 && v < l) || (r > 0 && v > l) {
			return nil, overflow
		}
		return v, nil
	case "*":
		if l == 0 || r == 0 {
			return int64(0), nil
		}
		v := l * r
		if v/r != l || (l == -1 && r == math.MinInt64) || (r == -1 && l == math.MinInt64) {
			return nil, overflow
		}
		return v, nil
	case "/", "%":
		if r == 0 {
			return nil, kindError{sqlir.DivisionByZero, "division by zero"}
		}
		if l == math.MinInt64 && r == -1 {
			if op == "%" {
				return int64(0), nil
			}
			return nil, overflow
		}
		if op == "/" {
			return l / r, nil
		}
		return l % r, nil
	}
	return nil, errUnknownExpr{"integer operator " + op}
}
