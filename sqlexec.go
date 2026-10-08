package detest

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"maps"
	"math"
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
// UPDATE ... FROM and DELETE ... USING. The expressions a statement holds are
// evaluated in sqleval.go and cast in sqlcast.go. Statements the IR cannot
// express never get here: the dialect frontend rejects them.

type sqlResult struct {
	cols     []string
	rows     [][]driver.Value
	affected int64
	// star are the columns a RETURNING * gives, the table's, and alias the
	// name RETURNING refers to the written row by.
	star  []string
	alias string
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
	// arrays are the arrays the statement's = ANY and <> ALL read, read
	// before it runs anything.
	arrays map[*sqlir.ArrayCmp]*arrayElems
	// arrayTypes are values of the type the column check found for the
	// operand of an = ANY or <> ALL over an uncast array, which its
	// elements are read as before the statement runs.
	arrayTypes map[*sqlir.ArrayCmp]any
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
	// inWrite is set while an INSERT, UPDATE or DELETE statement runs, and
	// fkChecks are the foreign key checks it holds back to its end.
	inWrite  bool
	fkChecks []func() error
	// frozen are the rows of the tables a statement reads, as they were
	// when it began, which its queries read instead of the latest.
	frozen map[string][]Row
	// queryCols are the output columns of the queries the statement ran,
	// in order, by the query (a *sqlir.SelectStmt), for a * over them, and
	// cteCols those of the CTEs in scope, by name, scoped as ctes is.
	queryCols map[any][]string
	cteCols   map[string][]string
	// pendingCTEs are the CTEs of WITH ... UPDATE or DELETE not run yet,
	// which run when the statement first reads them, and writeRows and
	// writeCols those run, kept apart from ctes, which a query with a WITH
	// of its own restores when it ends.
	pendingCTEs map[string]sqlir.CTE
	writeRows   map[string][]Row
	writeCols   map[string][]string
	// paramTimeTypes are the date and timestamp types Postgres gives
	// parameters compared with operands of those types (wallClock).
	paramTimeTypes map[int]string
	// exprTypes are the types the column check resolved for expressions
	// whose value a run converts by type: the time type CASE, COALESCE,
	// GREATEST and LEAST resolve a timestamp and a timestamptz to, and the
	// source of date_trunc.
	exprTypes map[sqlir.Expr]string
	// colls are the collations the column check found the columns of the
	// statement's column references to declare, and collates whether the
	// statement has a COLLATE clause.
	colls    map[*sqlir.ColumnRef]colNote
	collates bool
	// outputs are the ORDER BY keys that name an output column, with the
	// select list's expression they name.
	outputs map[sqlir.Expr]sqlir.Expr
	// searchOuter is the row a joined table's search is run for, whose
	// columns the search takes as constants.
	searchOuter *searchOuter
	// pushdown is set while a locking read searches a secondary index it
	// does not read all its columns from, whose condition MySQL pushes down
	// to the index, so that it finds the range ended without reading, nor
	// locking, the row past it.
	pushdown bool
	// scanned is called with each row a locking search of an UPDATE or a
	// DELETE locks, as it locks it, to write the row then.
	scanned func(table string, r Row) error
	// skipped is the rows a locking search's SKIP LOCKED passed over, on an
	// index record another transaction holds, which the rows the statement
	// then locks leave out too.
	skipped map[lockKey]bool
}

// rowByRow reports whether InnoDB writes the rows of an UPDATE or a DELETE
// of table one by one as its search locks them, so that a row's change, its
// checks and its error come before the search goes on to the next. MySQL
// reads all the rows first when it sorts them, and when the update changes
// a column of the index it searches by or of the primary key, which it then
// holds apart; detest locks and writes them so otherwise too, and here also
// for a write joining other tables, which it keeps whole.
func (x *sqlExec) rowByRow(table, alias string, where sqlir.Expr, simple bool, set []string) bool {
	if !x.tx.db.kind.InnoDB() || !simple {
		return false
	}
	def := x.tx.db.defs[table]
	if def == nil {
		return false
	}
	sr := x.searchRange(table, alias, where)
	return !slices.ContainsFunc(set, func(c string) bool { return slices.Contains(sr.cols, c) || slices.Contains(def.pk, c) })
}

func (x *sqlExec) skip(lk lockKey) {
	if x.skipped == nil {
		x.skipped = map[lockKey]bool{}
	}
	x.skipped[lk] = true
}

// searchOuter is the outer row of a joined table's search: the columns of
// env that are not the searched table's are constants to it.
type searchOuter struct {
	table, alias string
	env          *env
}

// outerConstant evaluates e as a constant of the joined table's search: an
// expression with no column of the searched table, evaluated with the outer
// row.
func (x *sqlExec) outerConstant(e sqlir.Expr, table string) (any, bool) {
	o := x.searchOuter
	if o == nil || o.table != table {
		return nil, false
	}
	if slices.ContainsFunc(sqlir.ColumnRefs(e), func(c *sqlir.ColumnRef) bool { return x.searchedColumn(c, table, o.alias) }) {
		return nil, false
	}
	v, err := x.eval(e, o.env)
	return v, err == nil
}

// searchedColumn reports whether c names a column of table, called alias in
// the query, rather than of a table joined to it.
func (x *sqlExec) searchedColumn(c *sqlir.ColumnRef, table, alias string) bool {
	if c.Table != "" {
		return c.Table == alias || c.Table == relname(table)
	}
	def := x.tx.db.defs[table]
	return def != nil && slices.Contains(def.columns, c.Column)
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

// write runs an INSERT, UPDATE or DELETE and then the checks it held back
// to its end.
func (x *sqlExec) write(run func() (*sqlResult, error)) (*sqlResult, error) {
	x.inWrite, x.fkChecks = true, nil
	defer func() { x.inWrite, x.fkChecks, x.frozen = false, nil, nil }()
	res, err := run()
	if err != nil {
		return nil, err
	}
	if err := x.endStatement(); err != nil {
		return nil, err
	}
	return res, nil
}

func (x *sqlExec) execStatement(stmt sqlir.Statement) (*sqlResult, error) {
	tx := x.tx
	if err := x.checkColumns(stmt); err != nil {
		return nil, err
	}
	// After the column check, which refuses what Postgres refuses when it
	// plans the statement, as Postgres reads the arrays only at bind time.
	if err := x.checkArrays(stmt); err != nil {
		return nil, err
	}
	switch st := stmt.(type) {
	case *sqlir.Script:
		res := &sqlResult{}
		for _, sub := range st.Stmts {
			// What the column check noted, and whether a column declares a
			// collation, belong to the statement before, whose DDL may
			// have changed the latter.
			x.colls, x.outputs, x.collates = nil, nil, false
			x.ctes, x.cteCols, x.frozen = map[string][]Row{}, nil, nil
			x.pendingCTEs, x.writeRows, x.writeCols = nil, nil, nil
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
		if st.Name == "all" {
			x.tx.lockTimeout = false // RESET ALL
		}
		if !tx.db.kind.InnoDB() && (st.Name == "timezone" || st.Name == "all") {
			// SET TIME ZONE holds from here on in the transaction; a
			// session setting, unlike SET LOCAL, outlives it once it commits.
			x.tx.timeZone = st.Zone
			if !st.Local {
				tx.pendingTimeZone = &zoneSetting{st.Zone}
			}
		}
		if !tx.db.kind.InnoDB() && !st.Local && (st.Name == "lock_timeout" || st.Name == "all") {
			// Record each script substatement, before a later SET LOCAL can
			// change the transaction's timeout without changing the session's.
			v := tx.lockTimeout
			tx.pendingLockTimeout = &v
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
		if st.Name == "search_path" {
			// Names resolve on the path postgres.SearchPath declares, for
			// every connection, so a SET of another path would read and
			// write other tables than the server, or find a table the
			// server does not. One that names the declared path again,
			// as a migration does, changes nothing. An empty path, as
			// pg_dump sets, leaves the server only qualified names,
			// which resolve the same either way; a dump writes nothing
			// else after it, and refusing it would stop dumps loading.
			var path []string
			for s := range strings.SplitSeq(st.Value, ",") {
				// "$user" names a schema no test declares, and pg_catalog
				// is on every path whether written or not.
				if s = strings.Trim(strings.TrimSpace(s), `"`); s != "" && s != "$user" && s != "pg_catalog" {
					path = append(path, s)
				}
			}
			// The empty path is the one written empty; a path of "$user"
			// or pg_catalog alone leaves the server no schema to find an
			// application table in, where detest would still find it.
			if strings.TrimSpace(st.Value) != "" && !slices.Equal(path, x.tx.db.kind.SearchPath()) {
				return nil, x.unsupported("SET search_path to a path other than postgres.SearchPath's (" + strings.Join(x.tx.db.kind.SearchPath(), ", ") + ")")
			}
		}
		if st.Returns {
			// set_config returns the value it set, as text.
			col := st.Column
			if col == "" {
				col = "set_config"
			}
			return &sqlResult{cols: []string{col}, rows: [][]driver.Value{{st.Value}}, affected: 1}, nil
		}
		return &sqlResult{}, nil
	case *sqlir.SetConstraintsStmt:
		return &sqlResult{}, x.setConstraints(st)
	case *sqlir.SelectStmt:
		return x.execSelect(st)
	case *sqlir.InsertStmt:
		res, err := x.write(func() (*sqlResult, error) { return x.execInsert(st) })
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
		return x.write(func() (*sqlResult, error) { return x.execUpdate(st) })
	case *sqlir.DeleteStmt:
		if !st.Truncate {
			return x.write(func() (*sqlResult, error) { return x.execDelete(st) })
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
			// USING converts the rows by an expression, which the rewrite
			// under the new type does not run. A migration runs it on a table
			// without rows, where the expression is never evaluated.
			if slices.ContainsFunc(ch.Columns, func(c sqlir.ColumnDef) bool { return c.Using }) {
				return nil, x.unsupported("ALTER COLUMN TYPE with USING on a table with rows")
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
	db := x.tx.db
	_, isView := db.views[db.resolve(st.Table)]
	_, isSeq := db.seqDefs[sequenceName(db.resolve(st.Table))]
	if _, isTable := db.defs[db.resolve(st.Table)]; (isTable || isView || isSeq || db.isIndex(db.resolve(st.Table))) && st.IfNotExists {
		// Postgres leaves the table as it is, without running the query.
		return &sqlResult{}, nil
	}
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
	colls, binary, err := x.outputCollations(st.Select, len(cols))
	if err != nil {
		return nil, err
	}
	for i := range ch.Columns {
		if i < len(binary) && binary[i] {
			// Kept as bytea, which orders by bytes whatever the collation.
			ch.Columns[i].Type = "bytea"
		}
	}
	if err := x.tx.db.applySchema(&sqlir.SchemaStmt{Changes: []sqlir.SchemaChange{ch}}, x.tx); err != nil {
		return nil, err
	}
	table := x.tx.db.resolve(st.Table)
	if def := x.tx.db.defs[table]; def != nil {
		for i, name := range colls {
			if name != "" && name != "default" {
				if def.collations == nil {
					def.collations = map[string]string{}
				}
				def.collations[cols[i]] = name
			}
		}
	}
	if st.Materialized {
		x.tx.db.matviews[table] = st
	}
	// The collations and the materialized view above change the schema
	// after applySchema dropped the cache.
	x.tx.db.declaredCache = nil
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
	// PostgreSQL resolves the function signature before evaluating arguments,
	// so a refused call must not advance sequences or acquire advisory locks.
	if t.Func.Name != "generate_series" {
		return nil, x.unsupported("set-returning function " + t.Func.Name + " in FROM")
	}
	if len(t.Func.Args) != 2 && len(t.Func.Args) != 3 {
		return nil, x.unsupported("generate_series with other than two or three arguments")
	}
	// The functions detest runs in FROM give one column, and the ordinality
	// one more.
	width := 1
	if t.Ordinality {
		width++
	}
	if len(t.Columns) > width {
		return nil, x.tx.db.kind.Error(sqlir.InvalidColumnReference, fmt.Sprintf("too many column aliases specified for function %s", t.Func.Name), "", "", "")
	}
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
	// A plain SELECT waits too, in pg_advisory_xact_lock or a locking
	// subquery, and reads from its snapshot after the wait all the same.
	x.freeze(sel)
	defer func() { x.frozen = nil }()
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

// withCTEs runs the CTEs of sel the query reads. Postgres skips one it does
// not read, so its errors, locks and calls never happen.
func (x *sqlExec) withCTEs(sel *sqlir.SelectStmt, outer *env) error {
	return x.evalCTEs(sel.With, reachableCTEs(sel), outer)
}

// evalCTEs runs the CTEs read of with, in order, so that a later one reads
// the earlier ones.
func (x *sqlExec) evalCTEs(with []sqlir.CTE, read map[string]bool, outer *env) error {
	for _, cte := range with {
		if !read[cte.Name] {
			continue
		}
		cols, rows, err := x.evalSelect(cte.Select, outer)
		if err != nil {
			return err
		}
		if x.cteCols == nil {
			x.cteCols = map[string][]string{}
		}
		x.cteCols[cte.Name] = cols
		x.ctes[cte.Name] = rows
	}
	return nil
}

// writeCTEs declares the CTEs of WITH ... UPDATE or DELETE that body reads.
// Each runs once, when the statement first reads it, after freeze so that it
// reads the statement's snapshot. Postgres runs a CTE that locks rows, or
// that the statement reads twice, once in the same way, and one it inlines
// reads the same rows from the snapshot. A CTE read only by SET, RETURNING
// or a subquery that no row reaches does not run in either, so one that
// would fail, such as by a division by zero, fails neither.
//
// Not eager as a SELECT's CTEs are: those run each time their query does,
// once per row for a correlated subquery, while a write's run once for the
// statement.
func (x *sqlExec) writeCTEs(with []sqlir.CTE, body any) error {
	read := reachableWith(with, body)
	for _, cte := range with {
		if !read[cte.Name] {
			continue
		}
		if x.pendingCTEs == nil {
			x.pendingCTEs = map[string]sqlir.CTE{}
		}
		x.pendingCTEs[cte.Name] = cte
	}
	return nil
}

// runPendingCTE runs the pending CTE name, if there is one, before the
// statement reads it.
func (x *sqlExec) runPendingCTE(name string) error {
	cte, ok := x.pendingCTEs[name]
	if !ok {
		return nil
	}
	delete(x.pendingCTEs, name)
	if err := x.evalCTEs([]sqlir.CTE{cte}, map[string]bool{name: true}, nil); err != nil {
		return err
	}
	if x.writeRows == nil {
		x.writeRows, x.writeCols = map[string][]Row{}, map[string][]string{}
	}
	x.writeRows[name], x.writeCols[name] = x.ctes[name], x.cteCols[name]
	delete(x.ctes, name)
	delete(x.cteCols, name)
	return nil
}

// viewRows runs a view's query without the statement's CTEs in scope: its
// names were bound when it was created, so a CTE does not shadow a table it
// reads.
func (x *sqlExec) viewRows(view *sqlir.SelectStmt) ([]string, []Row, error) {
	ctes, cteCols, pending, wrows, wcols := x.ctes, x.cteCols, x.pendingCTEs, x.writeRows, x.writeCols
	defer func() {
		x.ctes, x.cteCols, x.pendingCTEs, x.writeRows, x.writeCols = ctes, cteCols, pending, wrows, wcols
	}()
	x.ctes, x.cteCols, x.pendingCTEs, x.writeRows, x.writeCols = map[string][]Row{}, nil, nil, nil, nil
	return x.evalSelect(view, nil)
}

// cteRows are the rows of the CTE name in scope: a query's, or a write's.
// A query declaring a write's CTE name is refused, so the two never meet.
func (x *sqlExec) cteRows(name string) ([]Row, bool) {
	if rows, ok := x.ctes[name]; ok {
		return rows, true
	}
	rows, ok := x.writeRows[name]
	return rows, ok
}

func (x *sqlExec) cteColumns(name string) ([]string, bool) {
	if cols, ok := x.cteCols[name]; ok {
		return cols, true
	}
	cols, ok := x.writeCols[name]
	return cols, ok
}

// tableRows returns the rows of a FROM item with the alias they are known by.
func (x *sqlExec) tableRows(t sqlir.TableRef, outer *env) (alias string, rows []Row, base bool, err error) {
	alias = t.Alias
	if dup := duplicateName(t.Columns); dup != "" {
		// The rows are keyed by name, so the two columns would read the
		// same value, where Postgres keeps both.
		return "", nil, false, x.unsupported(fmt.Sprintf("column alias %q given twice", dup))
	}
	if t.Sub != nil {
		if names := outputNames(t.Sub); names != nil && len(t.Columns) > len(names) {
			// Postgres fails the list before it runs the query.
			return "", nil, false, x.tx.db.kind.Error(sqlir.InvalidColumnReference, fmt.Sprintf("table %q has %d columns available but %d columns specified", alias, len(names), len(t.Columns)), alias, "", "")
		}
		cols, rows, err := x.evalSelect(t.Sub, outer)
		if err == nil && len(t.Columns) > len(cols) {
			return "", nil, false, x.tx.db.kind.Error(sqlir.InvalidColumnReference, fmt.Sprintf("table %q has %d columns available but %d columns specified", alias, len(cols), len(t.Columns)), alias, "", "")
		}
		if err == nil && len(t.Columns) > 0 {
			rows = renameColumns(rows, cols, t.Columns) // AS alias(a, b)
			cols = renamedCols(cols, t.Columns)
		}
		x.noteCols(t.Sub, cols)
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
	if err := x.runPendingCTE(t.Name); err != nil {
		return "", nil, false, err
	}
	if rows, ok := x.cteRows(t.Name); ok {
		return alias, rows, false, nil
	}
	if err := x.tx.db.checkTable(x.tx.db.resolve(t.Name)); err != nil {
		return "", nil, false, err
	}
	if v := x.tx.db.views[x.tx.db.resolve(t.Name)]; v != nil {
		cols, rows, err := x.viewRows(v.View)
		if err == nil && len(v.ViewColumns) > 0 {
			rows = renameColumns(rows, cols, v.ViewColumns)
			cols = renamedCols(cols, v.ViewColumns)
		}
		x.noteCols(v.View, cols)
		return alias, rows, false, err
	}
	if x.consistent {
		return alias, x.tx.snapshotRows(t.Name), true, nil
	}
	if rows, ok := x.frozen[x.tx.db.resolve(t.Name)]; ok {
		return alias, rows, true, nil
	}
	return alias, x.tx.selectNoYield(t.Name, nil), true, nil
}

// freeze keeps the rows of the tables stmt reads as they are now, its start.
// Postgres reads them from the statement's snapshot, so a subquery, a joined
// table or a row re-checked after a lock wait sees neither the rows other
// transactions committed during the wait nor those the statement wrote
// itself. Only the row a write or a locking read takes is read at its latest.
// InnoDB reads the latest rows.
func (x *sqlExec) freeze(stmt any) {
	if x.tx.db.kind.InnoDB() {
		return
	}
	x.frozen = map[string][]Row{}
	seen := map[string]bool{}
	var add func(n any)
	add = func(n any) {
		for _, name := range sqlir.TableNames(n) {
			table := x.tx.db.resolve(name)
			if seen[table] {
				continue
			}
			seen[table] = true
			if v := x.tx.db.views[table]; v != nil {
				add(v.View)
				continue
			}
			if x.tx.db.defs[table] != nil {
				x.frozen[table] = x.tx.selectNoYield(table, nil)
			}
		}
	}
	add(stmt)
	// The table a write chooses its rows from is a name, not a FROM item.
	switch st := stmt.(type) {
	case *sqlir.UpdateStmt:
		add(sqlir.TableRef{Name: st.Table})
	case *sqlir.DeleteStmt:
		add(sqlir.TableRef{Name: st.Table})
	}
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
		isCTE := x.isCTE(t.Name) || slices.ContainsFunc(sel.With, func(c sqlir.CTE) bool { return c.Name == t.Name })
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
	colls, err := x.keyCollations(keys, vals)
	if err != nil {
		return err
	}
	idx := make([]int, len(rows))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return orderedBefore(keys, colls, vals[idx[a]], vals[idx[b]]) })
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
	ocols := x.outputCols(sel, rows)
	cols := outKeys(ocols)
	var out []Row
	for _, r := range rows {
		o, err := outputRow(ocols, r, func(i int) (any, error) { return x.eval(sel.Targets[i].Expr, r.env(outer)) })
		if err != nil {
			return nil, nil, err
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
	case *sqlir.Collate:
		return hasAggregate(v.X)
	case *sqlir.IsNull:
		return hasAggregate(v.X)
	case *sqlir.InExpr:
		return hasAggregate(v.X) || slices.ContainsFunc(v.List, hasAggregate)
	case *sqlir.ArrayCmp:
		return hasAggregate(v.X)
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
	v, err := x.evalAggRaw(e, g)
	if _, ok := v.(numRange); ok && err == nil {
		return nil, x.errInexact()
	}
	return v, err
}

// evalAggRaw is evalAgg, giving a numRange as it is.
func (x *sqlExec) evalAggRaw(e sqlir.Expr, g *aggEnv) (any, error) {
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
				val, err := x.evalRaw(v.Args[0], r.env(g.env.outer))
				if err != nil {
					return nil, err
				}
				// DISTINCT would tell ranges apart by their bounds, not by
				// the values the server gives.
				if _, ok := val.(numRange); ok && (v.Distinct || (v.Name != "sum" && v.Name != "avg")) {
					return nil, x.errInexact()
				}
				if derefValue(val) == nil {
					continue
				}
				if v.Distinct {
					k := valueKey(val)
					if seen[k] {
						continue
					}
					seen[k] = true
				}
				vals = append(vals, x.aggOperand(v.Name, val))
			}
			// Only min and max order their argument.
			var coll sqlir.Collation
			if v.Name == "min" || v.Name == "max" {
				var err error
				if coll, err = x.orderCollation(v.Args[0]); err == nil {
					coll, err = decide(coll, vals...)
				}
				if err != nil {
					return nil, err
				}
			}
			return x.foldAggregate(v.Name, false, vals, len(g.rows), exprNumberKind(v.Args[0]), coll)
		}
		if sqlir.OtherAggregates[v.Name] {
			return nil, x.unsupported("aggregate " + v.Name)
		}
		if !knownFunc(v.Name, x.tx.db.kind.InnoDB()) {
			return nil, errUnknownExpr{v.Name + "(...)"} // as in evalRaw, before the arguments
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
		if err := x.timeArgs(v, args); err != nil {
			return nil, err
		}
		if v.Name == "round" && len(args) == 1 {
			if out, ok, err := x.roundHalf(v.Args[0], args[0]); ok || err != nil {
				return out, err
			}
		}
		out, err := x.callOrdered(v, args)
		switch v.Name {
		case "coalesce", "greatest", "least", "nullif":
			if err == nil {
				out, err = x.branchValue(v.Args, args, out)
			}
		}
		return out, err
	case *sqlir.BinaryExpr:
		if v.Op == "AND" || v.Op == "OR" {
			// Three-valued, as eval does for a row.
			decides := v.Op == "OR"
			l, err := x.evalAgg(v.L, g)
			if err != nil {
				return nil, err
			}
			lb, lok := derefValue(l).(bool)
			if lok && lb == decides {
				return decides, nil
			}
			r, err := x.evalAgg(v.R, g)
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
		l, err := x.evalAggRaw(v.L, g)
		if err != nil {
			return nil, err
		}
		r, err := x.evalAggRaw(v.R, g)
		if err != nil {
			return nil, err
		}
		if isRange(l) || isRange(r) {
			return x.rangeBinary(v.Op, l, r)
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
		if v.Op == "||" {
			l, r = paramText(v.L, l), paramText(v.R, r)
		}
		if out, ok, err := x.orderedOperands(v, l, r); ok || err != nil {
			return out, err
		}
		res, err := x.binary(v.Op, l, r)
		if err != nil {
			return nil, err
		}
		return x.arithValue(v, l, r, res), nil
	case *sqlir.RowExpr:
		vals := make([]any, len(v.Items))
		for i, it := range v.Items {
			val, err := x.evalAgg(it, g)
			if err != nil {
				return nil, err
			}
			vals[i] = val
		}
		return vals, nil
	case *sqlir.Collate:
		val, err := x.evalAggRaw(v.X, g)
		return paramText(v.X, val), err
	case *sqlir.Cast:
		val, err := x.evalAgg(v.X, g)
		if err != nil {
			return nil, err
		}
		if err := x.byteaCast(v, val); err != nil {
			return nil, err
		}
		if err := x.paramTextCast(v, val); err != nil {
			return nil, err
		}
		if err := x.boolCastSource(v); err != nil {
			return nil, err
		}
		return x.castTo(v, x.halfToInteger(v, paramBool(v, val)))
	}
	if hasAggregate(e) {
		// eval would take the aggregate for a function of one row.
		return nil, x.unsupported("an aggregate inside NOT, CASE, IS NULL or IN")
	}
	return x.eval(e, g.env)
}

// --- INSERT ---

func (x *sqlExec) execInsert(ins *sqlir.InsertStmt) (*sqlResult, error) {
	table := x.tx.db.resolve(ins.Table)
	if err := x.tx.db.checkTable(table); err != nil {
		return nil, err
	}
	x.freeze(ins)
	if ins.OnConflict != nil && x.tx.db.defs[table] != nil {
		// Postgres settles the arbiters when it plans the statement, so a
		// target that names none fails before any row or default is made.
		if _, err := x.conflictTargets(table, ins.OnConflict); err != nil {
			return nil, err
		}
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
	if err := x.writesDeferrableKey(table, nil); err != nil {
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
				v, err := x.evalRaw(e, &env{})
				if err != nil {
					return nil, x.unsupportedExpr(err, "in VALUES")
				}
				if _, isDefault := e.(*sqlir.Default); isDefault {
					continue
				}
				if v, err = x.columnValue(table, cols[i], v); err != nil {
					return nil, err
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
	out := x.returningResult(ins.Returning, all, alias)
	type autoID struct {
		id        int64
		generated bool
	}
	ids := make([]autoID, len(rows))
	// Postgres refuses a DO UPDATE of a row this statement already inserted
	// or updated, where MySQL's ON DUPLICATE KEY UPDATE applies it again.
	written := map[string]bool{}
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
	// InnoDB takes the table's IX before any of the rows' duplicate checks.
	x.tx.noteTableLock(table, lockUpdate)
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
			if existing, err = x.findConflict(table, ins.OnConflict, row); err != nil {
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
			again, err := x.findConflict(table, ins.OnConflict, row)
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
					x.tx.note("on conflict do nothing: row skipped")
				}
				continue
			}
			if !x.tx.db.kind.InnoDB() && written[lk.key] {
				return nil, x.tx.db.kind.Error(sqlir.CardinalityViolation, "ON CONFLICT DO UPDATE command cannot affect row a second time", relname(table), "", "")
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
						x.tx.note("on conflict do update: WHERE false, row skipped")
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
			// InnoDB writes the undo record as it changes the clustered
			// record, before the secondary indexes' checks, which may wait.
			if !sameRow(cur, updated) {
				x.tx.undo++
			}
			x.tx.beginUpdate(lk, updated)
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
			written[lk.key] = true
			x.tx.endUpdates()
			if x.tx.p != nil {
				x.tx.note("on conflict do update: %s", updated)
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
		if noPK && len(x.tx.conflicting(lk, lockUpdate)) > 0 && (keyedByID || !x.inUniqueIndex(table, row)) {
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
		if err := x.tx.lockImplicit(lk, lockStruct{}); err != nil {
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
			// The row lock waited for a writer that committed the key.
			x.tx.shareDuplicate(lk, structKey(table, "PRIMARY", lockShare, "record"))
			return nil, x.tx.db.duplicateKey(table, x.tx.db.pkConstraint(table))
		}
		// InnoDB writes the undo record as it puts the row into the primary
		// key, before it checks the other unique indexes and the foreign
		// keys, so a wait in those checks weighs it already.
		x.tx.undo++
		if x.tx.db.kind.InnoDB() {
			x.tx.put = append(x.tx.put, putRow{table: table, row: row})
			x.tx.putting = true
		}
		if err := x.insertEntries(table, row); err != nil {
			return nil, err
		}
		// Last before the write, as the duplicate checks above may wait
		// while another transaction takes a gap lock the row falls into.
		if err := x.tx.insertIntention(table, row); err != nil {
			return nil, err
		}
		delete(x.tx.deleted, lk)
		x.tx.writes[lk] = row
		written[lk.key] = true
		x.tx.putting = false
		if x.tx.db.kind.InnoDB() {
			x.tx.inserts = append(x.tx.inserts, putRow{table: table, row: row})
		}
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
func (x *sqlExec) findConflict(table string, oc *sqlir.OnConflict, row Row) (Row, error) {
	cols := oc.Columns
	if x.tx.db.defs[table] != nil {
		targets, err := x.conflictTargets(table, oc)
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

// returningResult is the result of a write with RETURNING ret, all being
// the columns a * gives.
func (x *sqlExec) returningResult(ret []sqlir.Target, all []string, alias string) *sqlResult {
	return &sqlResult{cols: x.returningCols(ret, all), star: all, alias: alias}
}

// tableCols are the declared columns of table, in order.
func (x *sqlExec) tableCols(table string) []string {
	if def := x.tx.db.defs[table]; def != nil {
		return def.columns
	}
	return nil
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
	return x.appendReturningIn(out, ret, row, &env{tables: map[string]Row{out.alias: row}, merged: row})
}

// appendReturningIn evaluates RETURNING in e, which for UPDATE ... FROM and
// DELETE ... USING holds the joined rows besides the row written.
func (x *sqlExec) appendReturningIn(out *sqlResult, ret []sqlir.Target, row Row, e *env) error {
	if len(ret) == 0 {
		return nil
	}
	vals := make([]driver.Value, 0, len(out.cols))
	for _, t := range ret {
		if t.Star {
			for _, c := range out.star {
				vals = append(vals, toDriverValue(row[c]))
			}
			continue
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
	// The rows the statement began with, once freeze has kept them: a CTE
	// that waits for a lock runs before the rows are chosen, and Postgres
	// chooses them from the statement's snapshot, reading a chosen row at its
	// latest only once it locks it.
	base, ok := x.frozen[x.tx.db.resolve(table)]
	if !ok {
		base = x.tx.selectNoYield(table, nil)
	}
	var rows []jrow
	for _, r := range base {
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
		return x.returningResult(up.Returning, x.tableCols(table), ""), nil // nothing to update
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
	if err := x.writesDeferrableKey(table, assignedColumns(up.Set)); err != nil {
		return nil, err
	}
	if err := x.writesGenerated(table, assignedColumns(up.Set), assignedValues(up.Set), false); err != nil {
		return nil, err // before WHERE is evaluated, which may have effects
	}
	// A WHERE that reads the CTEs cannot be tried before they run, which is
	// after the statement's scheduling point.
	if len(up.With) == 0 {
		if _, err := x.writeCandidates(up.Table, up.Alias, nil, up.Where); err != nil && len(up.From) == 0 {
			return nil, err
		}
	}
	preview := lazyString(func() string {
		row := Row{}
		for _, a := range up.Set {
			// A value computed from the row it updates is not known before
			// the row is read, and one with effects, such as nextval's or a
			// subquery's locks, must not happen for the trace, so the trace
			// shows the expression instead.
			if len(sqlir.ColumnRefs(a.Value)) > 0 || hasEffects(a.Value) {
				row[a.Column] = x.exprString(a.Value)
				continue
			}
			v, err := x.eval(a.Value, &env{})
			if err != nil {
				v = sqlir.Unknown
			}
			row[a.Column] = v
		}
		return row.String()
	})
	x.tx.yieldf("%s: update %s set %s where %s", x.tx.db.name, up.Table, preview, lazyString(func() string { return x.exprString(up.Where) }))
	x.freeze(up)
	body := *up
	body.With = nil
	if err := x.writeCTEs(up.With, &body); err != nil {
		return nil, err
	}
	var stop *scanStop
	if len(up.From) == 0 {
		var err error
		if stop, err = x.stopAt(up.OrderBy, up.Limit, nil); err != nil {
			return nil, err
		}
	}
	out := x.returningResult(up.Returning, x.tableCols(table), alias)
	// MySQL's single-table UPDATE assigns left to right, each assignment
	// seeing the ones before it, where Postgres evaluates them all against the
	// old row.
	sequential := x.tx.db.kind.InnoDB() && len(up.From) == 0
	done := map[string]bool{}
	var matched int64 // LIMIT counts the rows matched, changed or not
	// apply updates the row of c if it still matches, once it is locked.
	apply := func(c jrow) error {
		key := c.base.Key()
		c, joined, err := x.lockJoinedReads(up.From, c)
		if err != nil {
			return err
		}
		if !joined {
			return nil // a joined row it waited for is gone
		}
		mode := x.tx.db.updateLock(table, assignedColumns(up.Set))
		key, cur, ok, err := x.tx.lockLatest(table, key, func(lk lockKey) error { return x.tx.lockMode(lk, mode) })
		if err != nil {
			return err
		}
		if !ok || done[key] {
			return nil
		}
		lk := lockKey{table, key}
		// Re-evaluate the predicate on the version visible after the lock, as
		// Postgres Read Committed does.
		c2 := c.rebind(alias, cur)
		if up.Where != nil {
			ok, err := x.evalBool(up.Where, c2.env(nil))
			if err != nil || !ok {
				return err
			}
		}
		done[key] = true
		updated := cur.clone()
		for _, a := range up.Set {
			v, err := x.assignedValue(table, a, c2.env(nil), "in SET")
			if err != nil {
				return err
			}
			updated[a.Column] = v
			if sequential {
				c2 = c2.rebind(alias, updated)
			}
		}
		if err := x.touchOnUpdate(table, cur, updated, up.Set); err != nil {
			return err
		}

		if err := x.checkRow(table, updated); err != nil {
			return err
		}
		// InnoDB writes the undo record as it changes the clustered record,
		// before the secondary indexes' checks, which may wait; none for an
		// update that changes nothing.
		if !sameRow(cur, updated) {
			x.tx.undo++
		}
		x.tx.beginUpdate(lk, updated)
		defer x.tx.endUpdates()
		if err := x.checkUniques(table, updated, key, cur); err != nil {
			return err
		}
		if err := x.checkParents(table, updated, cur); err != nil {
			return err
		}
		if err := x.onParentUpdate(table, cur, updated); err != nil {
			return err
		}
		lk, err = x.rekey(table, lk, updated)
		if err != nil {
			return err
		}
		// Last before the write, as the checks above may wait while another
		// transaction takes a gap lock the new value falls into.
		if err := x.tx.moveIntention(table, updated, cur); err != nil {
			return err
		}
		x.tx.writes[lk] = updated
		matched++
		if !x.tx.db.kind.InnoDB() || !sameRow(cur, updated) {
			// MySQL counts the rows an UPDATE changed, as go-sql-driver
			// reports them without clientFoundRows.
			out.affected++
		}
		return x.appendReturningIn(out, up.Returning, updated, c.rebind(alias, updated).env(nil))
	}
	changed := assignedColumns(up.Set)
	if def := x.tx.db.defs[table]; def != nil {
		for c := range def.onUpdate {
			changed = append(changed, c) // ON UPDATE CURRENT_TIMESTAMP changes it too
		}
	}
	if x.rowByRow(table, alias, up.Where, len(up.From) == 0 && len(up.OrderBy) == 0, changed) {
		n, err := x.count(up.Limit, "LIMIT", nil)
		if err != nil {
			return nil, err
		}
		x.scanned = func(t string, r Row) error {
			if t != table || done[r.Key()] || up.Limit != nil && matched >= int64(n) {
				return nil
			}
			return apply(newJrow(alias, r))
		}
	}
	err := x.nextKeyLocks(table, alias, up.Where, lockUpdate, nil, stop)
	x.scanned = nil
	if err != nil {
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
	for _, c := range cands {
		if done[c.base.Key()] {
			continue
		}
		if limit >= 0 && matched >= int64(limit) {
			break
		}
		if err := apply(c); err != nil {
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
		return x.returningResult(del.Returning, x.tableCols(x.tx.db.resolve(del.Table)), ""), nil // nothing to delete
	}
	x.tx.yieldf("%s: delete %s where %s", x.tx.db.name, del.Table, lazyString(func() string { return x.exprString(del.Where) }))
	x.freeze(del)
	body := *del
	body.With = nil
	if err := x.writeCTEs(del.With, &body); err != nil {
		return nil, err
	}
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
	table := x.tx.db.resolve(del.Table)
	alias := delAlias
	out := x.returningResult(del.Returning, x.tableCols(table), alias)
	done := map[string]bool{}
	// apply deletes the row of c if it still matches, once it is locked.
	apply := func(c jrow) error {
		key, cur, ok, err := x.tx.lockLatest(table, c.base.Key(), x.tx.lock)
		if err != nil {
			return err
		}
		if !ok || done[key] {
			return nil
		}
		lk := lockKey{table, key}
		if del.Where != nil {
			c2 := c.rebind(alias, cur)
			ok, err := x.evalBool(del.Where, c2.env(nil))
			if err != nil || !ok {
				return err
			}
		}
		done[key] = true
		x.tx.undo++ // written before the secondary entries, which may wait
		if err := x.releaseEntries(table, cur, nil); err != nil {
			return err
		}
		delete(x.tx.writes, lk)
		x.tx.deleted[lk] = true
		if err := x.onParentDelete(table, cur); err != nil {
			return err
		}
		out.affected++
		return x.appendReturningIn(out, del.Returning, cur, c.rebind(alias, cur).env(nil))
	}
	if x.rowByRow(table, alias, del.Where, len(del.Using) == 0 && len(del.OrderBy) == 0, nil) {
		n, err := x.count(del.Limit, "LIMIT", nil)
		if err != nil {
			return nil, err
		}
		x.scanned = func(t string, r Row) error {
			if t != table || done[r.Key()] || del.Limit != nil && out.affected >= int64(n) {
				return nil
			}
			return apply(newJrow(alias, r))
		}
	}
	err := x.nextKeyLocks(table, alias, del.Where, lockUpdate, nil, stop)
	x.scanned = nil
	if err != nil {
		return nil, err
	}
	cands, err := x.writeCandidates(del.Table, del.Alias, del.Using, del.Where)
	if err != nil {
		return nil, err
	}
	if err := x.inScanOrder(table, alias, del.Where, stop, cands); err != nil {
		return nil, err
	}
	limit, err := x.writeLimit(cands, del.OrderBy, del.Limit)
	if err != nil {
		return nil, err
	}
	for _, c := range cands {
		if done[c.base.Key()] {
			continue
		}
		if limit >= 0 && out.affected >= int64(limit) {
			break
		}
		if err := apply(c); err != nil {
			return nil, err
		}
	}
	return out, nil
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

func toDriverValue(v any) driver.Value {
	v = derefValue(v)
	switch x := v.(type) {
	case nil, int64, float64, bool, []byte, string, time.Time:
		return x
	case instant:
		return x.Time
	case pgInterval:
		return x.String()
	case uuidValue:
		return string(x)
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

// duplicateName returns a name given more than once in names, or "".
func duplicateName(names []string) string {
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			return n
		}
		seen[n] = true
	}
	return ""
}
