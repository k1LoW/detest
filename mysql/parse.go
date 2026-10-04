package mysql

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
	tidb "github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"

	// The parser needs an implementation of literal and parameter nodes; the
	// test driver is the one meant for using the parser on its own, and its
	// importing registers it.
	"github.com/pingcap/tidb/pkg/parser/test_driver"
)

// parser parses with the MySQL grammar of TiDB's parser and converts the AST
// into detest's IR.
type parser struct{}

func (parser) Name() string { return "mysql" }

func (parser) Parse(query string) (sqlir.Statement, error) {
	stmts, _, err := tidb.New().ParseSQL(query)
	if err != nil {
		return nil, &sqlir.ParseError{Query: query, Err: err}
	}
	c := &conv{query: query, params: paramIndexes(stmts)}
	if len(stmts) != 1 {
		return c.script(stmts)
	}
	return c.stmt(stmts[0])
}

// conv converts one query's AST.
type conv struct {
	query string
	// rowAlias is the alias INSERT ... AS alias gives the proposed row, which
	// ON DUPLICATE KEY UPDATE refers to as the excluded row.
	rowAlias string
	// cteNames are the CTEs the statement declares, by lower-case name.
	cteNames map[string]bool
	// inLimit is set while LIMIT and OFFSET convert.
	inLimit bool
	// params numbers the ? placeholders in the order they appear.
	params map[*test_driver.ParamMarkerExpr]int
}

// paramCollector gathers the placeholders of a statement.
type paramCollector struct {
	found []*test_driver.ParamMarkerExpr
}

func (v *paramCollector) Enter(n ast.Node) (ast.Node, bool) {
	if p, ok := n.(*test_driver.ParamMarkerExpr); ok {
		v.found = append(v.found, p)
	}
	return n, false
}

func (v *paramCollector) Leave(n ast.Node) (ast.Node, bool) { return n, true }

// paramIndexes numbers the placeholders by their position in the query,
// which is the order database/sql binds the arguments in.
func paramIndexes(stmts []ast.StmtNode) map[*test_driver.ParamMarkerExpr]int {
	v := &paramCollector{}
	for _, s := range stmts {
		s.Accept(v)
	}
	sort.SliceStable(v.found, func(i, j int) bool { return v.found[i].Offset < v.found[j].Offset })
	out := make(map[*test_driver.ParamMarkerExpr]int, len(v.found))
	for i, p := range v.found {
		out[p] = i
	}
	return out
}

func (c *conv) unsupported(what string) error { return sqlir.Unsupported(what, c.query) }

// script converts several statements, as a migration or a dump holds. BEGIN
// and COMMIT are refused there as alone: each statement of a script runs in
// its own transaction, so skipping them would commit what they enclose
// piecemeal.
func (c *conv) script(stmts []ast.StmtNode) (sqlir.Statement, error) {
	out := &sqlir.Script{}
	var locked map[string]bool // the tables a dump write-locked to load them, nil outside
	for _, s := range stmts {
		c.cteNames = nil // each statement declares its own
		switch a := s.(type) {
		case *ast.LockTablesStmt:
			if locked != nil {
				return nil, c.unsupported("LOCK TABLES inside LOCK TABLES")
			}
			locked = map[string]bool{}
			for _, l := range a.TableLocks {
				if l.Type != ast.TableLockWrite {
					// A dump write-locks the tables it loads; another lock
					// is code relying on table locks, which detest does not
					// model.
					return nil, c.unsupported("LOCK TABLES other than WRITE")
				}
				locked[strings.ToLower(tableName(l.Table))] = true
			}
			continue
		case *ast.UnlockTablesStmt:
			locked = nil
			continue
		case *ast.InsertStmt:
			if locked != nil {
				t, ok := a.Table.TableRefs.Left.(*ast.TableSource)
				name, isName := (*ast.TableName)(nil), false
				if ok {
					name, isName = t.Source.(*ast.TableName)
				}
				if !isName || !locked[strings.ToLower(tableName(name))] {
					return nil, c.unsupported("LOCK TABLES around an INSERT into a table it does not lock")
				}
			}
		case *ast.AlterTableStmt:
			if keysOnly(a) {
				continue // a dump's DISABLE KEYS and ENABLE KEYS, which change no data
			}
			if locked != nil {
				return nil, c.unsupported("LOCK TABLES around statements other than INSERT")
			}
		default:
			if locked != nil {
				// A dump locks tables only around the rows it loads. Code
				// that locks them around anything else relies on the lock,
				// which detest does not model.
				return nil, c.unsupported("LOCK TABLES around statements other than INSERT")
			}
		}
		st, err := c.stmt(s)
		if err != nil {
			return nil, err
		}
		out.Stmts = append(out.Stmts, st)
	}
	if locked != nil {
		return nil, c.unsupported("LOCK TABLES without UNLOCK TABLES")
	}
	return out, nil
}

// keysOnly reports an ALTER TABLE that only disables or enables the
// updating of nonunique indexes, which a dump wraps its rows in. InnoDB
// ignores both.
func keysOnly(s *ast.AlterTableStmt) bool {
	for _, sp := range s.Specs {
		if sp.Tp != ast.AlterTableEnableKeys && sp.Tp != ast.AlterTableDisableKeys {
			return false
		}
	}
	return len(s.Specs) > 0
}

func (c *conv) stmt(n ast.StmtNode) (sqlir.Statement, error) {
	switch s := n.(type) {
	case *ast.SelectStmt, *ast.SetOprStmt:
		return c.query1(n)
	case *ast.InsertStmt:
		return c.insert(s)
	case *ast.UpdateStmt:
		return c.update(s)
	case *ast.DeleteStmt:
		return c.delete(s)
	case *ast.SavepointStmt:
		return &sqlir.SavepointStmt{Op: "savepoint", Name: s.Name}, nil
	case *ast.ReleaseSavepointStmt:
		return &sqlir.SavepointStmt{Op: "release", Name: s.Name}, nil
	case *ast.RollbackStmt:
		if s.SavepointName != "" {
			return &sqlir.SavepointStmt{Op: "rollback_to", Name: s.SavepointName}, nil
		}
		return nil, c.unsupported("ROLLBACK as a statement (use database/sql's transactions)")
	case *ast.BeginStmt, *ast.CommitStmt:
		return nil, c.unsupported("BEGIN or COMMIT as a statement (use database/sql's transactions)")
	case *ast.SetStmt:
		return c.set(s)
	case *ast.CreateTableStmt:
		if s.TemporaryKeyword != ast.TemporaryNone {
			// A temporary table is the session's own, where detest keeps
			// one schema for every connection.
			return nil, c.unsupported("CREATE TEMPORARY TABLE")
		}
		if s.Select != nil {
			return c.createTableAs(s)
		}
		ch, err := c.createTable(s)
		if err != nil {
			return nil, err
		}
		return &sqlir.SchemaStmt{Changes: []sqlir.SchemaChange{ch}}, nil
	case *ast.AlterTableStmt:
		chs, err := c.alterTable(s)
		if err != nil {
			return nil, err
		}
		return &sqlir.SchemaStmt{Changes: chs}, nil
	case *ast.DropTableStmt:
		if s.TemporaryKeyword != ast.TemporaryNone {
			return nil, c.unsupported("DROP TEMPORARY TABLE")
		}
		out := &sqlir.SchemaStmt{}
		for _, t := range s.Tables {
			ch := sqlir.SchemaChange{Table: tableName(t), Drop: true, IfExists: s.IfExists}
			if s.IsView {
				ch.Object = "view"
			}
			out.Changes = append(out.Changes, ch)
		}
		return out, nil
	case *ast.CreateIndexStmt:
		ch, err := c.createIndex(s)
		if err != nil {
			return nil, err
		}
		return &sqlir.SchemaStmt{Changes: []sqlir.SchemaChange{ch}}, nil
	case *ast.DropIndexStmt:
		return &sqlir.SchemaStmt{Changes: []sqlir.SchemaChange{{Table: tableName(s.Table), DropIndexes: []string{s.IndexName}}}}, nil
	case *ast.RenameTableStmt:
		out := &sqlir.SchemaStmt{}
		for _, tt := range s.TableToTables {
			out.Changes = append(out.Changes, sqlir.SchemaChange{Table: tableName(tt.OldTable), RenameTo: tableName(tt.NewTable)})
		}
		return out, nil
	case *ast.CreateViewStmt:
		return c.createView(s)
	case *ast.TruncateTableStmt:
		return &sqlir.DeleteStmt{Table: tableName(s.Table), Truncate: true}, nil
	case *ast.UseStmt:
		return &sqlir.SetStmt{Name: "database", Value: s.DBName}, nil
	case *ast.LockTablesStmt, *ast.UnlockTablesStmt:
		// Table locks are not modeled, so code that takes them to keep
		// others out would run as if it had none.
		return nil, c.unsupported("LOCK TABLES")
	case *ast.CreateDatabaseStmt, *ast.DropDatabaseStmt,
		*ast.GrantStmt, *ast.CreateUserStmt, *ast.AlterDatabaseStmt:
		// A dump or a migration carries these; they declare nothing detest needs.
		return &sqlir.SchemaStmt{}, nil
	}
	return nil, c.unsupported(fmt.Sprintf("%T", n))
}

// actedOn are the variables a SET changes detest's behavior with.
var actedOn = map[string]bool{
	"innodb_lock_wait_timeout": true, "foreign_key_checks": true, "sql_mode": true,
	"autocommit": true, "tx_isolation": true, "transaction_isolation": true, "tx_read_only": true, "transaction_read_only": true,
}

// set converts SET. detest acts on innodb_lock_wait_timeout; the others, such
// as SET NAMES or the settings a dump makes, do nothing.
func (c *conv) set(s *ast.SetStmt) (sqlir.Statement, error) {
	out := &sqlir.SetStmt{}
	for _, v := range s.Variables {
		if v.IsGlobal && actedOn[strings.ToLower(v.Name)] {
			// The global value is for the sessions that start later, which
			// detest does not model, so applying it to this one would change
			// the running session where MySQL leaves it alone.
			return nil, c.unsupported("SET GLOBAL " + v.Name)
		}
		switch strings.ToLower(v.Name) {
		case "autocommit", "tx_isolation", "tx_isolation_one_shot", "transaction_isolation", "tx_read_only", "transaction_read_only":
			// These change what a transaction is, which detest takes from
			// database/sql's BeginTx and mysql.Isolation instead.
			if e, err := c.expr(v.Value); err == nil && strings.EqualFold(v.Name, "autocommit") {
				if k, ok := e.(*sqlir.Const); ok && mysqlTrue(k.Value) {
					continue // autocommit on, as it is
				}
			}
			return nil, c.unsupported("SET " + v.Name)
		}
		if strings.EqualFold(v.Name, "foreign_key_checks") {
			// A dump turns the checks off while it loads tables in name
			// order, which may put a child before its parent, and restores
			// them from the variable it saved them in.
			on := true
			if _, restore := v.Value.(*ast.VariableExpr); !restore {
				e, err := c.expr(v.Value)
				k, ok := e.(*sqlir.Const)
				if err != nil || !ok {
					return nil, c.unsupported("foreign_key_checks set to a value other than a constant")
				}
				on = mysqlTrue(k.Value)
			}
			if out.Name != "" {
				return nil, c.unsupported("SET of more than one setting detest acts on")
			}
			out = &sqlir.SetStmt{Name: "foreign_key_checks", Value: strconv.FormatBool(on)}
			continue
		}
		if strings.EqualFold(v.Name, "sql_mode") {
			// detest keeps strict mode whatever the setting; of the modes a
			// dump sets, NO_AUTO_VALUE_ON_ZERO decides the keys it loads.
			mode := ""
			if _, restore := v.Value.(*ast.VariableExpr); !restore {
				// A dump restores the mode it saved from a variable, which
				// leaves the default; any other value must be a constant.
				e, err := c.expr(v.Value)
				k, ok := e.(*sqlir.Const)
				if err != nil || !ok {
					return nil, c.unsupported("sql_mode set to a value other than a constant")
				}
				mode = fmt.Sprint(k.Value)
				modes := strings.Split(strings.ToUpper(mode), ",")
				for i := range modes {
					modes[i] = strings.TrimSpace(modes[i])
				}
				strict := slices.ContainsFunc(modes, func(m string) bool {
					return m == "STRICT_TRANS_TABLES" || m == "STRICT_ALL_TABLES" || m == "TRADITIONAL"
				})
				// detest keeps strict mode's errors. A mode without it would
				// turn them into warnings and store coerced values, which
				// detest does not do, except for the one mysqldump sets while
				// it loads rows that are valid anyway.
				if !strict && !slices.Equal(modes, []string{"NO_AUTO_VALUE_ON_ZERO"}) {
					return nil, c.unsupported("sql_mode without strict mode")
				}
			}
			for m := range strings.SplitSeq(strings.ToUpper(mode), ",") {
				switch strings.TrimSpace(m) {
				case "NO_BACKSLASH_ESCAPES", "ANSI_QUOTES", "ANSI", "PIPES_AS_CONCAT", "REAL_AS_FLOAT", "IGNORE_SPACE",
					"HIGH_NOT_PRECEDENCE", "PAD_CHAR_TO_FULL_LENGTH", "NO_UNSIGNED_SUBTRACTION", "TIME_TRUNCATE_FRACTIONAL":
					// These change how statements parse or evaluate, which
					// detest does not follow.
					return nil, c.unsupported("sql_mode " + strings.TrimSpace(m))
				}
			}
			if out.Name != "" {
				return nil, c.unsupported("SET of more than one setting detest acts on")
			}
			noAutoZero := slices.ContainsFunc(strings.Split(strings.ToUpper(mode), ","), func(m string) bool { return strings.TrimSpace(m) == "NO_AUTO_VALUE_ON_ZERO" })
			out = &sqlir.SetStmt{Name: "no_auto_value_on_zero", Value: strconv.FormatBool(noAutoZero)}
			continue
		}
		if strings.EqualFold(v.Name, "innodb_lock_wait_timeout") {
			// A session setting, unlike Postgres's SET LOCAL.
			e, err := c.expr(v.Value)
			k, ok := e.(*sqlir.Const)
			if err != nil || !ok {
				// detest decides at parse time whether lock waits may time
				// out, so it cannot take a value bound later.
				return nil, c.unsupported("innodb_lock_wait_timeout set to a value other than a constant")
			}
			n, isInt := k.Value.(int64)
			if !isInt {
				// MySQL refuses another type of value (1232).
				return nil, c.unsupported("innodb_lock_wait_timeout set to a value other than an integer")
			}
			if out.Name != "" {
				// One statement carries one setting to the session.
				return nil, c.unsupported("SET of more than one setting detest acts on")
			}
			// MySQL raises a value below the minimum of 1 second to it, with
			// a warning, so waits may still time out.
			out = &sqlir.SetStmt{Name: "lock_timeout", Value: strconv.FormatInt(max(n, 1), 10)}
		}
	}
	return out, nil
}

// mysqlTrue reports whether a constant turns a setting on: 1, ON or TRUE.
func mysqlTrue(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "on") || strings.EqualFold(v, "true") || v == "1"
	}
	f, ok := v.(int64)
	return ok && f == 1
}

// replaceAliases replaces the unqualified column references in e that name a
// select alias with the expression the alias names. It walks the expression
// by reflection, as sqlir.ColumnRefs does, so it needs no case for each kind
// of node.
func replaceAliases(e sqlir.Expr, aliases map[string]sqlir.Expr) sqlir.Expr {
	if len(aliases) == 0 || e == nil {
		return e
	}
	if c, ok := e.(*sqlir.ColumnRef); ok {
		if a, ok := aliases[c.Column]; ok && c.Table == "" {
			return a
		}
		return e
	}
	exprType := reflect.TypeFor[sqlir.Expr]()
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Interface:
			if v.IsNil() {
				return
			}
			if x, ok := reflect.TypeAssert[sqlir.Expr](v); ok && v.Type() == exprType && v.CanSet() {
				v.Set(reflect.ValueOf(replaceAliases(x, aliases)))
				return
			}
			walk(v.Elem())
		case reflect.Pointer:
			// A subquery is a scope of its own, where the aliases do not
			// reach.
			if v.IsNil() || v.Type() == reflect.TypeFor[*sqlir.SelectStmt]() {
				return
			}
			walk(v.Elem())
		case reflect.Struct:
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i))
				}
			}
		case reflect.Slice:
			for i := range v.Len() {
				walk(v.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(e))
	return e
}

// hasAggregate reports whether a query's select list aggregates, by the
// aggregates of its own block: one in a subquery or one a window computes
// does not make the query one group.
func hasAggregate(sel *sqlir.SelectStmt) bool {
	for _, t := range sel.Targets {
		if slices.ContainsFunc(sqlir.BlockFuncCalls(t.Expr), func(f *sqlir.FuncCall) bool {
			return f.Star || slices.Contains([]string{"count", "sum", "min", "max", "avg"}, f.Name)
		}) {
			return true
		}
	}
	return false
}

func tableName(t *ast.TableName) string {
	if t.Schema.O != "" {
		return t.Schema.O + "." + t.Name.O
	}
	return t.Name.O
}

// query1 converts a SELECT, a set operation or VALUES.
func (c *conv) query1(n ast.Node) (*sqlir.SelectStmt, error) {
	switch s := n.(type) {
	case *ast.SelectStmt:
		return c.selectStmt(s)
	case *ast.SetOprStmt:
		return c.setOpr(s)
	case *ast.SubqueryExpr:
		return c.query1(s.Query)
	}
	return nil, c.unsupported(fmt.Sprintf("query %T", n))
}

func (c *conv) setOpr(s *ast.SetOprStmt) (*sqlir.SelectStmt, error) {
	if s.SelectList == nil || len(s.SelectList.Selects) == 0 {
		return nil, c.unsupported("empty set operation")
	}
	// The CTEs come first, so the branches resolve the names they declare.
	defer func(names map[string]bool) { c.cteNames = names }(maps.Clone(c.cteNames))
	ctes, err := c.with(s.With)
	if err != nil {
		return nil, err
	}
	var out *sqlir.SelectStmt
	for i, n := range s.SelectList.Selects {
		sel, err := c.query1(n)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			out = sel
			continue
		}
		var op *ast.SetOprType
		switch v := n.(type) {
		case *ast.SelectStmt:
			op = v.AfterSetOperator
		case *ast.SetOprSelectList:
			op = v.AfterSetOperator
		}
		if op == nil {
			return nil, c.unsupported("set operation")
		}
		combined := &sqlir.SelectStmt{Larg: out, Rarg: sel}
		switch *op {
		case ast.Union:
			combined.SetOp = "union"
		case ast.UnionAll:
			combined.SetOp, combined.SetAll = "union", true
		case ast.Intersect:
			combined.SetOp = "intersect"
		case ast.IntersectAll:
			combined.SetOp, combined.SetAll = "intersect", true
		case ast.Except:
			combined.SetOp = "except"
		case ast.ExceptAll:
			combined.SetOp, combined.SetAll = "except", true
		default:
			return nil, c.unsupported("set operation")
		}
		out = combined
	}
	if ctes != nil {
		out.With = ctes
	}
	if out.OrderBy, err = c.orderBy(s.OrderBy); err != nil {
		return nil, err
	}
	if out.Limit, out.Offset, err = c.limit(s.Limit); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *conv) with(w *ast.WithClause) ([]sqlir.CTE, error) {
	if w == nil {
		return nil, nil
	}
	if w.IsRecursive {
		return nil, c.unsupported("recursive CTE")
	}
	var out []sqlir.CTE
	for _, cte := range w.CTEs {
		if len(cte.ColNameList) > 0 {
			return nil, c.unsupported("CTE column list")
		}
		sel, err := c.query1(cte.Query)
		if err != nil {
			return nil, err
		}
		// MySQL's CTE names are case-insensitive; references to them are
		// lowered to match (tableRef).
		if c.cteNames == nil {
			c.cteNames = map[string]bool{}
		}
		c.cteNames[cte.Name.L] = true
		out = append(out, sqlir.CTE{Name: cte.Name.L, Select: sel})
	}
	return out, nil
}

func (c *conv) selectStmt(s *ast.SelectStmt) (*sqlir.SelectStmt, error) {
	// The CTEs a query block declares are its own and its nested blocks'.
	defer func(names map[string]bool) { c.cteNames = names }(maps.Clone(c.cteNames))
	out := &sqlir.SelectStmt{Distinct: s.Distinct}
	var err error
	if out.With, err = c.with(s.With); err != nil {
		return nil, err
	}
	if s.Kind == ast.SelectStmtKindValues {
		for _, row := range s.Lists {
			var items []sqlir.Expr
			for _, v := range row.Values {
				e, err := c.expr(v)
				if err != nil {
					return nil, err
				}
				items = append(items, e)
			}
			out.Values = append(out.Values, items)
		}
		return out, nil
	}
	if s.Kind != ast.SelectStmtKindSelect {
		return nil, c.unsupported("TABLE statement")
	}
	if s.SelectIntoOpt != nil {
		return nil, c.unsupported("SELECT INTO")
	}
	if len(s.WindowSpecs) > 0 {
		return nil, c.unsupported("WINDOW clause")
	}
	if s.Fields != nil {
		for _, f := range s.Fields.Fields {
			if f.WildCard != nil {
				out.Targets = append(out.Targets, sqlir.Target{Star: true, Table: f.WildCard.Table.L}) // aliases are lower case
				continue
			}
			e, err := c.expr(f.Expr)
			if err != nil {
				return nil, err
			}
			out.Targets = append(out.Targets, sqlir.Target{Expr: e, Alias: f.AsName.O})
		}
	}
	if s.From != nil {
		if out.From, out.Joins, err = c.from(s.From.TableRefs); err != nil {
			return nil, err
		}
	}
	if s.Where != nil {
		if out.Where, err = c.cond(s.Where); err != nil {
			return nil, err
		}
	}
	if s.GroupBy != nil {
		if s.GroupBy.Rollup {
			return nil, c.unsupported("WITH ROLLUP")
		}
		for _, it := range s.GroupBy.Items {
			e, err := c.expr(it.Expr)
			if err != nil {
				return nil, err
			}
			// A GROUP BY item that names a select alias groups by what the
			// alias names, which grouping, before the select list, would
			// otherwise look up as a column.
			if ref, ok := e.(*sqlir.ColumnRef); ok && ref.Table == "" {
				for _, t := range out.Targets {
					if t.Alias != "" && t.Expr != nil && strings.EqualFold(t.Alias, ref.Column) {
						e = t.Expr
						break
					}
				}
			}
			out.GroupBy = append(out.GroupBy, e)
		}
	}
	if s.Having != nil {
		if out.Having, err = c.cond(s.Having.Expr); err != nil {
			return nil, err
		}
		// MySQL's HAVING may stand without grouping, which the executor
		// would evaluate over one group, and may name a select alias, which
		// the executor evaluates it before; the one is refused and the
		// other replaced by the expression it names.
		if len(out.GroupBy) == 0 && !hasAggregate(out) {
			return nil, c.unsupported("HAVING without GROUP BY or an aggregate")
		}
		aliases := map[string]sqlir.Expr{}
		for _, t := range out.Targets {
			if t.Alias != "" && t.Expr != nil {
				aliases[strings.ToLower(t.Alias)] = t.Expr
			}
		}
		out.Having = replaceAliases(out.Having, aliases)
	}
	if out.OrderBy, err = c.orderBy(s.OrderBy); err != nil {
		return nil, err
	}
	// MySQL's column aliases are case-insensitive, while the executor looks
	// the output names up exactly.
	for _, k := range out.OrderBy {
		if ref, ok := k.Expr.(*sqlir.ColumnRef); ok && ref.Table == "" {
			for _, t := range out.Targets {
				if t.Alias != "" && strings.EqualFold(t.Alias, ref.Column) {
					ref.Column = t.Alias
					break
				}
			}
		}
	}
	if out.Limit, out.Offset, err = c.limit(s.Limit); err != nil {
		return nil, err
	}
	if s.LockInfo != nil {
		out.Lock = lockClause(s.LockInfo.LockType)
		if out.Lock == nil && s.LockInfo.LockType != ast.SelectLockNone {
			// Such as FOR UPDATE WAIT n: dropping it would turn the
			// locking read into a plain one.
			return nil, c.unsupported("locking clause " + s.LockInfo.LockType.String())
		}
		if out.Lock != nil {
			for _, t := range s.LockInfo.Tables {
				out.Lock.Of = append(out.Lock.Of, t.Name.L)
			}
		}
	}
	return out, nil
}

// lockClause converts FOR UPDATE, FOR SHARE and LOCK IN SHARE MODE. InnoDB
// has the two strengths only.
func lockClause(t ast.SelectLockType) *sqlir.LockClause {
	switch t {
	case ast.SelectLockForUpdate:
		return &sqlir.LockClause{Strength: "update"}
	case ast.SelectLockForUpdateNoWait:
		return &sqlir.LockClause{Strength: "update", NoWait: true}
	case ast.SelectLockForUpdateSkipLocked:
		return &sqlir.LockClause{Strength: "update", SkipLocked: true}
	case ast.SelectLockForShare:
		return &sqlir.LockClause{Strength: "share"}
	case ast.SelectLockForShareNoWait:
		return &sqlir.LockClause{Strength: "share", NoWait: true}
	case ast.SelectLockForShareSkipLocked:
		return &sqlir.LockClause{Strength: "share", SkipLocked: true}
	}
	return nil
}

// orderBy converts ORDER BY. MySQL sorts NULL first when ascending and last
// when descending, the opposite of Postgres's default, so the order is made
// explicit.
func (c *conv) orderBy(o *ast.OrderByClause) ([]sqlir.OrderKey, error) {
	if o == nil {
		return nil, nil
	}
	var out []sqlir.OrderKey
	for _, it := range o.Items {
		e, err := c.expr(it.Expr)
		if err != nil {
			return nil, err
		}
		k := sqlir.OrderKey{Expr: e, Desc: it.Desc, Nulls: sqlir.NullsFirst}
		if it.Desc {
			k.Nulls = sqlir.NullsLast
		}
		out = append(out, k)
	}
	return out, nil
}

func (c *conv) limit(l *ast.Limit) (count, offset sqlir.Expr, err error) {
	if l == nil {
		return nil, nil, nil
	}
	c.inLimit = true
	defer func() { c.inLimit = false }()
	if l.Count != nil {
		if count, err = c.expr(l.Count); err != nil {
			return nil, nil, err
		}
	}
	if l.Offset != nil {
		if offset, err = c.expr(l.Offset); err != nil {
			return nil, nil, err
		}
	}
	return count, offset, nil
}

// from converts a FROM clause: the leftmost table, and the tables joined to
// it in order.
func (c *conv) from(j *ast.Join) (*sqlir.TableRef, []sqlir.Join, error) {
	if j == nil {
		return nil, nil, nil
	}
	if j.NaturalJoin || len(j.Using) > 0 {
		return nil, nil, c.unsupported("NATURAL JOIN or JOIN USING")
	}
	var base *sqlir.TableRef
	var joins []sqlir.Join
	switch l := j.Left.(type) {
	case *ast.Join:
		b, js, err := c.from(l)
		if err != nil {
			return nil, nil, err
		}
		base, joins = b, js
	default:
		t, err := c.tableRef(j.Left)
		if err != nil {
			return nil, nil, err
		}
		base = &t
	}
	if j.Right == nil {
		return base, joins, nil
	}
	if _, nested := j.Right.(*ast.Join); nested {
		return nil, nil, c.unsupported("nested join on the right")
	}
	t, err := c.tableRef(j.Right)
	if err != nil {
		return nil, nil, err
	}
	out := sqlir.Join{Table: t}
	switch j.Tp {
	case ast.LeftJoin:
		out.Kind = sqlir.LeftJoin
	case ast.RightJoin:
		out.Kind = sqlir.RightJoin
	default:
		out.Kind = sqlir.CrossJoin
		if j.On != nil {
			out.Kind = sqlir.InnerJoin
		}
	}
	if j.On != nil {
		if out.On, err = c.cond(j.On.Expr); err != nil {
			return nil, nil, err
		}
	}
	return base, append(joins, out), nil
}

func (c *conv) tableRef(n ast.ResultSetNode) (sqlir.TableRef, error) {
	ts, ok := n.(*ast.TableSource)
	if !ok {
		return sqlir.TableRef{}, c.unsupported(fmt.Sprintf("from item %T", n))
	}
	// Column references name their table in lower case (ColumnName.Table.L),
	// so the alias a FROM item is known by is lower case too.
	out := sqlir.TableRef{Alias: ts.AsName.L, Lateral: ts.Lateral}
	for _, cn := range ts.ColumnNames {
		out.Columns = append(out.Columns, cn.L)
	}
	switch src := ts.Source.(type) {
	case *ast.TableName:
		if len(src.IndexHints) > 0 {
			// detest picks the index a statement searches by itself, and
			// would lock another range than the hinted index does.
			return out, c.unsupported("index hints")
		}
		out.Name = tableName(src)
		if src.Schema.L == "" && c.cteNames[src.Name.L] {
			out.Name = src.Name.L
		}
		if out.Alias == "" && src.Name.O != src.Name.L {
			out.Alias = src.Name.L
		}
	case *ast.SelectStmt, *ast.SetOprStmt:
		sel, err := c.query1(src)
		if err != nil {
			return out, err
		}
		out.Sub = sel
	default:
		return out, c.unsupported(fmt.Sprintf("from item %T", src))
	}
	return out, nil
}

// target returns the single table an INSERT, UPDATE or DELETE writes, with
// the tables a multi-table UPDATE joins to it.
func (c *conv) target(refs *ast.TableRefsClause) (sqlir.TableRef, []sqlir.TableRef, sqlir.Expr, error) {
	if refs == nil || refs.TableRefs == nil {
		return sqlir.TableRef{}, nil, nil, c.unsupported("statement without a table")
	}
	base, joins, err := c.from(refs.TableRefs)
	if err != nil {
		return sqlir.TableRef{}, nil, nil, err
	}
	if base.Sub != nil {
		return sqlir.TableRef{}, nil, nil, c.unsupported("writing to a derived table")
	}
	var extra []sqlir.TableRef
	var on sqlir.Expr
	for _, j := range joins {
		if j.Kind != sqlir.InnerJoin && j.Kind != sqlir.CrossJoin {
			return sqlir.TableRef{}, nil, nil, c.unsupported("outer join in UPDATE or DELETE")
		}
		extra = append(extra, j.Table)
		if j.On != nil {
			on = and(on, j.On)
		}
	}
	return *base, extra, on, nil
}

func and(a, b sqlir.Expr) sqlir.Expr {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return &sqlir.BinaryExpr{Op: "AND", L: a, R: b}
}

func (c *conv) insert(s *ast.InsertStmt) (sqlir.Statement, error) {
	if s.IsReplace {
		return nil, c.unsupported("REPLACE")
	}
	t, extra, _, err := c.target(s.Table)
	if err != nil {
		return nil, err
	}
	if len(extra) > 0 {
		return nil, c.unsupported("INSERT into a join")
	}
	out := &sqlir.InsertStmt{Table: t.Name}
	for _, col := range s.Columns {
		out.Columns = append(out.Columns, col.Name.L)
	}
	if len(s.ColumnAliases) > 0 {
		return nil, c.unsupported("INSERT ... AS alias with column aliases")
	}
	for _, row := range s.Lists {
		var items []sqlir.Expr
		for _, v := range row {
			e, err := c.expr(v)
			if err != nil {
				return nil, err
			}
			items = append(items, e)
		}
		out.Rows = append(out.Rows, items)
	}
	if s.Select != nil {
		if out.Select, err = c.query1(s.Select); err != nil {
			return nil, err
		}
	}
	switch {
	case len(s.OnDuplicate) > 0:
		// The row alias names the proposed row only in these expressions; a
		// later statement of a script may use the name for a table.
		c.rowAlias = s.RowAlias.L
		defer func() { c.rowAlias = "" }()
		oc := &sqlir.OnConflict{}
		for _, a := range s.OnDuplicate {
			if _, ok := a.Expr.(*ast.DefaultExpr); ok {
				return nil, c.unsupported("DEFAULT in ON DUPLICATE KEY UPDATE")
			}
			e, err := c.expr(a.Expr)
			if err != nil {
				return nil, err
			}
			oc.Set = append(oc.Set, sqlir.Assignment{Column: a.Column.Name.L, Value: e})
		}
		out.OnConflict = oc
	case s.IgnoreErr:
		// INSERT IGNORE skips a row that collides with any unique index.
		out.OnConflict = &sqlir.OnConflict{DoNothing: true}
	}
	if len(s.Returning) > 0 {
		return nil, c.unsupported("RETURNING")
	}
	return out, nil
}

func (c *conv) update(s *ast.UpdateStmt) (sqlir.Statement, error) {
	if s.With != nil {
		return nil, c.unsupported("CTE on UPDATE")
	}
	if s.IgnoreErr {
		return nil, c.unsupported("UPDATE IGNORE")
	}
	t, extra, on, err := c.target(s.TableRefs)
	if err != nil {
		return nil, err
	}
	out := &sqlir.UpdateStmt{Table: t.Name, Alias: t.Alias, From: extra}
	for _, a := range s.List {
		if q := a.Column.Table.L; q != "" && q != strings.ToLower(relname(t.Name)) && q != strings.ToLower(t.Name) && q != strings.ToLower(t.Alias) {
			return nil, c.unsupported("UPDATE of more than one table")
		}
		if _, ok := a.Expr.(*ast.DefaultExpr); ok {
			// The executor applies a column's default on insert only.
			return nil, c.unsupported("SET column = DEFAULT in UPDATE")
		}
		e, err := c.expr(a.Expr)
		if err != nil {
			return nil, err
		}
		out.Set = append(out.Set, sqlir.Assignment{Column: a.Column.Name.L, Value: e})
	}
	if s.Where != nil {
		if out.Where, err = c.cond(s.Where); err != nil {
			return nil, err
		}
	}
	out.Where = and(on, out.Where)
	if len(extra) > 0 && (s.Order != nil || s.Limit != nil) {
		return nil, c.unsupported("ORDER BY or LIMIT in a multi-table UPDATE, which MySQL refuses")
	}
	if out.OrderBy, err = c.orderBy(s.Order); err != nil {
		return nil, err
	}
	if out.Limit, _, err = c.limit(s.Limit); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *conv) delete(s *ast.DeleteStmt) (sqlir.Statement, error) {
	if s.IsMultiTable {
		return nil, c.unsupported("DELETE from more than one table")
	}
	if s.With != nil {
		return nil, c.unsupported("CTE on DELETE")
	}
	t, extra, on, err := c.target(s.TableRefs)
	if err != nil {
		return nil, err
	}
	out := &sqlir.DeleteStmt{Table: t.Name, Alias: t.Alias, Using: extra}
	if s.Where != nil {
		if out.Where, err = c.cond(s.Where); err != nil {
			return nil, err
		}
	}
	out.Where = and(on, out.Where)
	if out.OrderBy, err = c.orderBy(s.Order); err != nil {
		return nil, err
	}
	if out.Limit, _, err = c.limit(s.Limit); err != nil {
		return nil, err
	}
	return out, nil
}
