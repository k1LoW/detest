// Package postgres is PostgreSQL for detest: pass postgres.New() to
// detest.Sim.DB. It parses SQL with the real PostgreSQL grammar (pg_query
// compiled to wasm, pure Go) and converts the AST into detest's IR.
package postgres

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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
	timeZone   *time.Location
	zoneErr    error
	collation  sqlir.Collation
	collations map[string]sqlir.Collation
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

// TimeZone sets the server's TimeZone, which a session starts in and SET
// TIME ZONE LOCAL, DEFAULT and RESET return to (default "UTC"), as
// postgresql.conf sets it. name is an IANA time zone name, such as
// "Asia/Tokyo", which time.LoadLocation reads; declaring a database on a
// server whose zone does not load fails.
//
// The zone decides how Postgres converts between a timestamp and a
// timestamptz, and the date, the fields, date_trunc and the days and months
// added to a timestamptz.
func TimeZone(name string) Option {
	return func(c *config) {
		c.timeZone, c.zoneErr = loadZone(name)
	}
}

// Collation sets the database's collation, which orders text that declares
// none: ORDER BY, <, min and max over it (default C, which orders text byte
// by byte). It must be the collation the real database has, or the
// statements that order text read and lock other rows than they do in
// production. pg_database shows it by its provider, datlocprovider:
// datcollate for libc, and for ICU and the builtin provider, whose C and
// C.UTF-8 order as C does, datlocale on Postgres 17 and later and
// daticulocale on 15 and 16. Before 15 there is libc only, and datcollate.
// Equality does not depend on
// it, as Postgres tells two strings equal only when their bytes are.
func Collation(c sqlir.Collation) Option {
	return func(cf *config) { cf.collation = c }
}

// Collations declares the collations COLLATE and a column may name, besides
// "C", "POSIX" and "default", which detest knows. An unqualified name is
// declared as it is written, such as en_US.utf8, and a name in a schema
// other than pg_catalog with each part double quoted, as `"app"."name"`. A
// statement that orders text by a collation neither names is refused.
func Collations(named map[string]sqlir.Collation) Option {
	return func(cf *config) { cf.collations = named }
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
		Name:          "postgres",
		Parser:        parser{},
		Isolation:     c.isolation,
		Supported:     []sqlir.IsolationLevel{sqlir.ReadCommitted},
		SearchPath:    c.searchPath,
		Codes:         codes,
		Convert:       c.convert,
		TimeZone:      c.timeZone,
		TimeZoneErr:   c.zoneErr,
		TextCollation: c.collation,
		Collations:    c.collations,
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
	case sqlir.InvalidDatetimeFormat:
		return "22007", 0
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
	case sqlir.InvalidParameterValue:
		return "22023", 0
	case sqlir.InvalidRowCountInLimit:
		return "2201W", 0
	case sqlir.InvalidRowCountInOffset:
		return "2201X", 0
	case sqlir.CardinalityViolation:
		return "21000", 0
	case sqlir.ForeignKeyParentViolation:
		return "23503", 0
	case sqlir.RestrictViolation:
		return "23001", 0
	case sqlir.UndefinedColumn:
		return "42703", 0
	case sqlir.AmbiguousColumn:
		return "42702", 0
	case sqlir.DuplicateColumn:
		return "42701", 0
	case sqlir.ArithmeticOutOfRange:
		return "22003", 0
	case sqlir.LockWaitTimeout:
		return "55P03", 0 // lock_timeout reports lock_not_available, as NOWAIT does
	case sqlir.StringDataRightTruncation:
		return "22001", 0
	case sqlir.DataTruncated:
		return "22P02", 0 // invalid input value for enum
	}
	return "", 0
}

// The parser runs PostgreSQL's grammar as a WebAssembly module, which it
// compiles on its first parse in the process. That takes seconds under the
// race detector, so it is done before Explore watches for stalls rather than
// in the first statement of a run. Compiling it here in init instead would
// make every binary that imports the package pay for it.
func init() {
	var once sync.Once
	sqlir.RegisterWarmup(func() {
		once.Do(func() { _, _ = parser{}.Parse("SELECT 1") })
	})
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
	// lockKey is set while the arguments of an advisory lock function are
	// converted, where hashtext may stand.
	lockKey bool
}

func (c *pgConv) unsupported(what string) error { return sqlir.Unsupported(what, c.query) }

// transactionSet refuses the SETs that change what a transaction is. SET
// TRANSACTION, SET TRANSACTION SNAPSHOT and the transaction_isolation and
// transaction_read_only settings are refused in every form, as MySQL's SET
// TRANSACTION is: Postgres fails them with 25001 once the transaction has
// run a query, which detest does not track, and the level and the read-only
// state they set are the driver's to take from BeginTx. The session defaults,
// SET SESSION CHARACTERISTICS AS TRANSACTION and default_transaction_*, have
// no such timing rule and run when they ask for what detest runs anyway, Read
// Committed (Read Uncommitted is Read Committed in Postgres) and READ WRITE.
// A read-only default would fail every write with 25006, which detest does
// not model. DEFERRABLE changes nothing outside Serializable and is ignored.
func (c *pgConv) transactionSet(v *pg.VariableSetStmt) error {
	var level, readOnly string
	switch v.Kind {
	case pg.VariableSetKind_VAR_SET_MULTI:
		switch v.Name {
		case "TRANSACTION":
			return c.unsupported("SET TRANSACTION (use database/sql's BeginTx)")
		case "TRANSACTION SNAPSHOT":
			return c.unsupported("SET TRANSACTION SNAPSHOT")
		case "SESSION CHARACTERISTICS":
		default:
			return nil
		}
		seen := map[string]bool{}
		for _, a := range v.Args {
			d := a.GetDefElem()
			if seen[d.GetDefname()] {
				// Postgres refuses a mode given twice (42601), rather than
				// letting the last one win.
				return c.unsupported("a transaction mode given twice (" + d.GetDefname() + ")")
			}
			seen[d.GetDefname()] = true
			switch d.GetDefname() {
			case "transaction_isolation":
				level = d.Arg.GetAConst().GetSval().GetSval()
			case "transaction_read_only":
				readOnly = strconv.FormatInt(int64(d.Arg.GetAConst().GetIval().GetIval()), 10)
			}
		}
	case pg.VariableSetKind_VAR_SET_VALUE, pg.VariableSetKind_VAR_RESET, pg.VariableSetKind_VAR_SET_DEFAULT, pg.VariableSetKind_VAR_SET_CURRENT:
		switch v.Name {
		case "transaction_isolation", "transaction_read_only":
			return c.unsupported("SET " + v.Name + " (use database/sql's BeginTx)")
		case "default_transaction_isolation", "default_transaction_read_only":
		default:
			return nil
		}
		if v.Kind == pg.VariableSetKind_VAR_SET_CURRENT {
			// Takes the session's value, which detest does not track.
			return c.unsupported("SET " + v.Name + " FROM CURRENT")
		}
		var val string
		if len(v.Args) > 0 {
			// The constant as written: a string, or the integer or boolean
			// a boolean setting also takes (= 1, = on, = 'true').
			if e, err := c.expr(v.Args[0]); err == nil {
				if k, ok := e.(*sqlir.Const); ok && k.Value != nil {
					val = fmt.Sprint(k.Value)
				}
			}
			if val == "" {
				// Postgres refuses an empty value for these settings
				// (22023), where RESET and TO DEFAULT, which carry no
				// value, restore the default.
				return c.unsupported("SET " + v.Name + " to an empty value")
			}
		}
		if v.Name == "default_transaction_isolation" {
			level = val
		} else {
			readOnly = val
		}
	default:
		return nil
	}
	switch strings.ToLower(level) {
	case "", "read committed", "read uncommitted":
	default:
		return c.unsupported("an isolation level other than Read Committed set by SQL (" + strings.ToUpper(level) + ")")
	}
	switch strings.ToLower(readOnly) {
	case "", "0", "off", "false", "no":
		return nil
	}
	return c.unsupported("a read-only transaction set by SQL (READ ONLY)")
}

func (c *pgConv) stmt(n *pg.Node) (sqlir.Statement, error) {
	switch s := n.Node.(type) {
	case *pg.Node_SelectStmt:
		if path, ok := setConfigSearchPath(s.SelectStmt); ok {
			// A schema dump sets search_path with SELECT pg_catalog.set_config(...),
			// which is checked as SET search_path is. The row it returns
			// carries the alias when the SELECT gives one.
			col := s.SelectStmt.TargetList[0].GetResTarget().GetName()
			return &sqlir.SetStmt{Name: "search_path", Value: path, Returns: true, Column: col}, nil
		}
		return c.selectStmt(s.SelectStmt)
	case *pg.Node_CreateTableAsStmt:
		return c.createTableAs(s.CreateTableAsStmt)
	case *pg.Node_VariableSetStmt:
		v := s.VariableSetStmt
		if err := c.transactionSet(v); err != nil {
			return nil, err
		}
		if zone, ok, err := c.timeZoneSet(v); err != nil {
			return nil, err
		} else if ok {
			return &sqlir.SetStmt{Name: "timezone", Local: v.IsLocal, Zone: zone}, nil
		}
		if v.Kind == pg.VariableSetKind_VAR_RESET_ALL {
			// Resets every setting, among them lock_timeout, the one
			// detest acts on; "all" is no setting's name.
			return &sqlir.SetStmt{Name: "all"}, nil
		}
		out := &sqlir.SetStmt{Name: v.Name, Local: v.IsLocal}
		// The constants as written, joined as a list setting such as
		// search_path is (SET search_path TO app, public).
		var vals []string
		for _, a := range v.Args {
			if e, err := c.expr(a); err == nil {
				if k, ok := e.(*sqlir.Const); ok && k.Value != nil {
					vals = append(vals, fmt.Sprint(k.Value))
				}
			}
		}
		out.Value = strings.Join(vals, ", ")
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
		u.Index = true
		return []sqlir.SchemaChange{{Table: rangeVarName(ix.Relation), Constraints: []sqlir.UniqueDef{u}}}, true, nil
	case *pg.Node_DropStmt:
		if s.DropStmt.RemoveType == pg.ObjectType_OBJECT_DOMAIN {
			for _, obj := range s.DropStmt.Objects {
				var parts []string
				for _, n := range obj.GetTypeName().GetNames() {
					parts = append(parts, n.GetString_().GetSval())
				}
				if len(parts) == 0 {
					continue
				}
				changes = append(changes, sqlir.SchemaChange{Table: strings.Join(parts, "."), Object: "domain", Drop: true,
					Columns: []sqlir.ColumnDef{{Name: parts[len(parts)-1]}}})
			}
			return changes, true, nil
		}
		object, ok := map[pg.ObjectType]string{pg.ObjectType_OBJECT_TABLE: "", pg.ObjectType_OBJECT_VIEW: "view",
			pg.ObjectType_OBJECT_MATVIEW: "matview", pg.ObjectType_OBJECT_INDEX: "index", pg.ObjectType_OBJECT_SEQUENCE: "sequence"}[s.DropStmt.RemoveType]
		if !ok {
			return nil, true, nil
		}
		for _, obj := range s.DropStmt.Objects {
			var parts []string
			for _, p := range obj.GetList().GetItems() {
				parts = append(parts, p.GetString_().GetSval())
			}
			changes = append(changes, sqlir.SchemaChange{Table: strings.Join(parts, "."), Drop: true, IfExists: s.DropStmt.MissingOk, Object: object,
				Cascade: s.DropStmt.Behavior == pg.DropBehavior_DROP_CASCADE})
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
		case pg.ObjectType_OBJECT_DOMAIN, pg.ObjectType_OBJECT_TYPE:
			// A domain's collation follows it to its new name; renaming
			// another type changes nothing detest keeps.
			var parts []string
			for _, n := range r.Object.GetList().GetItems() {
				parts = append(parts, n.GetString_().GetSval())
			}
			if len(parts) == 0 {
				return nil, true, nil
			}
			// Columns[0].Name is the bare name, which a dot in a quoted
			// identifier would make Table's last part misread.
			return []sqlir.SchemaChange{{Table: strings.Join(parts, "."), Object: "domain", RenameTo: r.Newname,
				Columns: []sqlir.ColumnDef{{Name: parts[len(parts)-1]}}}}, true, nil
		}
		return nil, true, nil
	case *pg.Node_DoStmt:
		// detest cannot run PL/pgSQL. In migrations a DO block usually checks the
		// catalog and raises, declaring nothing, so it runs as nothing.
		return nil, true, nil
	case *pg.Node_CreateSeqStmt:
		opts, _, err := c.sequenceOptions(s.CreateSeqStmt.Options)
		if err != nil {
			return nil, true, err
		}
		return []sqlir.SchemaChange{{Table: rangeVarName(s.CreateSeqStmt.Sequence), Object: "sequence", Create: true, IfNotExists: s.CreateSeqStmt.IfNotExists, Sequence: opts}}, true, nil
	case *pg.Node_AlterSeqStmt:
		opts, _, err := c.sequenceOptions(s.AlterSeqStmt.Options)
		if err != nil {
			return nil, true, err
		}
		return []sqlir.SchemaChange{{Table: rangeVarName(s.AlterSeqStmt.Sequence), Object: "sequence", Sequence: opts, IfExists: s.AlterSeqStmt.MissingOk}}, true, nil
	case *pg.Node_CreateDomainStmt:
		// Only the collation is kept: a column of the domain orders by it.
		// detest checks neither the domain's base type nor its constraints.
		names := s.CreateDomainStmt.Domainname
		if len(names) == 0 {
			return nil, true, nil
		}
		name := names[len(names)-1].GetString_().GetSval()
		var parts []string
		for _, n := range names {
			parts = append(parts, n.GetString_().GetSval())
		}
		// Table is the qualified name, so that domains of one name in two
		// schemas can be told apart; Columns[0].Name is the bare name a
		// column's type is written with.
		return []sqlir.SchemaChange{{Table: strings.Join(parts, "."), Object: "domain", Create: true,
			Columns: []sqlir.ColumnDef{{Name: name, Type: typeName(s.CreateDomainStmt.TypeName), Collation: collationName(s.CreateDomainStmt.CollClause)}}}}, true, nil
	case *pg.Node_CommentStmt, *pg.Node_CreateFunctionStmt, *pg.Node_CreateExtensionStmt,
		*pg.Node_CreateSchemaStmt, *pg.Node_GrantStmt, *pg.Node_GrantRoleStmt,
		*pg.Node_AlterOwnerStmt, *pg.Node_CreateTrigStmt,
		*pg.Node_AlterDefaultPrivilegesStmt, *pg.Node_CreateEnumStmt,
		*pg.Node_CompositeTypeStmt, *pg.Node_DefineStmt,
		*pg.Node_AlterEnumStmt, *pg.Node_CreatePolicyStmt, *pg.Node_RuleStmt,
		*pg.Node_CreateStatsStmt, *pg.Node_AlterObjectSchemaStmt:
		return nil, true, nil
	}
	return nil, false, nil
}

// setConfigSearchPath recognizes SELECT [pg_catalog.]set_config('search_path',
// <value>, ...), which a schema dump writes to set the path, and returns the
// value. Any other call of set_config is a function call, which detest does
// not run and refuses, so that a setting it would change is not ignored.
func setConfigSearchPath(sel *pg.SelectStmt) (string, bool) {
	// The bare SELECT of the call and nothing else: a clause such as
	// WHERE false would decide whether the server evaluates it at all.
	if sel == nil || len(sel.TargetList) != 1 || sel.FromClause != nil || sel.WhereClause != nil ||
		sel.GroupClause != nil || sel.HavingClause != nil || sel.WindowClause != nil || sel.SortClause != nil ||
		sel.LimitCount != nil || sel.LimitOffset != nil || sel.DistinctClause != nil || sel.WithClause != nil ||
		sel.LockingClause != nil || sel.ValuesLists != nil || sel.IntoClause != nil || sel.Op != pg.SetOperation_SETOP_NONE {
		return "", false
	}
	fc := sel.TargetList[0].GetResTarget().GetVal().GetFuncCall()
	if fc == nil || len(fc.Funcname) == 0 || len(fc.Funcname) > 2 || len(fc.Args) != 3 ||
		fc.AggDistinct || fc.AggStar || fc.Over != nil || fc.AggFilter != nil || len(fc.AggOrder) > 0 || fc.AggWithinGroup {
		return "", false
	}
	if len(fc.Funcname) == 2 && fc.Funcname[0].GetString_().GetSval() != "pg_catalog" {
		return "", false
	}
	if fc.Funcname[len(fc.Funcname)-1].GetString_().GetSval() != "set_config" {
		return "", false
	}
	name, value := fc.Args[0].GetAConst().GetSval(), fc.Args[1].GetAConst().GetSval()
	if name == nil || name.GetSval() != "search_path" || value == nil || fc.Args[2].GetAConst().GetBoolval() == nil {
		return "", false
	}
	return value.GetSval(), true
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
			chk, ok, err := c.check(e.Constraint, ch.Table)
			if err != nil {
				return ch, err
			}
			if ok {
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

// check converts a CHECK constraint. One detest cannot convert is kept as
// Unconverted, as a default it cannot convert is: the schema still
// loads, and a write to the table fails as unsupported instead of passing a
// constraint the server would have checked.
func (c *pgConv) check(k *pg.Constraint, table string) (sqlir.CheckDef, bool, error) {
	if k == nil || k.Contype != pg.ConstrType_CONSTR_CHECK {
		return sqlir.CheckDef{}, false, nil
	}
	e, err := c.expr(k.RawExpr)
	if err != nil {
		return sqlir.CheckDef{Name: k.Conname, Expr: &sqlir.Unconverted{}}, true, nil
	}
	if err := c.ownColumns(e, table); err != nil {
		return sqlir.CheckDef{}, false, err
	}
	unqualify(e)
	return sqlir.CheckDef{Name: k.Conname, Expr: e}, true, nil
}

// ownColumns refuses a column reference qualified by a name other than
// table's, which a CHECK or generation expression cannot refer to.
func (c *pgConv) ownColumns(e sqlir.Expr, table string) error {
	for _, r := range sqlir.ColumnRefs(e) {
		if r.Table != "" && r.Table != relnameOf(table) && r.Table != table {
			return c.unsupported("a constraint or generated column referring to " + r.Table + "." + r.Column)
		}
	}
	return nil
}

// unqualify drops the table name from the column references of a CHECK or
// generation expression, which can only refer to its own table: kept, t.a
// would read nothing once the table is renamed.
func unqualify(e sqlir.Expr) {
	for _, r := range sqlir.ColumnRefs(e) {
		r.Table = ""
	}
}

// collationName is the name a COLLATE clause gives, empty without one. An
// unqualified name is kept as written, en_US.utf8 among them, and so is a
// name in pg_catalog, which pg_dump qualifies the built-in ones with
// (pg_catalog."C"). A name in any other schema is written with each part
// double quoted, "app"."name", since two schemas may have collations of the
// same name, and an unqualified collation may be named app.name itself.
func collationName(cc *pg.CollateClause) string {
	var parts []string
	for _, n := range cc.GetCollname() {
		parts = append(parts, n.GetString_().GetSval())
	}
	if len(parts) > 1 && parts[0] == "pg_catalog" {
		parts = parts[1:]
	}
	if len(parts) == 1 {
		return parts[0]
	}
	for i, p := range parts {
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(parts, ".")
}

// columnDef converts a column with the constraints written on it.
func (c *pgConv) columnDef(table string, d *pg.ColumnDef) (sqlir.ColumnDef, []sqlir.UniqueDef, []sqlir.CheckDef, error) {
	col := sqlir.ColumnDef{Name: d.Colname, Type: typeName(d.TypeName), Collation: collationName(d.CollClause)}
	if err := c.columnLimits(&col, d.TypeName); err != nil {
		return col, nil, nil, err
	}
	if d.RawDefault != nil {
		col.Default = c.defaultExpr(d.RawDefault)
	}
	if isSerial(d.TypeName) {
		// A serial column owns a sequence of its own, as an identity
		// column does, which CREATE SEQUENCE IF NOT EXISTS then finds.
		col.Default = sequenceDefault(table, d.Colname)
		col.Sequence = &sqlir.SequenceOptions{}
	}
	var cons []sqlir.UniqueDef
	var checks []sqlir.CheckDef
	var last *sqlir.UniqueDef
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
			chk, ok, err := c.check(k, table)
			if err != nil {
				return col, nil, nil, err
			}
			if ok {
				if chk.Name == "" {
					chk.Name = relnameOf(table) + "_" + d.Colname + "_check"
				}
				checks = append(checks, chk)
			}
		case pg.ConstrType_CONSTR_DEFAULT:
			col.Default = c.defaultExpr(k.RawExpr)
		case pg.ConstrType_CONSTR_IDENTITY:
			if err := c.identity(&col, table, k); err != nil {
				return col, nil, nil, err
			}
		case pg.ConstrType_CONSTR_GENERATED:
			g, err := c.expr(k.RawExpr)
			// Every refusal of an aggregate (string_agg, FILTER, ORDER BY,
			// WITHIN GROUP) reads "aggregate ...", and of a SQL value
			// function such as CURRENT_DATE "SQL value function ...":
			// Postgres refuses both here, not only detest.
			if u := (*sqlir.ErrUnsupportedSQL)(nil); errors.As(err, &u) && (strings.HasPrefix(u.What, "aggregate ") || strings.HasPrefix(u.What, "SQL value function ")) {
				return col, nil, nil, err
			}
			if err != nil {
				g = &sqlir.Unconverted{} // as defaultExpr: the schema still loads
			}
			col.Generated = g
			if err := c.immutable(col.Generated, relnameOf(table)); err != nil {
				return col, nil, nil, err
			}
			unqualify(col.Generated)
		case pg.ConstrType_CONSTR_PRIMARY, pg.ConstrType_CONSTR_UNIQUE:
			u, _, err := c.constraintDef(k, []string{d.Colname})
			if err != nil {
				return col, nil, nil, err
			}
			cons = append(cons, u)
			last = &cons[len(cons)-1]
		case pg.ConstrType_CONSTR_FOREIGN:
			last = nil // the attributes after it are the foreign key's
		case pg.ConstrType_CONSTR_ATTR_DEFERRABLE, pg.ConstrType_CONSTR_ATTR_DEFERRED:
			// The grammar gives DEFERRABLE and INITIALLY DEFERRED written
			// after a column constraint as constraints of their own.
			if last != nil {
				last.Deferrable = true
			}
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
	"statement_timestamp": true, "current_date": true, "random": true, "concat": true,
	"pg_try_advisory_xact_lock": true, "pg_advisory_xact_lock": true,
	"count": true, "sum": true, "min": true, "max": true, "avg": true,
}

// immutable refuses what Postgres does not allow in a generation expression:
// a function that is not immutable, such as nextval or now, an aggregate, a
// subquery or a parameter. detest would otherwise run it on every write.
func (c *pgConv) immutable(e sqlir.Expr, table string) error {
	for _, x := range sqlir.Exprs(e) {
		switch v := x.(type) {
		case *sqlir.ColumnRef:
			if err := c.ownColumns(v, table); err != nil {
				return err
			}
		case *sqlir.FuncCall:
			if mutableFuncs[v.Name] || sqlir.OtherAggregates[v.Name] {
				return c.unsupported("generated column calling " + v.Name)
			}
		case *sqlir.SubQuery, *sqlir.Exists, *sqlir.Param, *sqlir.WindowFunc:
			return c.unsupported("generated column with a subquery, parameter or window function")
		case *sqlir.InExpr:
			if v.Sub != nil {
				return c.unsupported("generated column with a subquery")
			}
		case *sqlir.Cast:
			// Whether a cast is immutable depends on the type it casts
			// from, as timestamptz::text depends on the time zone, which
			// detest does not know here. A cast to a number is immutable
			// from any type.
			switch v.Type {
			case "int2", "int4", "int8", "numeric", "float4", "float8":
			default:
				return c.unsupported("generated column with a cast to " + v.Type)
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
// becomes Unconverted instead of failing the whole schema, which is
// often a dump with defaults no invariant looks at.
func (c *pgConv) defaultExpr(n *pg.Node) sqlir.Expr {
	e, err := c.expr(n)
	if err != nil {
		return &sqlir.Unconverted{}
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

// columnLimits sets the length a varchar(n) or char(n) column holds and the
// precision and scale of a numeric(p, s), from the type's modifiers. A
// negative scale, which rounds to the left of the point, is not modeled.
func (c *pgConv) columnLimits(col *sqlir.ColumnDef, t *pg.TypeName) error {
	mods := typmods(t)
	switch col.Type {
	case "varchar":
		if len(mods) == 1 {
			col.MaxLen = mods[0]
		}
	case "bpchar":
		// char without a length is char(1).
		col.MaxLen = 1
		if len(mods) == 1 {
			col.MaxLen = mods[0]
		}
	case "numeric":
		if len(mods) >= 1 {
			col.Precision = mods[0]
		}
		if len(mods) == 2 {
			col.Scale = mods[1]
		}
		if col.Scale < 0 {
			return c.unsupported("a numeric column with a negative scale")
		}
	case "timestamp", "timestamptz":
		// Postgres keeps six fractional digits, also when more are declared.
		col.FSP = 6
		if len(mods) == 1 && mods[0] < 6 {
			col.FSP = mods[0]
		}
	case "interval":
		// The table loads so that a dump builds, and a write is refused.
		if len(t.GetTypmods()) > 0 {
			col.FSP = -1
		}
	}
	return nil
}

// typmods are the integers a type is declared with, as varchar(255) and
// numeric(10, 2) carry them.
func typmods(t *pg.TypeName) []int {
	var out []int
	for _, m := range t.GetTypmods() {
		k := m.GetAConst()
		if k == nil || k.GetIval() == nil {
			return nil
		}
		out = append(out, int(k.GetIval().Ival))
	}
	return out
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

// identity makes col an identity column, whose default is nextval of the
// sequence the constraint names, or of table_column_seq.
func (c *pgConv) identity(col *sqlir.ColumnDef, table string, k *pg.Constraint) error {
	seq, name, err := c.sequenceOptions(k.GetOptions())
	if err != nil {
		return err
	}
	col.Default = sequenceDefault(table, col.Name)
	if name != "" {
		col.Default = &sqlir.FuncCall{Name: "nextval", Args: []sqlir.Expr{&sqlir.Const{Value: name}}}
	}
	col.Sequence = seq
	col.Identity = identityMode(k.GetGeneratedWhen())
	return nil
}

// identityMode is the IR's name for the generated_when of an identity
// column: 'a' for ALWAYS, 'd' for BY DEFAULT.
func identityMode(when string) string {
	if when == "a" {
		return "always"
	}
	return "by default"
}

// sequenceOptions converts the options of a sequence that decide its values,
// and returns the name an identity column's SEQUENCE NAME gives it. AS, OWNED
// BY and CYCLE change nothing a run reaches.
func (c *pgConv) sequenceOptions(opts []*pg.Node) (*sqlir.SequenceOptions, string, error) {
	out := &sqlir.SequenceOptions{}
	var name string
	for _, n := range opts {
		d := n.GetDefElem()
		if d == nil {
			continue
		}
		var v *int64
		if a := d.Arg; a != nil {
			var n int64
			switch {
			case a.GetInteger() != nil:
				n = int64(a.GetInteger().Ival)
				v = &n
			case a.GetFloat() != nil:
				p, err := strconv.ParseInt(a.GetFloat().Fval, 10, 64)
				if err != nil {
					return nil, "", c.unsupported("sequence option " + d.Defname + " " + a.GetFloat().Fval)
				}
				v = &p
			}
		}
		switch d.Defname {
		case "start":
			out.Start = v
		case "increment":
			out.Increment = v
		case "minvalue":
			out.MinValue = v
		case "maxvalue":
			out.MaxValue = v
		case "cache":
			out.Cache = v
		case "restart":
			out.Restart, out.RestartStart = v, v == nil
		case "owned_by":
			var parts []string
			for _, p := range d.Arg.GetList().GetItems() {
				parts = append(parts, p.GetString_().GetSval())
			}
			switch {
			case len(parts) == 1 && strings.EqualFold(parts[0], "none"):
				out.OwnedNone = true
			case len(parts) >= 2:
				out.OwnedBy = [2]string{strings.Join(parts[:len(parts)-1], "."), parts[len(parts)-1]}
			}
		case "sequence_name":
			var parts []string
			for _, p := range d.Arg.GetList().GetItems() {
				parts = append(parts, p.GetString_().GetSval())
			}
			name = strings.Join(parts, ".")
		}
	}
	if out.Increment != nil && *out.Increment == 0 {
		return nil, "", c.unsupported("sequence INCREMENT 0")
	}
	return out, name, nil
}

// sequenceDefault is the default of a serial or identity column. Postgres
// names the sequence table_column_seq.
func sequenceDefault(table, column string) sqlir.Expr {
	// The sequence goes to the table's schema, which an unqualified name
	// leaves to the search path as it does the table's.
	return &sqlir.FuncCall{Name: "nextval", Args: []sqlir.Expr{&sqlir.Const{Value: table + "_" + column + "_seq"}}}
}

// constraintDef converts a PRIMARY KEY or UNIQUE constraint. cols are the
// columns of a constraint written on a column, which lists no keys itself.
func (c *pgConv) constraintDef(k *pg.Constraint, cols []string) (sqlir.UniqueDef, bool, error) {
	if k.Contype != pg.ConstrType_CONSTR_PRIMARY && k.Contype != pg.ConstrType_CONSTR_UNIQUE {
		return sqlir.UniqueDef{}, false, nil
	}
	u := sqlir.UniqueDef{Name: k.Conname, Primary: k.Contype == pg.ConstrType_CONSTR_PRIMARY, NullsNotDistinct: k.NullsNotDistinct, Deferrable: k.Deferrable || k.Initdeferred}
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
			chk, ok, err := c.check(cmd.Def.GetConstraint(), ch.Table)
			if err != nil {
				return nil, err
			}
			if ok {
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
			col := sqlir.ColumnDef{Name: cmd.Name}
			if err := c.identity(&col, ch.Table, cmd.Def.GetConstraint()); err != nil {
				return nil, err
			}
			ch.Columns = append(ch.Columns, col)
		case pg.AlterTableType_AT_SetIdentity:
			var opts []*pg.Node
			var mode string
			for _, n := range cmd.Def.GetList().GetItems() {
				if d := n.GetDefElem(); d.GetDefname() == "generated" {
					mode = identityMode(string(rune(d.GetArg().GetInteger().GetIval())))
					continue
				}
				opts = append(opts, n)
			}
			seq, _, err := c.sequenceOptions(opts)
			if err != nil {
				return nil, err
			}
			ch.Columns = append(ch.Columns, sqlir.ColumnDef{Name: cmd.Name, TypeOnly: true, Sequence: seq, Identity: mode})
		case pg.AlterTableType_AT_DropIdentity:
			// The default goes as with DROP DEFAULT, and the identity with it.
			mode := "drop"
			if cmd.MissingOk {
				mode = "drop if exists"
			}
			ch.Columns = append(ch.Columns, sqlir.ColumnDef{Name: cmd.Name, Identity: mode})
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
				col := sqlir.ColumnDef{Name: cmd.Name, Type: typeName(cd.TypeName), TypeOnly: true, Using: cd.RawDefault != nil, Collation: collationName(cd.CollClause)}
				if err := c.columnLimits(&col, cd.TypeName); err != nil {
					return nil, err
				}
				ch.Columns = append(ch.Columns, col)
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
	seen := map[string]bool{}
	for _, n := range w.Ctes {
		cte := n.GetCommonTableExpr()
		// Postgres fails the statement (42712); the CTEs are kept by name,
		// so the later one would stand in for both.
		if seen[cte.Ctename] {
			return nil, c.unsupported("a WITH query name given twice")
		}
		seen[cte.Ctename] = true
		if len(cte.Aliascolnames) > 0 {
			// The CTE's rows are keyed by the names its query gives them.
			return nil, c.unsupported("a column alias list on a CTE")
		}
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
			if len(cr.Fields) > 2 {
				return nil, c.unsupported("a schema-qualified *")
			}
			if len(cr.Fields) == 2 {
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
			if len(f.RangeVar.Alias.Colnames) > 0 {
				// The rows of a table are keyed by its column names and
				// locked by their identity, which renaming them would lose.
				return nil, nil, c.unsupported("a column alias list on a table, CTE or view")
			}
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
			t := sqlir.Target{Star: true}
			if len(cr.Fields) > 2 {
				return nil, c.unsupported("a schema-qualified *")
			}
			if len(cr.Fields) == 2 {
				t.Table = cr.Fields[0].GetString_().GetSval()
			}
			out = append(out, t)
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
	switch s.Override {
	case pg.OverridingKind_OVERRIDING_SYSTEM_VALUE:
		out.OverridingSystemValue = true
	case pg.OverridingKind_OVERRIDING_USER_VALUE:
		return nil, c.unsupported("OVERRIDING USER VALUE")
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
	if s.WithClause != nil {
		// WITH ... INSERT puts the CTEs on the INSERT, where the query reads
		// them as it would its own.
		if out.Select == nil {
			return nil, c.unsupported("CTE on INSERT ... VALUES")
		}
		with, err := c.with(s.WithClause)
		if err != nil {
			return nil, err
		}
		out.Select.With = append(with, out.Select.With...)
	}
	if oc := s.OnConflictClause; oc != nil {
		conf := &sqlir.OnConflict{}
		if oc.Infer != nil {
			conf.Constraint = oc.Infer.Conname
			exprs := false
			for _, n := range oc.Infer.IndexElems {
				ie := n.GetIndexElem()
				if len(ie.Opclass) > 0 || len(ie.Collation) > 0 {
					// Postgres picks arbiters by them, which detest's
					// indexes do not keep.
					return nil, c.unsupported("an operator class or collation in ON CONFLICT's target")
				}
				if ie.Expr == nil {
					conf.Columns = append(conf.Columns, ie.Name)
					conf.Elems = append(conf.Elems, &sqlir.ColumnRef{Column: ie.Name})
					continue
				}
				e, err := c.expr(ie.Expr)
				if err != nil {
					return nil, err
				}
				exprs = true
				conf.Elems = append(conf.Elems, e)
			}
			if exprs {
				conf.Columns = nil
			}
			if oc.Infer.WhereClause != nil {
				w, err := c.expr(oc.Infer.WhereClause)
				if err != nil {
					return nil, err
				}
				conf.InferWhere = w
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
	with, err := c.with(s.WithClause)
	if err != nil {
		return nil, err
	}
	out := &sqlir.UpdateStmt{With: with, Table: rangeVarName(s.Relation)}
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
	with, err := c.with(s.WithClause)
	if err != nil {
		return nil, err
	}
	out := &sqlir.DeleteStmt{With: with, Table: rangeVarName(s.Relation)}
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
	case *pg.Node_CollateClause:
		x, err := c.expr(e.CollateClause.Arg)
		if err != nil {
			return nil, err
		}
		return &sqlir.Collate{X: x, Name: collationName(e.CollateClause)}, nil
	case *pg.Node_TypeCast:
		if len(e.TypeCast.TypeName.GetArrayBounds()) > 0 {
			// Dropping array bounds would run scalar casts and operators
			// instead of PostgreSQL's array overloads. ANY handles its own
			// supported array casts before reaching this conversion.
			return nil, c.unsupported("an array cast")
		}
		x, err := c.expr(e.TypeCast.Arg)
		if err != nil {
			return nil, err
		}
		names := e.TypeCast.TypeName.GetNames()
		typ := strings.ToLower(names[len(names)-1].GetString_().GetSval())
		switch typ {
		case "interval", "timestamp", "timestamptz":
			// interval '1.5 months' month drops the days and ::timestamp(0)
			// rounds the seconds, which the cast without them does not.
			if len(e.TypeCast.TypeName.GetTypmods()) > 0 {
				return nil, c.unsupported("a cast to " + typ + " with a precision or fields")
			}
		}
		cast := &sqlir.Cast{X: x, Type: typ}
		if len(e.TypeCast.TypeName.GetTypmods()) > 0 {
			switch typ {
			case "bpchar":
				// char(n), and char alone, which is char(1), pads text with
				// spaces it then compares without, which detest's strings
				// do not follow. A bare ::bpchar, as dumps write defaults,
				// has no length and stays text.
				return nil, c.unsupported("a cast to char(n)")
			case "varchar":
				mods := typmods(e.TypeCast.TypeName)
				if len(mods) != 1 || mods[0] < 1 {
					return nil, c.unsupported("a cast to varchar with a length other than a positive integer")
				}
				cast.Len = mods[0]
			}
		}
		return cast, nil
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
			// IN (subquery) comes with no operator and = ANY (subquery)
			// with =, and both are IN. NOT IN arrives as a NOT around one.
			// Another operator compares with each row, <> ANY meaning
			// "differs from one", which IN cannot express.
			if n := e.SubLink.OperName; len(n) > 0 && (len(n) > 1 || n[0].GetString_().GetSval() != "=") {
				return nil, c.unsupported("an operator other than = with ANY (subquery)")
			}
			x, err := c.expr(e.SubLink.Testexpr)
			if err != nil {
				return nil, err
			}
			return &sqlir.InExpr{X: x, Sub: sel}, nil
		}
		return nil, c.unsupported("subquery kind")
	case *pg.Node_FuncCall:
		fc := e.FuncCall
		switch {
		case fc.AggFilter != nil:
			return nil, c.unsupported("aggregate FILTER")
		case len(fc.AggOrder) > 0:
			return nil, c.unsupported("aggregate ORDER BY")
		case fc.AggWithinGroup:
			return nil, c.unsupported("aggregate WITHIN GROUP")
		}
		fname := ""
		if len(fc.Funcname) > 0 {
			fname = strings.ToLower(fc.Funcname[len(fc.Funcname)-1].GetString_().GetSval())
		}
		if len(fc.Funcname) > 2 || len(fc.Funcname) == 2 && fc.Funcname[0].GetString_().GetSval() != "pg_catalog" {
			// A function of another schema is the user's, which detest
			// would otherwise take for the built-in of the same name.
			return nil, c.unsupported("function " + fc.Funcname[0].GetString_().GetSval() + "." + fc.Funcname[len(fc.Funcname)-1].GetString_().GetSval())
		}
		if fname == "hashtext" && !c.lockKey {
			// The executor returns hashtext's input, which keys an advisory
			// lock as the int4 hash would, equal for equal strings, but is
			// not the value Postgres gives anywhere the application reads
			// or compares it. Two keys whose 32-bit hashes collide on the
			// server, which detest tells apart, are not modeled: among
			// the keys an application holds the chance is 2^-32 a pair,
			// and modeling it needs the server's hash function.
			return nil, c.unsupported("hashtext anywhere but as the key of an advisory lock")
		}
		if fname == "hashtext" {
			// The allowance is for this call alone: a hashtext inside its
			// argument would be read as the string on the way to the key.
			c.lockKey = false
		}
		if fname == "pg_advisory_xact_lock" || fname == "pg_try_advisory_xact_lock" {
			// hashtext may stand only as a key itself, not inside an
			// expression that reads its value on the way to the key.
			args := make([]sqlir.Expr, 0, len(fc.Args))
			for _, a := range fc.Args {
				inner := a.GetFuncCall()
				c.lockKey = inner != nil && len(inner.Funcname) > 0 && strings.EqualFold(inner.Funcname[len(inner.Funcname)-1].GetString_().GetSval(), "hashtext")
				e, err := c.expr(a)
				c.lockKey = false
				if err != nil {
					return nil, err
				}
				args = append(args, e)
			}
			if fc.AggDistinct {
				return nil, c.unsupported("DISTINCT in a call to " + fname)
			}
			call := &sqlir.FuncCall{Name: fname, Args: args, Star: fc.AggStar}
			if fc.Over != nil {
				return c.window(call, fc.Over)
			}
			return call, nil
		}
		if fname == "make_interval" {
			// The executor takes make_interval's one argument as seconds,
			// which is the named form backoff SQL writes. Postgres reads a
			// positional first argument as years and has six other names,
			// which the executor would also take for seconds.
			if len(fc.Args) != 1 || fc.Args[0].GetNamedArgExpr().GetName() != "secs" {
				return nil, c.unsupported("make_interval with an argument other than secs => n")
			}
		}
		args, err := c.exprs(fc.Args)
		if err != nil {
			return nil, err
		}
		call := &sqlir.FuncCall{Name: strings.ToLower(fc.Funcname[len(fc.Funcname)-1].GetString_().GetSval()), Args: args, Star: fc.AggStar, Distinct: fc.AggDistinct}
		if sqlir.OtherAggregates[call.Name] {
			// Refused here, before a WITH query or anything else runs.
			return nil, c.unsupported("aggregate " + call.Name)
		}
		switch call.Name {
		case "count":
			// count(*) or count(x): no other signature.
			if call.Star != (len(call.Args) == 0) || len(call.Args) > 1 {
				return nil, c.unsupported("aggregate count with these arguments")
			}
		case "sum", "min", "max", "avg":
			if call.Star || len(call.Args) != 1 {
				return nil, c.unsupported("aggregate " + call.Name + " with these arguments")
			}
		default:
			if call.Distinct {
				// Postgres takes DISTINCT only in an aggregate's arguments.
				return nil, c.unsupported("DISTINCT in a call to " + call.Name)
			}
		}
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
		case pg.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP:
			return &sqlir.FuncCall{Name: "now"}, nil
		case pg.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP:
			// LOCALTIMESTAMP is a timestamp, now()'s clock in the session's
			// TimeZone. It cannot be written as a call, so the name is free.
			return &sqlir.FuncCall{Name: "localtimestamp"}, nil
		case pg.SQLValueFunctionOp_SVFOP_CURRENT_DATE:
			// CURRENT_DATE cannot be written as a call, so the name is free.
			return &sqlir.FuncCall{Name: "current_date"}, nil
		case pg.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP_N, pg.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP_N:
			// current_timestamp with an argument cannot be written as a call,
			// so it is free to carry the precision to round to.
			name := "current_timestamp"
			if e.SqlvalueFunction.Op == pg.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP_N {
				name = "localtimestamp"
			}
			return &sqlir.FuncCall{Name: name, Args: []sqlir.Expr{&sqlir.Const{Value: int64(e.SqlvalueFunction.Typmod)}}}, nil
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

// textElements reports whether Postgres types an array of these elements as
// text: each is a string literal, NULL, a parameter or a cast to text or
// varchar, and one at least is a string or such a cast, or every one is NULL.
// A number among them would make the array a number's instead.
func textElements(elements []*pg.Node) bool {
	str, nulls := false, len(elements) > 0
	for _, el := range elements {
		if tc := el.GetTypeCast(); tc != nil {
			if _, ok := plainTextCast(tc, false, "text", "varchar"); !ok {
				return false
			}
			str, nulls = true, false
			continue
		}
		if el.GetParamRef() != nil {
			nulls = false
			continue
		}
		k := el.GetAConst()
		switch {
		case k == nil:
			return false
		case k.GetSval() != nil:
			str, nulls = true, false
		case !k.Isnull:
			return false
		}
	}
	return str || nulls
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
	// Only the built-in type: unqualified or in pg_catalog, and as the
	// parser gives it, not a quoted "TEXT" or a type of another schema.
	names := tc.TypeName.GetNames()
	if len(names) == 2 && names[0].GetString_().GetSval() != "pg_catalog" || len(names) > 2 {
		return "", false
	}
	typ := names[len(names)-1].GetString_().GetSval()
	return typ, slices.Contains(types, typ)
}

// arrayElemTypes are the element types of the array casts that
// x = ANY ($1::T[]) runs with. The cast converts each element as the scalar
// cast does, which detest's casts do for these types; for others, such as
// bpchar or numeric, it pads or keeps digits as detest's casts do not.
var arrayElemTypes = []string{"int2", "int4", "int8", "text", "varchar", "uuid"}

// arrayCmp converts x = ANY (a) and x <> ALL (a) over an array that is a
// value, a parameter or a string literal, cast to an array type or not. ok
// is false for any other array, which the caller converts as ARRAY[...] or
// refuses.
func (c *pgConv) arrayCmp(e *pg.A_Expr, isAny bool) (sqlir.Expr, bool, error) {
	arr, elemType := e.Rexpr, ""
	if tc := arr.GetTypeCast(); tc != nil {
		arr = tc.Arg
		if !arrayValue(arr) {
			return nil, false, nil
		}
		if len(tc.TypeName.GetArrayBounds()) == 0 {
			return nil, true, c.unsupported("ANY or ALL over a value cast to a type other than an array")
		}
		typ, ok := plainTextCast(tc, true, arrayElemTypes...)
		if !ok {
			return nil, true, c.unsupported("ANY or ALL over an array cast to " + typ + "[]")
		}
		elemType = typ
	}
	if !arrayValue(arr) {
		return nil, false, nil
	}
	a, err := c.expr(arr)
	if err != nil {
		return nil, true, err
	}
	l, err := c.expr(e.Lexpr)
	if err != nil {
		return nil, true, err
	}
	// Postgres takes the array's element type from x, so with neither typed,
	// as in $1 = ANY ($2), it resolves both to text or fails, by rules detest
	// does not model.
	if elemType == "" && untypedOperand(l) {
		return nil, true, c.unsupported("ANY or ALL with neither side typed")
	}
	return &sqlir.ArrayCmp{X: l, Array: a, ElemType: elemType, All: !isAny}, true, nil
}

// arrayValue reports whether n is an array given as a value: a parameter or
// a string literal.
func arrayValue(n *pg.Node) bool {
	return n.GetParamRef() != nil || n.GetAConst() != nil && n.GetAConst().GetSval() != nil
}

func untypedOperand(e sqlir.Expr) bool {
	switch e := e.(type) {
	case *sqlir.Param:
		return true
	case *sqlir.Const:
		_, ok := e.Value.(string)
		return ok
	}
	return false
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
		case "<=>":
			// MySQL's null-safe equality, which the executor evaluates for
			// its converter. Postgres has no such operator and fails the
			// statement with 42883.
			return nil, c.unsupported("operator <=>")
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
		if cmp, ok, err := c.arrayCmp(e, isAny); ok {
			return cmp, err
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
		// An array of string literals is a text[] in Postgres, not literals
		// each resolved against x, so id = ANY (ARRAY['1']) compares an
		// integer with text, which Postgres refuses.
		if len(casts) == 0 && textElements(arr.GetAArrayExpr().Elements) {
			casts = []string{"text"}
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
