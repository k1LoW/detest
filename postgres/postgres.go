// Package postgres is PostgreSQL for detest: pass postgres.New() to
// detest.Sim.DB. It parses SQL with the real PostgreSQL grammar (pg_query
// compiled to wasm, pure Go) and converts the AST into detest's IR.
package postgres

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"

	"github.com/k1LoW/detest/internal/sqlir"
)

// Option configures the server New describes.
type Option func(*config)

type config struct {
	isolation  sqlir.IsolationLevel
	searchPath []string
	convert    func(*sqlir.DBError) error
}

// Isolation sets the level transactions run at when they do not ask for one,
// as default_transaction_isolation does (default Read Committed).
func Isolation(level sqlir.IsolationLevel) Option {
	return func(c *config) { c.isolation = level }
}

// SearchPath sets the schemas an unqualified table name is looked up in, in
// order, as search_path does (default "public"). An unqualified name that
// exists in none of them refers to the first.
func SearchPath(schemas ...string) Option {
	return func(c *config) { c.searchPath = schemas }
}

// Errors sets how the database errors detest raises reach the code under
// test. Production code branches on its driver's error type, such as pgx's
// *pgconn.PgError; convert builds that type from the SQLSTATE and the details
// detest reports (pgxerr.Convert does it for pgx). Without Errors the code
// under test sees *detest.DBError.
func Errors(convert func(*sqlir.DBError) error) Option {
	return func(c *config) { c.convert = convert }
}

// New describes a PostgreSQL server. detest implements Read Committed.
func New(opts ...Option) sqlir.Server {
	c := &config{isolation: sqlir.ReadCommitted, searchPath: []string{"public"}}
	for _, o := range opts {
		o(c)
	}
	return sqlir.NewServer(sqlir.ServerSpec{
		Name:       "postgres",
		Parser:     parser{},
		Isolation:  c.isolation,
		Supported:  []sqlir.IsolationLevel{sqlir.ReadCommitted},
		SearchPath: c.searchPath,
		Codes:      codes,
		Convert:    c.convert,
	})
}

func codes(k sqlir.DBErrorKind) (string, int) {
	switch k {
	case sqlir.UniqueViolation:
		return "23505", 0
	case sqlir.NotNullViolation:
		return "23502", 0
	case sqlir.Deadlock:
		return "40P01", 0
	case sqlir.InFailedTransaction:
		return "25P02", 0
	case sqlir.LockNotAvailable:
		return "55P03", 0
	case sqlir.UndefinedTable:
		return "42P01", 0
	case sqlir.ForeignKeyViolation:
		return "23503", 0
	case sqlir.DivisionByZero:
		return "22012", 0
	case sqlir.NumericValueOutOfRange:
		return "22003", 0
	case sqlir.InvalidTextRepresentation:
		return "22P02", 0
	case sqlir.SyntaxError:
		return "42601", 0
	case sqlir.UndefinedParameter:
		return "42P02", 0
	case sqlir.InvalidColumnReference:
		return "42P10", 0
	case sqlir.DuplicateTable:
		return "42P07", 0
	case sqlir.WrongObjectType:
		return "42809", 0
	case sqlir.InvalidTableDefinition:
		return "42P16", 0
	case sqlir.NoActiveTransaction:
		return "25P01", 0
	case sqlir.InvalidSavepoint:
		return "3B001", 0
	case sqlir.CheckViolation:
		return "23514", 0
	}
	return "", 0
}

// parser parses with the real PostgreSQL grammar and converts the AST into
// detest's IR.
type parser struct{}

func (parser) Name() string { return "postgres" }

func (parser) Parse(query string) (sqlir.Statement, error) {
	res, err := pgquery.Parse(query)
	if err != nil {
		return nil, &sqlir.ParseError{Query: query, Err: err}
	}
	c := &pgConv{query: query}
	if len(res.Stmts) != 1 {
		return c.schemaScript(res.Stmts)
	}
	stmt, err := c.stmt(res.Stmts[0].Stmt)
	if err != nil {
		return nil, err
	}
	return stmt, nil
}

type pgConv struct {
	query   string
	windows map[string]*pg.WindowDef // the WINDOW clause of the query being converted
}

func (c *pgConv) unsupported(what string) error { return sqlir.Unsupported(what, c.query) }

func (c *pgConv) stmt(n *pg.Node) (sqlir.Statement, error) {
	switch s := n.Node.(type) {
	case *pg.Node_SelectStmt:
		if isSetConfig(s.SelectStmt) {
			// A schema dump sets search_path with SELECT pg_catalog.set_config(...).
			return &sqlir.SchemaStmt{}, nil
		}
		return c.selectStmt(s.SelectStmt)
	case *pg.Node_CreateTableAsStmt:
		return c.createTableAs(s.CreateTableAsStmt)
	case *pg.Node_VariableSetStmt:
		v := s.VariableSetStmt
		out := &sqlir.SetStmt{Name: v.Name, Local: v.IsLocal}
		if len(v.Args) > 0 {
			if e, err := c.expr(v.Args[0]); err == nil {
				if k, ok := e.(*sqlir.Const); ok {
					out.Value = fmt.Sprint(k.Value)
				}
			}
		}
		return out, nil
	case *pg.Node_ConstraintsSetStmt:
		out := &sqlir.SetConstraintsStmt{Deferred: s.ConstraintsSetStmt.Deferred}
		for _, n := range s.ConstraintsSetStmt.Constraints {
			out.Names = append(out.Names, n.GetRangeVar().GetRelname())
		}
		return out, nil
	case *pg.Node_RefreshMatViewStmt:
		return &sqlir.RefreshStmt{Table: rangeVarName(s.RefreshMatViewStmt.Relation), NoData: s.RefreshMatViewStmt.SkipData}, nil
	case *pg.Node_InsertStmt:
		return c.insertStmt(s.InsertStmt)
	case *pg.Node_UpdateStmt:
		return c.updateStmt(s.UpdateStmt)
	case *pg.Node_DeleteStmt:
		return c.deleteStmt(s.DeleteStmt)
	case *pg.Node_TransactionStmt:
		ts := s.TransactionStmt
		switch ts.Kind {
		case pg.TransactionStmtKind_TRANS_STMT_SAVEPOINT:
			return &sqlir.SavepointStmt{Op: "savepoint", Name: ts.SavepointName}, nil
		case pg.TransactionStmtKind_TRANS_STMT_RELEASE:
			return &sqlir.SavepointStmt{Op: "release", Name: ts.SavepointName}, nil
		case pg.TransactionStmtKind_TRANS_STMT_ROLLBACK_TO:
			return &sqlir.SavepointStmt{Op: "rollback_to", Name: ts.SavepointName}, nil
		}
		return nil, c.unsupported("BEGIN, COMMIT or ROLLBACK as a statement (use database/sql's transactions)")
	}
	if changes, ok, err := c.schema(n); ok {
		if err != nil {
			return nil, err
		}
		return &sqlir.SchemaStmt{Changes: changes}, nil
	}
	return nil, c.unsupported(fmt.Sprintf("%T", n.Node))
}

// schemaScript converts a script of several statements, such as a schema dump
// or a migration that backfills data between its DDL. BEGIN and COMMIT in it
// do nothing: the script runs as one implicit transaction anyway.
func (c *pgConv) schemaScript(stmts []*pg.RawStmt) (sqlir.Statement, error) {
	out := &sqlir.Script{}
	for _, raw := range stmts {
		if ts := raw.Stmt.GetTransactionStmt(); ts != nil {
			switch ts.Kind {
			case pg.TransactionStmtKind_TRANS_STMT_BEGIN, pg.TransactionStmtKind_TRANS_STMT_START, pg.TransactionStmtKind_TRANS_STMT_COMMIT:
				continue
			}
		}
		st, err := c.stmt(raw.Stmt)
		if err != nil {
			return nil, err
		}
		out.Stmts = append(out.Stmts, st)
	}
	return out, nil
}

func (c *pgConv) createTableAs(s *pg.CreateTableAsStmt) (sqlir.Statement, error) {
	if s.IsSelectInto {
		return nil, c.unsupported("SELECT INTO")
	}
	sel, err := c.selectStmt(s.Query.GetSelectStmt())
	if err != nil {
		return nil, err
	}
	out := &sqlir.CreateTableAsStmt{Table: rangeVarName(s.Into.Rel), Select: sel, NoData: s.Into.SkipData, IfNotExists: s.IfNotExists,
		Materialized: s.Objtype == pg.ObjectType_OBJECT_MATVIEW}
	for _, n := range s.Into.ColNames {
		out.Columns = append(out.Columns, n.GetString_().GetSval())
	}
	return out, nil
}

// schema converts a schema statement. ok is false for a statement that is not
// one. Schema statements detest does not need convert to no changes, so a
// schema dump runs as a whole.
func (c *pgConv) schema(n *pg.Node) (changes []sqlir.SchemaChange, ok bool, err error) {
	switch s := n.Node.(type) {
	case *pg.Node_CreateStmt:
		ch, err := c.createTable(s.CreateStmt)
		return []sqlir.SchemaChange{ch}, true, err
	case *pg.Node_AlterTableStmt:
		ch, err := c.alterTable(s.AlterTableStmt)
		if err != nil || ch == nil {
			return nil, true, err
		}
		return []sqlir.SchemaChange{*ch}, true, nil
	case *pg.Node_IndexStmt:
		ix := s.IndexStmt
		if !ix.Unique && !ix.Primary {
			return nil, true, nil
		}
		u, err := c.indexDef(ix)
		if err != nil {
			return nil, true, err
		}
		return []sqlir.SchemaChange{{Table: rangeVarName(ix.Relation), Constraints: []sqlir.UniqueDef{u}}}, true, nil
	case *pg.Node_DropStmt:
		object, ok := map[pg.ObjectType]string{pg.ObjectType_OBJECT_TABLE: "", pg.ObjectType_OBJECT_VIEW: "view",
			pg.ObjectType_OBJECT_MATVIEW: "matview", pg.ObjectType_OBJECT_INDEX: "index"}[s.DropStmt.RemoveType]
		if !ok {
			return nil, true, nil
		}
		for _, obj := range s.DropStmt.Objects {
			var parts []string
			for _, p := range obj.GetList().GetItems() {
				parts = append(parts, p.GetString_().GetSval())
			}
			changes = append(changes, sqlir.SchemaChange{Table: strings.Join(parts, "."), Drop: true, IfExists: s.DropStmt.MissingOk, Object: object})
		}
		return changes, true, nil
	case *pg.Node_ViewStmt:
		v := s.ViewStmt
		sel, err := c.selectStmt(v.Query.GetSelectStmt())
		if err != nil {
			return nil, true, err
		}
		ch := sqlir.SchemaChange{Table: rangeVarName(v.View), Object: "view", View: sel, Replace: v.Replace}
		for _, a := range v.Aliases {
			ch.ViewColumns = append(ch.ViewColumns, a.GetString_().GetSval())
		}
		return []sqlir.SchemaChange{ch}, true, nil
	case *pg.Node_RenameStmt:
		r := s.RenameStmt
		switch r.RenameType {
		case pg.ObjectType_OBJECT_TABLE, pg.ObjectType_OBJECT_VIEW, pg.ObjectType_OBJECT_MATVIEW:
			return []sqlir.SchemaChange{{Table: rangeVarName(r.Relation), RenameTo: r.Newname, IfExists: r.MissingOk}}, true, nil
		case pg.ObjectType_OBJECT_COLUMN:
			return []sqlir.SchemaChange{{Table: rangeVarName(r.Relation), RenameColumn: [2]string{r.Subname, r.Newname}, IfExists: r.MissingOk}}, true, nil
		case pg.ObjectType_OBJECT_TABCONSTRAINT:
			return []sqlir.SchemaChange{{Table: rangeVarName(r.Relation), RenameConstraint: [2]string{r.Subname, r.Newname}, IfExists: r.MissingOk}}, true, nil
		case pg.ObjectType_OBJECT_INDEX:
			return []sqlir.SchemaChange{{Table: rangeVarName(r.Relation), Object: "index", RenameConstraint: [2]string{r.Relation.GetRelname(), r.Newname}, IfExists: r.MissingOk}}, true, nil
		}
		return nil, true, nil
	case *pg.Node_DoStmt:
		// detest cannot run PL/pgSQL. In migrations a DO block usually checks the
		// catalog and raises, declaring nothing, so it runs as nothing.
		return nil, true, nil
	case *pg.Node_CommentStmt, *pg.Node_CreateFunctionStmt,
		*pg.Node_CreateSeqStmt, *pg.Node_AlterSeqStmt, *pg.Node_CreateExtensionStmt,
		*pg.Node_CreateSchemaStmt, *pg.Node_GrantStmt, *pg.Node_GrantRoleStmt,
		*pg.Node_AlterOwnerStmt, *pg.Node_CreateTrigStmt,
		*pg.Node_AlterDefaultPrivilegesStmt, *pg.Node_CreateEnumStmt,
		*pg.Node_CompositeTypeStmt, *pg.Node_CreateDomainStmt, *pg.Node_DefineStmt,
		*pg.Node_AlterEnumStmt, *pg.Node_CreatePolicyStmt, *pg.Node_RuleStmt,
		*pg.Node_CreateStatsStmt, *pg.Node_AlterObjectSchemaStmt:
		return nil, true, nil
	}
	return nil, false, nil
}

func isSetConfig(sel *pg.SelectStmt) bool {
	if sel == nil || sel.FromClause != nil || len(sel.TargetList) != 1 {
		return false
	}
	fc := sel.TargetList[0].GetResTarget().GetVal().GetFuncCall()
	if fc == nil || len(fc.Funcname) == 0 {
		return false
	}
	return fc.Funcname[len(fc.Funcname)-1].GetString_().GetSval() == "set_config"
}

// rangeVarName is the table name as written, schema-qualified when it is.
func rangeVarName(rv *pg.RangeVar) string {
	if rv.GetSchemaname() != "" {
		return rv.GetSchemaname() + "." + rv.GetRelname()
	}
	return rv.GetRelname()
}

func (c *pgConv) createTable(s *pg.CreateStmt) (sqlir.SchemaChange, error) {
	ch := sqlir.SchemaChange{Table: rangeVarName(s.Relation), Create: true, IfNotExists: s.IfNotExists}
	for _, elt := range s.TableElts {
		switch e := elt.Node.(type) {
		case *pg.Node_ColumnDef:
			col, cons, checks, err := c.columnDef(ch.Table, e.ColumnDef)
			if err != nil {
				return ch, err
			}
			ch.Columns = append(ch.Columns, col)
			ch.Constraints = append(ch.Constraints, cons...)
			ch.Checks = append(ch.Checks, checks...)
			ch.ForeignKeys = append(ch.ForeignKeys, columnForeignKeys(e.ColumnDef)...)
		case *pg.Node_Constraint:
			if fk, ok := foreignKey(e.Constraint, nil); ok {
				ch.ForeignKeys = append(ch.ForeignKeys, fk)
				continue
			}
			if chk, ok := c.check(e.Constraint); ok {
				ch.Checks = append(ch.Checks, chk)
				continue
			}
			u, ok, err := c.constraintDef(e.Constraint, nil)
			if err != nil {
				return ch, err
			}
			if ok {
				ch.Constraints = append(ch.Constraints, u)
			}
		}
	}
	return ch, nil
}

// check converts a CHECK constraint. One detest cannot evaluate is dropped,
// as a default it cannot evaluate becomes unknown: the schema still loads.
func (c *pgConv) check(k *pg.Constraint) (sqlir.CheckDef, bool) {
	if k == nil || k.Contype != pg.ConstrType_CONSTR_CHECK {
		return sqlir.CheckDef{}, false
	}
	e, err := c.expr(k.RawExpr)
	if err != nil {
		return sqlir.CheckDef{}, false
	}
	return sqlir.CheckDef{Name: k.Conname, Expr: e}, true
}

// columnDef converts a column with the constraints written on it.
func (c *pgConv) columnDef(table string, d *pg.ColumnDef) (sqlir.ColumnDef, []sqlir.UniqueDef, []sqlir.CheckDef, error) {
	col := sqlir.ColumnDef{Name: d.Colname, Type: typeName(d.TypeName)}
	if d.RawDefault != nil {
		col.Default = c.defaultExpr(d.RawDefault)
	}
	if d.Identity != "" || isSerial(d.TypeName) {
		col.Default = sequenceDefault(table, d.Colname)
	}
	var cons []sqlir.UniqueDef
	var checks []sqlir.CheckDef
	col.NotNull = d.IsNotNull
	for _, n := range d.Constraints {
		k := n.GetConstraint()
		if k == nil {
			continue
		}
		switch k.Contype {
		case pg.ConstrType_CONSTR_NOTNULL:
			col.NotNull = true
		case pg.ConstrType_CONSTR_CHECK:
			if chk, ok := c.check(k); ok {
				if chk.Name == "" {
					chk.Name = relnameOf(table) + "_" + d.Colname + "_check"
				}
				checks = append(checks, chk)
			}
		case pg.ConstrType_CONSTR_DEFAULT:
			col.Default = c.defaultExpr(k.RawExpr)
		case pg.ConstrType_CONSTR_IDENTITY:
			col.Default = sequenceDefault(table, d.Colname)
		case pg.ConstrType_CONSTR_GENERATED:
			col.Generated = c.defaultExpr(k.RawExpr)
			if err := c.immutable(col.Generated); err != nil {
				return col, nil, nil, err
			}
		case pg.ConstrType_CONSTR_PRIMARY, pg.ConstrType_CONSTR_UNIQUE:
			u, _, err := c.constraintDef(k, []string{d.Colname})
			if err != nil {
				return col, nil, nil, err
			}
			cons = append(cons, u)
		}
	}
	if col.Generated != nil && col.Default != nil {
		return col, nil, nil, c.unsupported("a default or identity on a generated column")
	}
	return col, cons, checks, nil
}

// mutableFuncs are the functions detest evaluates that Postgres does not
// mark immutable, which a generation expression may not call. A function
// detest does not know is let through: the schema loads, and a write that
// needs it fails then.
var mutableFuncs = map[string]bool{
	"nextval": true, "setval": true, "currval": true, "gen_random_uuid": true, "uuid_generate_v4": true,
	"now": true, "clock_timestamp": true, "current_timestamp": true, "transaction_timestamp": true,
	"statement_timestamp": true, "random": true, "concat": true,
	"pg_try_advisory_xact_lock": true, "pg_advisory_xact_lock": true,
	"count": true, "sum": true, "min": true, "max": true, "avg": true,
}

// immutable refuses what Postgres does not allow in a generation expression:
// a function that is not immutable, such as nextval or now, an aggregate, a
// subquery or a parameter. detest would otherwise run it on every write.
func (c *pgConv) immutable(e sqlir.Expr) error {
	for _, x := range sqlir.Exprs(e) {
		switch v := x.(type) {
		case *sqlir.FuncCall:
			if mutableFuncs[v.Name] {
				return c.unsupported("generated column calling " + v.Name)
			}
		case *sqlir.SubQuery, *sqlir.Exists, *sqlir.Param, *sqlir.WindowFunc:
			return c.unsupported("generated column with a subquery, parameter or window function")
		case *sqlir.InExpr:
			if v.Sub != nil {
				return c.unsupported("generated column with a subquery")
			}
		}
	}
	return nil
}

// relnameOf is a table name without its schema, as constraint names use it.
func relnameOf(table string) string {
	if i := strings.LastIndexByte(table, '.'); i >= 0 {
		return table[i+1:]
	}
	return table
}

// defaultExpr converts a column default. A default detest cannot convert
// becomes the unknown value instead of failing the whole schema, which is
// often a dump with defaults no invariant looks at.
func (c *pgConv) defaultExpr(n *pg.Node) sqlir.Expr {
	e, err := c.expr(n)
	if err != nil {
		return &sqlir.Const{Value: sqlir.Unknown}
	}
	return e
}

// foreignKey converts a FOREIGN KEY constraint. cols are the column of a
// REFERENCES written on a column, which lists no columns itself.
func foreignKey(k *pg.Constraint, cols []string) (sqlir.ForeignKey, bool) {
	if k == nil || k.Contype != pg.ConstrType_CONSTR_FOREIGN {
		return sqlir.ForeignKey{}, false
	}
	action := func(a string) string {
		return map[string]string{"r": "restrict", "c": "cascade", "n": "set null", "d": "set default"}[a]
	}
	// INITIALLY DEFERRED implies DEFERRABLE.
	fk := sqlir.ForeignKey{Name: k.Conname, RefTable: rangeVarName(k.Pktable), OnDelete: action(k.FkDelAction), OnUpdate: action(k.FkUpdAction),
		Deferrable: k.Deferrable || k.Initdeferred, Deferred: k.Initdeferred, MatchFull: k.FkMatchtype == "f"}
	if fk.OnDelete == "" {
		fk.OnDelete = "no action"
	}
	if fk.OnUpdate == "" {
		fk.OnUpdate = "no action"
	}
	for _, n := range k.FkAttrs {
		cols = append(cols, n.GetString_().GetSval())
	}
	fk.Columns = cols
	for _, n := range k.PkAttrs {
		fk.RefColumns = append(fk.RefColumns, n.GetString_().GetSval())
	}
	return fk, true
}

// columnForeignKeys converts the REFERENCES of a column. The grammar gives
// the DEFERRABLE and INITIALLY clauses written after one as constraints of
// their own that follow it, which apply to it.
func columnForeignKeys(d *pg.ColumnDef) []sqlir.ForeignKey {
	var out []sqlir.ForeignKey
	for _, n := range d.GetConstraints() {
		k := n.GetConstraint()
		if fk, ok := foreignKey(k, []string{d.Colname}); ok {
			out = append(out, fk)
			continue
		}
		if len(out) == 0 || k == nil {
			continue
		}
		last := &out[len(out)-1]
		switch k.Contype {
		case pg.ConstrType_CONSTR_ATTR_DEFERRABLE:
			last.Deferrable = true
		case pg.ConstrType_CONSTR_ATTR_NOT_DEFERRABLE:
			last.Deferrable = false
		case pg.ConstrType_CONSTR_ATTR_DEFERRED:
			last.Deferrable, last.Deferred = true, true
		case pg.ConstrType_CONSTR_ATTR_IMMEDIATE:
			last.Deferred = false
		}
	}
	return out
}

// typeName is a column type's name as Postgres catalogs it: int4 for integer
// and serial, int8 for bigint and bigserial, and so on.
func typeName(t *pg.TypeName) string {
	if t == nil || len(t.Names) == 0 {
		return ""
	}
	if len(t.ArrayBounds) > 0 {
		return "array"
	}
	n := t.Names[len(t.Names)-1].GetString_().GetSval()
	switch n {
	case "serial", "serial4":
		return "int4"
	case "bigserial", "serial8":
		return "int8"
	case "smallserial", "serial2":
		return "int2"
	}
	return n
}

func isSerial(t *pg.TypeName) bool {
	if t == nil || len(t.Names) == 0 {
		return false
	}
	switch t.Names[len(t.Names)-1].GetString_().GetSval() {
	case "serial", "bigserial", "smallserial", "serial2", "serial4", "serial8":
		return true
	}
	return false
}

// sequenceDefault is the default of a serial or identity column. Postgres
// names the sequence table_column_seq.
func sequenceDefault(table, column string) sqlir.Expr {
	rel := table
	if i := strings.LastIndex(rel, "."); i >= 0 {
		rel = rel[i+1:]
	}
	return &sqlir.FuncCall{Name: "nextval", Args: []sqlir.Expr{&sqlir.Const{Value: rel + "_" + column + "_seq"}}}
}

// constraintDef converts a PRIMARY KEY or UNIQUE constraint. cols are the
// columns of a constraint written on a column, which lists no keys itself.
func (c *pgConv) constraintDef(k *pg.Constraint, cols []string) (sqlir.UniqueDef, bool, error) {
	if k.Contype != pg.ConstrType_CONSTR_PRIMARY && k.Contype != pg.ConstrType_CONSTR_UNIQUE {
		return sqlir.UniqueDef{}, false, nil
	}
	u := sqlir.UniqueDef{Name: k.Conname, Primary: k.Contype == pg.ConstrType_CONSTR_PRIMARY, NullsNotDistinct: k.NullsNotDistinct}
	if k.Indexname != "" {
		return u, false, c.unsupported("constraint USING INDEX")
	}
	for _, key := range k.Keys {
		cols = append(cols, key.GetString_().GetSval())
	}
	for _, col := range cols {
		u.Elems = append(u.Elems, &sqlir.ColumnRef{Column: col})
	}
	return u, true, nil
}

func (c *pgConv) indexDef(ix *pg.IndexStmt) (sqlir.UniqueDef, error) {
	u := sqlir.UniqueDef{Name: ix.Idxname, Primary: ix.Primary, NullsNotDistinct: ix.NullsNotDistinct}
	for _, n := range ix.IndexParams {
		el := n.GetIndexElem()
		if el.Name != "" {
			u.Elems = append(u.Elems, &sqlir.ColumnRef{Column: el.Name})
			continue
		}
		e, err := c.expr(el.Expr)
		if err != nil {
			return u, err
		}
		u.Elems = append(u.Elems, e)
	}
	if ix.WhereClause != nil {
		e, err := c.expr(ix.WhereClause)
		if err != nil {
			return u, err
		}
		u.Where = e
	}
	return u, nil
}

// alterTable converts the ALTER TABLE subcommands that change keys or
// defaults. Others (owner, storage, triggers) declare nothing detest needs.
func (c *pgConv) alterTable(s *pg.AlterTableStmt) (*sqlir.SchemaChange, error) {
	if s.Objtype != pg.ObjectType_OBJECT_TABLE {
		return nil, nil
	}
	ch := &sqlir.SchemaChange{Table: rangeVarName(s.Relation)}
	for _, n := range s.Cmds {
		cmd := n.GetAlterTableCmd()
		switch cmd.Subtype {
		case pg.AlterTableType_AT_AddConstraint:
			if fk, ok := foreignKey(cmd.Def.GetConstraint(), nil); ok {
				ch.ForeignKeys = append(ch.ForeignKeys, fk)
				continue
			}
			if chk, ok := c.check(cmd.Def.GetConstraint()); ok {
				ch.Checks = append(ch.Checks, chk)
				continue
			}
			u, ok, err := c.constraintDef(cmd.Def.GetConstraint(), nil)
			if err != nil {
				return nil, err
			}
			if ok {
				ch.Constraints = append(ch.Constraints, u)
			}
		case pg.AlterTableType_AT_ColumnDefault:
			col := sqlir.ColumnDef{Name: cmd.Name}
			if cmd.Def != nil {
				col.Default = c.defaultExpr(cmd.Def)
			}
			ch.Columns = append(ch.Columns, col)
		case pg.AlterTableType_AT_AddIdentity:
			ch.Columns = append(ch.Columns, sqlir.ColumnDef{Name: cmd.Name, Default: sequenceDefault(ch.Table, cmd.Name)})
		case pg.AlterTableType_AT_AddColumn:
			col, cons, checks, err := c.columnDef(ch.Table, cmd.Def.GetColumnDef())
			if err != nil {
				return nil, err
			}
			ch.Columns = append(ch.Columns, col)
			ch.Constraints = append(ch.Constraints, cons...)
			ch.Checks = append(ch.Checks, checks...)
			ch.ForeignKeys = append(ch.ForeignKeys, columnForeignKeys(cmd.Def.GetColumnDef())...)
		case pg.AlterTableType_AT_AlterColumnType:
			if cd := cmd.Def.GetColumnDef(); cd != nil {
				ch.Columns = append(ch.Columns, sqlir.ColumnDef{Name: cmd.Name, Type: typeName(cd.TypeName), TypeOnly: true})
			}
		case pg.AlterTableType_AT_SetNotNull:
			ch.Columns = append(ch.Columns, sqlir.ColumnDef{Name: cmd.Name, NotNull: true, TypeOnly: true})
		case pg.AlterTableType_AT_DropNotNull:
			ch.Columns = append(ch.Columns, sqlir.ColumnDef{Name: cmd.Name, DropNotNull: true, TypeOnly: true})
		case pg.AlterTableType_AT_DropConstraint:
			ch.DropConstraints = append(ch.DropConstraints, cmd.Name)
		case pg.AlterTableType_AT_DropColumn:
			ch.DropColumns = append(ch.DropColumns, cmd.Name)
		case pg.AlterTableType_AT_SetExpression, pg.AlterTableType_AT_DropExpression:
			return nil, c.unsupported("changing the expression of a generated column")
		}
	}
	return ch, nil
}

func (c *pgConv) with(w *pg.WithClause) ([]sqlir.CTE, error) {
	if w == nil {
		return nil, nil
	}
	if w.Recursive {
		return nil, c.unsupported("recursive CTE")
	}
	var out []sqlir.CTE
	for _, n := range w.Ctes {
		cte := n.GetCommonTableExpr()
		sel, err := c.selectStmt(cte.Ctequery.GetSelectStmt())
		if err != nil {
			return nil, err
		}
		out = append(out, sqlir.CTE{Name: cte.Ctename, Select: sel})
	}
	return out, nil
}

func (c *pgConv) selectStmt(s *pg.SelectStmt) (*sqlir.SelectStmt, error) {
	if s == nil {
		return nil, c.unsupported("select")
	}
	out := &sqlir.SelectStmt{}
	var err error
	if out.With, err = c.with(s.WithClause); err != nil {
		return nil, err
	}
	if len(s.LockingClause) > 0 && (s.Op != pg.SetOperation_SETOP_NONE || len(s.ValuesLists) > 0) {
		return nil, c.unsupported("FOR UPDATE on a set operation or VALUES")
	}
	if s.Op == pg.SetOperation_SETOP_UNION || s.Op == pg.SetOperation_SETOP_INTERSECT || s.Op == pg.SetOperation_SETOP_EXCEPT {
		out.SetOp = map[pg.SetOperation]string{pg.SetOperation_SETOP_UNION: "union", pg.SetOperation_SETOP_INTERSECT: "intersect", pg.SetOperation_SETOP_EXCEPT: "except"}[s.Op]
		out.SetAll = s.All
		if out.Larg, err = c.selectStmt(s.Larg); err != nil {
			return nil, err
		}
		if out.Rarg, err = c.selectStmt(s.Rarg); err != nil {
			return nil, err
		}
		if out.Larg.Lock != nil || out.Rarg.Lock != nil {
			return nil, c.unsupported("FOR UPDATE in a set operation")
		}
		return out, c.tail(s, out)
	}
	if len(s.ValuesLists) > 0 {
		for _, l := range s.ValuesLists {
			row, err := c.exprs(l.GetList().GetItems())
			if err != nil {
				return nil, err
			}
			out.Values = append(out.Values, row)
		}
		return out, c.tail(s, out)
	}
	for _, d := range s.DistinctClause {
		if d == nil || d.Node == nil {
			out.Distinct = true // DISTINCT, which lists no expressions
			continue
		}
		e, err := c.expr(d)
		if err != nil {
			return nil, err
		}
		out.DistinctOn = append(out.DistinctOn, e)
	}
	saved := c.windows
	defer func() { c.windows = saved }()
	c.windows = map[string]*pg.WindowDef{}
	for _, w := range s.WindowClause {
		if wd := w.GetWindowDef(); wd != nil {
			c.windows[wd.Name] = wd
		}
	}
	for _, t := range s.TargetList {
		rt := t.GetResTarget()
		if cr := rt.Val.GetColumnRef(); cr != nil && len(cr.Fields) > 0 && cr.Fields[len(cr.Fields)-1].GetAStar() != nil {
			tg := sqlir.Target{Star: true}
			if len(cr.Fields) > 1 {
				tg.Table = cr.Fields[0].GetString_().GetSval()
			}
			out.Targets = append(out.Targets, tg)
			continue
		}
		e, err := c.expr(rt.Val)
		if err != nil {
			return nil, err
		}
		out.Targets = append(out.Targets, sqlir.Target{Expr: e, Alias: rt.Name})
	}
	switch len(s.FromClause) {
	case 0:
	case 1:
		from, joins, err := c.fromItem(s.FromClause[0])
		if err != nil {
			return nil, err
		}
		out.From, out.Joins = from, joins
	default:
		// FROM a, b is a cross join.
		from, joins, err := c.fromItem(s.FromClause[0])
		if err != nil {
			return nil, err
		}
		out.From, out.Joins = from, joins
		for _, f := range s.FromClause[1:] {
			t, js, err := c.fromItem(f)
			if err != nil {
				return nil, err
			}
			out.Joins = append(out.Joins, sqlir.Join{Kind: sqlir.CrossJoin, Table: *t})
			out.Joins = append(out.Joins, js...)
		}
	}
	if s.WhereClause != nil {
		if out.Where, err = c.expr(s.WhereClause); err != nil {
			return nil, err
		}
	}
	for _, g := range s.GroupClause {
		e, err := c.expr(g)
		if err != nil {
			return nil, err
		}
		out.GroupBy = append(out.GroupBy, e)
	}
	if s.HavingClause != nil {
		if out.Having, err = c.expr(s.HavingClause); err != nil {
			return nil, err
		}
	}
	if err := c.tail(s, out); err != nil {
		return nil, err
	}
	if len(s.LockingClause) > 1 {
		return nil, c.unsupported("more than one locking clause")
	}
	for _, lc := range s.LockingClause {
		if l := lc.GetLockingClause(); l != nil {
			out.Lock = &sqlir.LockClause{SkipLocked: l.WaitPolicy == pg.LockWaitPolicy_LockWaitSkip, NoWait: l.WaitPolicy == pg.LockWaitPolicy_LockWaitError,
				Strength: map[pg.LockClauseStrength]string{pg.LockClauseStrength_LCS_FORUPDATE: "update", pg.LockClauseStrength_LCS_FORNOKEYUPDATE: "no key update",
					pg.LockClauseStrength_LCS_FORSHARE: "share", pg.LockClauseStrength_LCS_FORKEYSHARE: "key share"}[l.Strength]}
			for _, r := range l.LockedRels {
				out.Lock.Of = append(out.Lock.Of, r.GetRangeVar().GetRelname())
			}
		}
	}
	return out, nil
}

// tail converts ORDER BY, LIMIT and OFFSET, which a set operation and a
// VALUES list take as well as a plain query.
func (c *pgConv) tail(s *pg.SelectStmt, out *sqlir.SelectStmt) error {
	keys, err := c.orderKeys(s.SortClause)
	if err != nil {
		return err
	}
	out.OrderBy = keys
	if s.LimitCount != nil {
		if out.Limit, err = c.expr(s.LimitCount); err != nil {
			return err
		}
	}
	if s.LimitOffset != nil {
		if out.Offset, err = c.expr(s.LimitOffset); err != nil {
			return err
		}
	}
	return nil
}

func (c *pgConv) orderKeys(list []*pg.Node) ([]sqlir.OrderKey, error) {
	var keys []sqlir.OrderKey
	for _, sc := range list {
		sb := sc.GetSortBy()
		e, err := c.expr(sb.Node)
		if err != nil {
			return nil, err
		}
		k := sqlir.OrderKey{Expr: e, Desc: sb.SortbyDir == pg.SortByDir_SORTBY_DESC}
		switch sb.SortbyNulls {
		case pg.SortByNulls_SORTBY_NULLS_FIRST:
			k.Nulls = sqlir.NullsFirst
		case pg.SortByNulls_SORTBY_NULLS_LAST:
			k.Nulls = sqlir.NullsLast
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// Frame options of a window, from PostgreSQL's parsenodes.h.
const (
	frameNondefault              = 0x00001
	frameStartUnboundedPreceding = 0x00020
	frameEndUnboundedFollowing   = 0x00100
)

// window converts the OVER clause of a function call, resolving a named
// window from the query's WINDOW clause.
func (c *pgConv) window(fc *sqlir.FuncCall, over *pg.WindowDef) (sqlir.Expr, error) {
	partition, order, frame := over.PartitionClause, over.OrderClause, over.FrameOptions
	if over.Refname != "" || (over.Name != "" && len(partition) == 0 && len(order) == 0 && frame&frameNondefault == 0) {
		name := over.Refname
		if name == "" {
			name = over.Name
		}
		named, ok := c.windows[name]
		if !ok {
			return nil, c.unsupported("window " + name + " is not defined")
		}
		partition = named.PartitionClause
		if len(order) == 0 {
			order = named.OrderClause
		}
		if frame&frameNondefault == 0 {
			frame = named.FrameOptions
		}
	}
	w := &sqlir.WindowFunc{Func: fc}
	var err error
	if w.Partition, err = c.exprs(partition); err != nil {
		return nil, err
	}
	if w.Order, err = c.orderKeys(order); err != nil {
		return nil, err
	}
	if frame&frameNondefault != 0 {
		const whole = frameStartUnboundedPreceding | frameEndUnboundedFollowing
		if frame&whole != whole {
			return nil, c.unsupported("window frame other than the default or the whole partition")
		}
		w.Whole = true
	}
	return w, nil
}

// fromItem converts a FROM item into a base table plus joins (left-deep).
func (c *pgConv) fromItem(n *pg.Node) (*sqlir.TableRef, []sqlir.Join, error) {
	switch f := n.Node.(type) {
	case *pg.Node_RangeVar:
		t := &sqlir.TableRef{Name: rangeVarName(f.RangeVar)}
		if f.RangeVar.Alias != nil {
			t.Alias = f.RangeVar.Alias.Aliasname
		}
		return t, nil, nil
	case *pg.Node_RangeFunction:
		rf := f.RangeFunction
		if rf.IsRowsfrom || len(rf.Functions) != 1 || len(rf.Coldeflist) > 0 {
			return nil, nil, c.unsupported("set-returning function in FROM with ROWS FROM or a column definition list")
		}
		items := rf.Functions[0].GetList().GetItems()
		if len(items) == 0 {
			return nil, nil, c.unsupported("function in FROM")
		}
		e, err := c.expr(items[0])
		if err != nil {
			return nil, nil, err
		}
		fc, ok := e.(*sqlir.FuncCall)
		if !ok {
			return nil, nil, c.unsupported("function in FROM")
		}
		t := &sqlir.TableRef{Name: fc.Name, Func: fc, Ordinality: rf.Ordinality, Lateral: true}
		if rf.Alias != nil {
			t.Alias = rf.Alias.Aliasname
			for _, cn := range rf.Alias.Colnames {
				t.Columns = append(t.Columns, cn.GetString_().GetSval())
			}
		}
		return t, nil, nil
	case *pg.Node_RangeSubselect:
		sel, err := c.selectStmt(f.RangeSubselect.Subquery.GetSelectStmt())
		if err != nil {
			return nil, nil, err
		}
		t := &sqlir.TableRef{Sub: sel, Lateral: f.RangeSubselect.Lateral}
		if a := f.RangeSubselect.Alias; a != nil {
			t.Alias = a.Aliasname
			for _, cn := range a.Colnames {
				t.Columns = append(t.Columns, cn.GetString_().GetSval())
			}
		}
		return t, nil, nil
	case *pg.Node_JoinExpr:
		j := f.JoinExpr
		left, leftJoins, err := c.fromItem(j.Larg)
		if err != nil {
			return nil, nil, err
		}
		right, rightJoins, err := c.fromItem(j.Rarg)
		if err != nil {
			return nil, nil, err
		}
		if len(rightJoins) > 0 {
			return nil, nil, c.unsupported("nested join on the right")
		}
		if j.IsNatural {
			return nil, nil, c.unsupported("NATURAL JOIN")
		}
		kind := sqlir.InnerJoin
		switch j.Jointype {
		case pg.JoinType_JOIN_LEFT:
			kind = sqlir.LeftJoin
		case pg.JoinType_JOIN_RIGHT:
			kind = sqlir.RightJoin
		case pg.JoinType_JOIN_FULL:
			kind = sqlir.FullJoin
		case pg.JoinType_JOIN_INNER:
			if j.Quals == nil {
				kind = sqlir.CrossJoin
			}
		default:
			return nil, nil, c.unsupported("join type")
		}
		if len(j.UsingClause) > 0 {
			return nil, nil, c.unsupported("JOIN USING")
		}
		var on sqlir.Expr
		if j.Quals != nil {
			if on, err = c.expr(j.Quals); err != nil {
				return nil, nil, err
			}
		}
		return left, append(leftJoins, sqlir.Join{Kind: kind, Table: *right, On: on}), nil
	}
	return nil, nil, c.unsupported("from item")
}

func (c *pgConv) targets(list []*pg.Node) ([]sqlir.Target, error) {
	var out []sqlir.Target
	for _, t := range list {
		rt := t.GetResTarget()
		if cr := rt.Val.GetColumnRef(); cr != nil && len(cr.Fields) > 0 && cr.Fields[len(cr.Fields)-1].GetAStar() != nil {
			out = append(out, sqlir.Target{Star: true})
			continue
		}
		e, err := c.expr(rt.Val)
		if err != nil {
			return nil, err
		}
		out = append(out, sqlir.Target{Expr: e, Alias: rt.Name})
	}
	return out, nil
}

func (c *pgConv) insertStmt(s *pg.InsertStmt) (sqlir.Statement, error) {
	out := &sqlir.InsertStmt{Table: rangeVarName(s.Relation)}
	if s.Relation.Alias != nil {
		out.Alias = s.Relation.Alias.Aliasname
	}
	for _, col := range s.Cols {
		name, err := c.targetColumn(col.GetResTarget())
		if err != nil {
			return nil, err
		}
		out.Columns = append(out.Columns, name)
	}
	sel := s.SelectStmt.GetSelectStmt()
	if sel == nil {
		return nil, c.unsupported("INSERT without VALUES or SELECT")
	}
	if len(sel.ValuesLists) > 0 {
		for _, vl := range sel.ValuesLists {
			var row []sqlir.Expr
			for _, it := range vl.GetList().GetItems() {
				e, err := c.expr(it)
				if err != nil {
					return nil, err
				}
				row = append(row, e)
			}
			out.Rows = append(out.Rows, row)
		}
	} else {
		q, err := c.selectStmt(sel)
		if err != nil {
			return nil, err
		}
		out.Select = q
	}
	if oc := s.OnConflictClause; oc != nil {
		conf := &sqlir.OnConflict{}
		if oc.Infer != nil {
			for _, ie := range oc.Infer.IndexElems {
				conf.Columns = append(conf.Columns, ie.GetIndexElem().GetName())
			}
		}
		switch oc.Action {
		case pg.OnConflictAction_ONCONFLICT_NOTHING:
			conf.DoNothing = true
		case pg.OnConflictAction_ONCONFLICT_UPDATE:
			for _, t := range oc.TargetList {
				rt := t.GetResTarget()
				name, err := c.targetColumn(rt)
				if err != nil {
					return nil, err
				}
				e, err := c.expr(rt.Val)
				if err != nil {
					return nil, err
				}
				conf.Set = append(conf.Set, sqlir.Assignment{Column: name, Value: e})
			}
			if oc.WhereClause != nil {
				w, err := c.expr(oc.WhereClause)
				if err != nil {
					return nil, err
				}
				conf.Where = w
			}
		default:
			return nil, c.unsupported("ON CONFLICT action")
		}
		out.OnConflict = conf
	}
	var err error
	if out.Returning, err = c.targets(s.ReturningList); err != nil {
		return nil, err
	}
	return out, nil
}

// targetColumn is the column an INSERT or SET target writes. A subscript or
// field (tags[1], addr.city) writes part of the column, which detest would
// otherwise apply to the whole of it.
func (c *pgConv) targetColumn(rt *pg.ResTarget) (string, error) {
	if len(rt.Indirection) > 0 {
		return "", c.unsupported("subscript or field in a target column")
	}
	return rt.Name, nil
}

func (c *pgConv) updateStmt(s *pg.UpdateStmt) (sqlir.Statement, error) {
	if s.WithClause != nil {
		return nil, c.unsupported("CTE on UPDATE")
	}
	out := &sqlir.UpdateStmt{Table: rangeVarName(s.Relation)}
	if s.Relation.Alias != nil {
		out.Alias = s.Relation.Alias.Aliasname
	}
	for _, t := range s.TargetList {
		rt := t.GetResTarget()
		name, err := c.targetColumn(rt)
		if err != nil {
			return nil, err
		}
		e, err := c.expr(rt.Val)
		if err != nil {
			return nil, err
		}
		out.Set = append(out.Set, sqlir.Assignment{Column: name, Value: e})
	}
	for _, f := range s.FromClause {
		t, joins, err := c.fromItem(f)
		if err != nil {
			return nil, err
		}
		if len(joins) > 0 {
			return nil, c.unsupported("JOIN in UPDATE FROM")
		}
		out.From = append(out.From, *t)
	}
	var err error
	if s.WhereClause != nil {
		if out.Where, err = c.expr(s.WhereClause); err != nil {
			return nil, err
		}
	}
	if out.Returning, err = c.targets(s.ReturningList); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *pgConv) deleteStmt(s *pg.DeleteStmt) (sqlir.Statement, error) {
	if s.WithClause != nil {
		return nil, c.unsupported("CTE on DELETE")
	}
	out := &sqlir.DeleteStmt{Table: rangeVarName(s.Relation)}
	if s.Relation.Alias != nil {
		out.Alias = s.Relation.Alias.Aliasname
	}
	for _, u := range s.UsingClause {
		t, joins, err := c.fromItem(u)
		if err != nil {
			return nil, err
		}
		if len(joins) > 0 {
			return nil, c.unsupported("JOIN in DELETE USING")
		}
		out.Using = append(out.Using, *t)
	}
	var err error
	if s.WhereClause != nil {
		if out.Where, err = c.expr(s.WhereClause); err != nil {
			return nil, err
		}
	}
	if out.Returning, err = c.targets(s.ReturningList); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *pgConv) exprs(list []*pg.Node) ([]sqlir.Expr, error) {
	var out []sqlir.Expr
	for _, n := range list {
		e, err := c.expr(n)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (c *pgConv) expr(n *pg.Node) (sqlir.Expr, error) {
	if n == nil {
		return nil, c.unsupported("empty expression")
	}
	switch e := n.Node.(type) {
	case *pg.Node_ColumnRef:
		f := e.ColumnRef.Fields
		switch len(f) {
		case 1:
			return &sqlir.ColumnRef{Column: f[0].GetString_().GetSval()}, nil
		case 2:
			return &sqlir.ColumnRef{Table: f[0].GetString_().GetSval(), Column: f[1].GetString_().GetSval()}, nil
		}
		return nil, c.unsupported("column reference")
	case *pg.Node_ParamRef:
		return &sqlir.Param{Index: int(e.ParamRef.Number) - 1}, nil
	case *pg.Node_AConst:
		if e.AConst.Isnull {
			return &sqlir.Const{}, nil
		}
		switch v := e.AConst.Val.(type) {
		case *pg.A_Const_Ival:
			return &sqlir.Const{Value: int64(v.Ival.Ival)}, nil
		case *pg.A_Const_Fval:
			// pg_query gives an integer too large for int4 as a float literal.
			if i, err := strconv.ParseInt(v.Fval.Fval, 10, 64); err == nil {
				return &sqlir.Const{Value: i}, nil
			}
			if f, err := strconv.ParseFloat(v.Fval.Fval, 64); err == nil {
				return &sqlir.Const{Value: f}, nil
			}
			return &sqlir.Const{Value: v.Fval.Fval}, nil
		case *pg.A_Const_Sval:
			return &sqlir.Const{Value: v.Sval.Sval}, nil
		case *pg.A_Const_Boolval:
			return &sqlir.Const{Value: v.Boolval.Boolval}, nil
		}
		return nil, c.unsupported("constant")
	case *pg.Node_TypeCast:
		x, err := c.expr(e.TypeCast.Arg)
		if err != nil {
			return nil, err
		}
		names := e.TypeCast.TypeName.GetNames()
		typ := strings.ToLower(names[len(names)-1].GetString_().GetSval())
		return &sqlir.Cast{X: x, Type: typ}, nil
	case *pg.Node_AExpr:
		return c.aExpr(e.AExpr)
	case *pg.Node_BoolExpr:
		args, err := c.exprs(e.BoolExpr.Args)
		if err != nil {
			return nil, err
		}
		switch e.BoolExpr.Boolop {
		case pg.BoolExprType_NOT_EXPR:
			return &sqlir.UnaryExpr{Op: "NOT", X: args[0]}, nil
		case pg.BoolExprType_AND_EXPR, pg.BoolExprType_OR_EXPR:
			op := "AND"
			if e.BoolExpr.Boolop == pg.BoolExprType_OR_EXPR {
				op = "OR"
			}
			acc := args[0]
			for _, a := range args[1:] {
				acc = &sqlir.BinaryExpr{Op: op, L: acc, R: a}
			}
			return acc, nil
		}
	case *pg.Node_NullTest:
		x, err := c.expr(e.NullTest.Arg)
		if err != nil {
			return nil, err
		}
		return &sqlir.IsNull{X: x, Not: e.NullTest.Nulltesttype == pg.NullTestType_IS_NOT_NULL}, nil
	case *pg.Node_SubLink:
		sel, err := c.selectStmt(e.SubLink.Subselect.GetSelectStmt())
		if err != nil {
			return nil, err
		}
		switch e.SubLink.SubLinkType {
		case pg.SubLinkType_EXISTS_SUBLINK:
			return &sqlir.Exists{Select: sel}, nil
		case pg.SubLinkType_EXPR_SUBLINK:
			return &sqlir.SubQuery{Select: sel}, nil
		case pg.SubLinkType_ANY_SUBLINK:
			x, err := c.expr(e.SubLink.Testexpr)
			if err != nil {
				return nil, err
			}
			not := false
			if len(e.SubLink.OperName) > 0 && e.SubLink.OperName[0].GetString_().GetSval() == "<>" {
				not = true
			}
			return &sqlir.InExpr{X: x, Sub: sel, Not: not}, nil
		}
		return nil, c.unsupported("subquery kind")
	case *pg.Node_FuncCall:
		fc := e.FuncCall
		switch {
		case fc.AggFilter != nil:
			return nil, c.unsupported("aggregate FILTER")
		case len(fc.AggOrder) > 0:
			return nil, c.unsupported("ORDER BY in an aggregate")
		case fc.AggWithinGroup:
			return nil, c.unsupported("WITHIN GROUP")
		}
		args, err := c.exprs(fc.Args)
		if err != nil {
			return nil, err
		}
		call := &sqlir.FuncCall{Name: strings.ToLower(fc.Funcname[len(fc.Funcname)-1].GetString_().GetSval()), Args: args, Star: fc.AggStar, Distinct: fc.AggDistinct}
		if fc.Over != nil {
			return c.window(call, fc.Over)
		}
		return call, nil
	case *pg.Node_NamedArgExpr:
		return c.expr(e.NamedArgExpr.Arg)
	case *pg.Node_CoalesceExpr:
		args, err := c.exprs(e.CoalesceExpr.Args)
		if err != nil {
			return nil, err
		}
		return &sqlir.FuncCall{Name: "coalesce", Args: args}, nil
	case *pg.Node_MinMaxExpr:
		args, err := c.exprs(e.MinMaxExpr.Args)
		if err != nil {
			return nil, err
		}
		name := "greatest"
		if e.MinMaxExpr.Op == pg.MinMaxOp_IS_LEAST {
			name = "least"
		}
		return &sqlir.FuncCall{Name: name, Args: args}, nil
	case *pg.Node_SqlvalueFunction:
		switch e.SqlvalueFunction.Op {
		case pg.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP, pg.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP:
			return &sqlir.FuncCall{Name: "now"}, nil
		case pg.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP_N, pg.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP_N:
			// current_timestamp with an argument cannot be written as a call,
			// so it is free to carry the precision to round to.
			return &sqlir.FuncCall{Name: "current_timestamp", Args: []sqlir.Expr{&sqlir.Const{Value: int64(e.SqlvalueFunction.Typmod)}}}, nil
		}
		return nil, c.unsupported("SQL value function " + e.SqlvalueFunction.Op.String())
	case *pg.Node_CaseExpr:
		out := &sqlir.CaseExpr{}
		var err error
		if e.CaseExpr.Arg != nil {
			if out.Arg, err = c.expr(e.CaseExpr.Arg); err != nil {
				return nil, err
			}
		}
		for _, w := range e.CaseExpr.Args {
			cw := w.GetCaseWhen()
			when, err := c.expr(cw.Expr)
			if err != nil {
				return nil, err
			}
			then, err := c.expr(cw.Result)
			if err != nil {
				return nil, err
			}
			out.Whens = append(out.Whens, sqlir.CaseWhen{When: when, Then: then})
		}
		if e.CaseExpr.Defresult != nil {
			if out.Else, err = c.expr(e.CaseExpr.Defresult); err != nil {
				return nil, err
			}
		}
		return out, nil
	case *pg.Node_RowExpr:
		items, err := c.exprs(e.RowExpr.Args)
		if err != nil {
			return nil, err
		}
		return &sqlir.RowExpr{Items: items}, nil
	case *pg.Node_SetToDefault:
		return &sqlir.Default{}, nil
	case *pg.Node_List:
		items, err := c.exprs(e.List.Items)
		if err != nil {
			return nil, err
		}
		return &sqlir.RowExpr{Items: items}, nil
	}
	return nil, c.unsupported(fmt.Sprintf("expression %T", n.Node))
}

// isConstElement reports whether an array element is a constant or a
// parameter, possibly cast to text or varchar as pg_dump writes it, whose
// evaluation cannot fail and keeps its value. A number that becomes text,
// through its own cast or the array's (textCast), is not one: Postgres keeps
// its digits as written (1.20), which detest's number does not.
func isConstElement(n *pg.Node, textCast bool) bool {
	for n.GetTypeCast() != nil {
		if _, ok := plainTextCast(n.GetTypeCast(), false, "text", "varchar"); !ok {
			return false
		}
		textCast = true
		n = n.GetTypeCast().Arg
	}
	if k := n.GetAConst(); k != nil {
		return !textCast || k.Isnull || k.GetSval() != nil
	}
	return n.GetParamRef() != nil
}

// plainTextCast returns the type of a cast to one of types, or to an array
// of one, without a type modifier, such as varchar(1), which would change the
// value.
func plainTextCast(tc *pg.TypeCast, array bool, types ...string) (string, bool) {
	if len(tc.TypeName.GetTypmods()) > 0 || (len(tc.TypeName.GetArrayBounds()) > 0) != array {
		return "", false
	}
	names := tc.TypeName.GetNames()
	typ := strings.ToLower(names[len(names)-1].GetString_().GetSval())
	return typ, slices.Contains(types, typ)
}

func (c *pgConv) aExpr(e *pg.A_Expr) (sqlir.Expr, error) {
	op := ""
	if len(e.Name) > 0 {
		op = e.Name[0].GetString_().GetSval()
	}
	switch e.Kind {
	case pg.A_Expr_Kind_AEXPR_OP:
		if e.Lexpr == nil { // unary minus
			x, err := c.expr(e.Rexpr)
			if err != nil {
				return nil, err
			}
			return &sqlir.UnaryExpr{Op: op, X: x}, nil
		}
		l, err := c.expr(e.Lexpr)
		if err != nil {
			return nil, err
		}
		r, err := c.expr(e.Rexpr)
		if err != nil {
			return nil, err
		}
		switch op {
		case "~~":
			op = "LIKE"
		case "!~~":
			op = "NOT LIKE"
		case "~~*":
			op = "ILIKE"
		case "!~~*":
			op = "NOT ILIKE"
		}
		return &sqlir.BinaryExpr{Op: op, L: l, R: r}, nil
	case pg.A_Expr_Kind_AEXPR_IN:
		l, err := c.expr(e.Lexpr)
		if err != nil {
			return nil, err
		}
		var list []sqlir.Expr
		if lst := e.Rexpr.GetList(); lst != nil {
			if list, err = c.exprs(lst.Items); err != nil {
				return nil, err
			}
		} else {
			r, err := c.expr(e.Rexpr)
			if err != nil {
				return nil, err
			}
			list = []sqlir.Expr{r}
		}
		return &sqlir.InExpr{X: l, List: list, Not: op == "<>"}, nil
	case pg.A_Expr_Kind_AEXPR_OP_ANY, pg.A_Expr_Kind_AEXPR_OP_ALL:
		// Postgres stores IN (...) as = ANY (ARRAY[...]), so pg_dump writes a
		// CHECK (status IN ('a', 'b')) back in this form. Only the two forms
		// that mean IN and NOT IN over a literal array are converted.
		isAny := e.Kind == pg.A_Expr_Kind_AEXPR_OP_ANY
		if (!isAny || op != "=") && (isAny || op != "<>") {
			return nil, c.unsupported("operator " + op + " with ANY or ALL")
		}
		arr := e.Rexpr
		var casts []string // outermost first
		for arr.GetTypeCast() != nil {
			// pg_dump casts the array only to text[]. A cast to another type
			// coerces each element in Postgres, as '01' to 1 for int[] or
			// 'a ' to 'a' for bpchar[], which detest's casts do not do.
			typ, ok := plainTextCast(arr.GetTypeCast(), true, "text")
			if !ok {
				return nil, c.unsupported("ANY or ALL over an array cast to a type other than text[]")
			}
			casts = append(casts, typ)
			arr = arr.GetTypeCast().Arg
		}
		if arr.GetAArrayExpr() == nil {
			return nil, c.unsupported("ANY or ALL over an array other than ARRAY[...]")
		}
		// IN () has no NULL-free answer for a NULL operand, while = ANY and
		// <> ALL over an empty array are false and true whatever the operand.
		if len(arr.GetAArrayExpr().Elements) == 0 {
			return nil, c.unsupported("ANY or ALL over an empty array")
		}
		// Postgres builds the whole array before comparing, while IN stops
		// at the first match, so an element that can fail (1/0) would raise
		// in one and not the other.
		for _, el := range arr.GetAArrayExpr().Elements {
			if !isConstElement(el, len(casts) > 0) {
				return nil, c.unsupported("ANY or ALL over an array with an element other than a constant or parameter")
			}
		}
		l, err := c.expr(e.Lexpr)
		if err != nil {
			return nil, err
		}
		list, err := c.exprs(arr.GetAArrayExpr().Elements)
		if err != nil {
			return nil, err
		}
		// A cast of the array casts each element, so it is kept on them:
		// x = ANY ((ARRAY['1'])::text[]) compares x with the text '1'.
		for i := range list {
			for _, typ := range slices.Backward(casts) {
				list[i] = &sqlir.Cast{X: list[i], Type: typ}
			}
		}
		return &sqlir.InExpr{X: l, List: list, Not: !isAny}, nil
	case pg.A_Expr_Kind_AEXPR_LIKE, pg.A_Expr_Kind_AEXPR_ILIKE:
		l, err := c.expr(e.Lexpr)
		if err != nil {
			return nil, err
		}
		r, err := c.expr(e.Rexpr)
		if err != nil {
			return nil, err
		}
		bop := "LIKE"
		if e.Kind == pg.A_Expr_Kind_AEXPR_ILIKE {
			bop = "ILIKE"
		}
		if strings.HasPrefix(op, "!") {
			bop = "NOT " + bop
		}
		return &sqlir.BinaryExpr{Op: bop, L: l, R: r}, nil
	case pg.A_Expr_Kind_AEXPR_NULLIF:
		l, err := c.expr(e.Lexpr)
		if err != nil {
			return nil, err
		}
		r, err := c.expr(e.Rexpr)
		if err != nil {
			return nil, err
		}
		return &sqlir.FuncCall{Name: "nullif", Args: []sqlir.Expr{l, r}}, nil
	case pg.A_Expr_Kind_AEXPR_BETWEEN, pg.A_Expr_Kind_AEXPR_NOT_BETWEEN:
		x, err := c.expr(e.Lexpr)
		if err != nil {
			return nil, err
		}
		lst := e.Rexpr.GetList()
		if lst == nil || len(lst.Items) != 2 {
			return nil, c.unsupported("BETWEEN bounds")
		}
		lo, err := c.expr(lst.Items[0])
		if err != nil {
			return nil, err
		}
		hi, err := c.expr(lst.Items[1])
		if err != nil {
			return nil, err
		}
		var between sqlir.Expr = &sqlir.BinaryExpr{Op: "AND", L: &sqlir.BinaryExpr{Op: ">=", L: x, R: lo}, R: &sqlir.BinaryExpr{Op: "<=", L: x, R: hi}}
		if e.Kind == pg.A_Expr_Kind_AEXPR_NOT_BETWEEN {
			between = &sqlir.UnaryExpr{Op: "NOT", X: between}
		}
		return between, nil
	}
	return nil, c.unsupported("operator kind " + e.Kind.String())
}
