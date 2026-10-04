// Package sqlir is the SQL intermediate representation detest's executor
// runs, shared with the parsers of the database servers detest models: the
// postgres package converts pg_query's AST into it, and a MySQL parser can
// convert vitess's AST into the same shapes. It also holds what describes a
// server to detest, so those packages need not import detest itself.
package sqlir

import (
	"errors"
	"fmt"
	"slices"
)

// Parser parses a query string of one engine's SQL into detest's statement IR.
type Parser interface {
	// Name identifies the SQL dialect in error messages and the parse cache.
	Name() string
	// Parse parses exactly one statement, or a script of schema statements
	// (a schema dump or a migration) into one SchemaStmt. Placeholders ($1 or
	// ?) become Param nodes numbered from 0 in argument order.
	Parse(query string) (Statement, error)
}

// Statement is one parsed SQL statement.
type Statement interface{ isStatement() }

// SelectStmt is a query, possibly with CTEs, joins and aggregation.
type SelectStmt struct {
	With       []CTE
	Distinct   bool
	DistinctOn []Expr
	// A set operation combines Larg and Rarg; Targets through Having are then
	// empty and OrderBy, Limit and Offset apply to the combined rows.
	SetOp      string // "union", "intersect" or "except"
	SetAll     bool
	Larg, Rarg *SelectStmt
	// Values is a VALUES list used as a query; its columns are column1, ...
	Values  [][]Expr
	Targets []Target
	From    *TableRef // nil for SELECT without FROM
	Joins   []Join
	Where   Expr
	GroupBy []Expr
	Having  Expr
	OrderBy []OrderKey
	Limit   Expr
	Offset  Expr
	Lock    *LockClause
}

// CTE is a common table expression.
type CTE struct {
	Name   string
	Select *SelectStmt
}

// TableRef is a base table, a CTE reference or a derived table.
type TableRef struct {
	Name  string
	Alias string
	Sub   *SelectStmt
	// Func is a set-returning function in FROM, such as generate_series;
	// Columns are the column names the alias gives it.
	Func       *FuncCall
	Columns    []string
	Ordinality bool
	// Lateral evaluates the item once per row of the items before it, which
	// it may refer to. A function in FROM is always lateral.
	Lateral bool
}

// JoinKind distinguishes inner and outer joins.
type JoinKind int

const (
	InnerJoin JoinKind = iota
	LeftJoin
	RightJoin
	FullJoin
	CrossJoin
)

// Join is a joined table with its condition.
type Join struct {
	Kind  JoinKind
	Table TableRef
	On    Expr
}

// Target is one SELECT list item.
type Target struct {
	Expr  Expr
	Alias string
	Star  bool   // SELECT * or t.*
	Table string // qualifier of t.*
}

// OrderKey is one ORDER BY item.
type OrderKey struct {
	Expr Expr
	Desc bool
	// Nulls is NullsFirst or NullsLast, or 0 for the default: last when
	// ascending, first when descending.
	Nulls int
}

const (
	NullsFirst = 1
	NullsLast  = 2
)

// WindowFunc is a function computed over the rows of a window: Func over the
// rows sharing Partition, ordered by Order. Whole means the frame is the whole
// partition (ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING);
// otherwise it is the default, from the partition's start to the current
// row's last peer.
type WindowFunc struct {
	Func      *FuncCall
	Partition []Expr
	Order     []OrderKey
	Whole     bool
}

// LockClause is FOR UPDATE / FOR SHARE with its wait policy. Of lists the
// tables FOR UPDATE OF names, by alias or name; empty locks every table.
type LockClause struct {
	Strength   string // "update", "no key update", "share" or "key share"
	SkipLocked bool
	NoWait     bool
	// Of names the FROM items (by alias, or table name when unaliased) whose
	// rows are locked. Empty locks the rows of every table in FROM.
	Of []string
}

// InsertStmt is INSERT ... VALUES or INSERT ... SELECT.
type InsertStmt struct {
	Table      string
	Alias      string
	Columns    []string
	Rows       [][]Expr
	Select     *SelectStmt
	OnConflict *OnConflict
	Returning  []Target
	// OverridingSystemValue is OVERRIDING SYSTEM VALUE, which lets an
	// INSERT give a GENERATED ALWAYS identity column a value.
	OverridingSystemValue bool
}

// OnConflict is ON CONFLICT (cols) DO NOTHING | DO UPDATE SET ... WHERE ...
// (MySQL's INSERT IGNORE and ON DUPLICATE KEY UPDATE map here too).
type OnConflict struct {
	Columns []string
	// Elems are Postgres's inference elements, the columns and expressions
	// of the unique index to arbitrate on, when one of them is not a plain
	// column; Where is the predicate that infers a partial one, and
	// Constraint names the constraint of ON CONFLICT ON CONSTRAINT.
	Elems      []Expr
	InferWhere Expr
	Constraint string
	DoNothing  bool
	Set        []Assignment
	Where      Expr
}

// UpdateStmt is UPDATE ... SET ... [FROM ...] WHERE ... [RETURNING ...].
type UpdateStmt struct {
	Table     string
	Alias     string
	Set       []Assignment
	From      []TableRef
	Where     Expr
	Returning []Target
	// OrderBy and Limit are MySQL's UPDATE ... ORDER BY ... LIMIT: the rows
	// updated are the first Limit of the matching rows in that order.
	OrderBy []OrderKey
	Limit   Expr
}

// DeleteStmt is DELETE FROM ... [USING ...] WHERE ... [RETURNING ...].
type DeleteStmt struct {
	Table     string
	Alias     string
	Using     []TableRef
	Where     Expr
	Returning []Target
	OrderBy   []OrderKey // with Limit, as in UpdateStmt
	Limit     Expr
	// Truncate is MySQL's TRUNCATE, which also starts AUTO_INCREMENT over.
	Truncate bool
}

// SchemaStmt declares table structure: the tables, their primary keys,
// unique constraints and column defaults. A schema statement detest does not
// need (a function, a sequence, a grant, a comment, a session setting) adds
// nothing. Table names are as written, possibly schema-qualified.
type SchemaStmt struct {
	Changes []SchemaChange
}

// SchemaChange is what one statement declares about one table.
type SchemaChange struct {
	Table       string
	Create      bool // CREATE TABLE
	Drop        bool // DROP TABLE
	IfNotExists bool
	IfExists    bool
	Columns     []ColumnDef
	Constraints []UniqueDef
	ForeignKeys []ForeignKey
	Checks      []CheckDef
	// Indexes are the indexes that are not unique. Nothing checks them, but
	// InnoDB locks gaps along the index a statement searches by.
	Indexes []IndexDef
	// Object is what Drop and the renames act on besides a table: "view",
	// "matview" or "index" (Table then names the index).
	Object          string
	DropColumns     []string
	DropConstraints []string // by name, whatever kind of constraint or index it is
	// DropIndexes, DropForeignKeys and DropChecks drop only that kind, as
	// MySQL's DROP INDEX, DROP FOREIGN KEY and DROP CHECK do: a foreign key
	// and the index on its columns may share a name.
	DropIndexes      []string
	DropForeignKeys  []string
	DropChecks       []string
	RenameTo         string    // ALTER TABLE ... RENAME TO, qualified when the statement qualifies it
	RenameColumn     [2]string // old and new name
	RenameConstraint [2]string // old and new name of a constraint or index
	RenameIndex      [2]string // old and new name of an index only, as MySQL's RENAME INDEX
	// Collation is a MySQL table's default collation, for the text columns
	// that declare none; ConvertCollation applies it to the table's text
	// columns too, as CONVERT TO CHARACTER SET does.
	Collation        string
	ConvertCollation bool
	// AutoIncrement is MySQL's AUTO_INCREMENT=n table option, the next
	// value the table's AUTO_INCREMENT column generates at least.
	AutoIncrement int64
	// View is the query of CREATE [OR REPLACE] VIEW, with the column names
	// the view gives it.
	View        *SelectStmt
	ViewColumns []string
	Replace     bool
	// Sequence is CREATE SEQUENCE (Create) or ALTER SEQUENCE of the
	// sequence Table names, with Object "sequence".
	Sequence *SequenceOptions
}

// SequenceOptions are the options of a sequence that decide the values
// nextval returns, nil when not given.
type SequenceOptions struct {
	Start, Increment, MinValue, MaxValue, Cache *int64
	// Restart is RESTART WITH n, and RestartStart a RESTART without a
	// value, which starts over at the start value.
	Restart      *int64
	RestartStart bool
}

// ColumnDef is a column with its default, if any. A serial or identity column
// defaults to nextval of a sequence named after it.
type ColumnDef struct {
	Name    string
	Default Expr
	// Type is the column's type name as Postgres normalizes it ("int4",
	// "uuid", "text"), or empty when the statement does not change it.
	Type string
	// TypeOnly changes the type and leaves the default (ALTER COLUMN TYPE).
	TypeOnly bool
	// NotNull adds NOT NULL (in CREATE TABLE, ADD COLUMN or SET NOT NULL);
	// DropNotNull removes it.
	NotNull     bool
	DropNotNull bool
	// Generated is the expression of a generated column, GENERATED ALWAYS
	// AS (expr), whose value is computed from the row on every write.
	Generated Expr
	// Sequence are the options of an identity column's sequence, the one
	// its Default calls nextval of. Identity is "always" for GENERATED
	// ALWAYS, which takes no value but DEFAULT, "by default" for GENERATED
	// BY DEFAULT, "drop" for DROP IDENTITY and empty to leave it as it is.
	Sequence *SequenceOptions
	Identity string
	// AutoIncrement is MySQL's AUTO_INCREMENT: an insert that leaves the
	// column NULL or 0 takes the next value, and an explicit larger value
	// moves the counter past it. DropAutoIncrement removes it, as a MODIFY
	// that does not say it again does.
	AutoIncrement     bool
	DropAutoIncrement bool
	// First and After place the column, as MySQL's ADD, MODIFY and CHANGE
	// may: first, or after the column named.
	First bool
	After string
	// OnUpdate is MySQL's ON UPDATE CURRENT_TIMESTAMP: an update that
	// changes the row and does not set the column sets it to this.
	OnUpdate Expr
	// MaxLen is the most characters a CHAR(n) or VARCHAR(n) column holds,
	// 0 for no limit detest checks. Members are a MySQL ENUM's or SET's
	// values, Set telling the two apart.
	MaxLen  int
	Members []string
	Set     bool
	// Precision and Scale are a Postgres NUMERIC(p, s) column's, which
	// rounds what it stores to s places; 0 for an unconstrained numeric.
	Precision int
	Scale     int
	// FSP is the fractional seconds a MySQL DATETIME or TIMESTAMP column
	// keeps, 0 without one declared.
	FSP int
	// Collation is a MySQL text column's collation as declared, or the
	// default one of the character set it declares; empty for the table's.
	Collation string
}

// IndexDef is an index that is not unique, by the columns it is on.
type IndexDef struct {
	Name    string
	Columns []string
	// Prefix is the column whose first characters the part after Columns
	// holds, as name(3) does.
	Prefix string
	// Desc is an index with a descending part, which orders its keys
	// otherwise than detest's gap locks do.
	Desc bool
}

// CheckDef is a CHECK constraint. Name is empty when the statement gives
// none.
type CheckDef struct {
	Name string
	Expr Expr
}

// UniqueDef is a primary key, a unique constraint or a unique index. Elems are
// column references or, for an expression index, expressions over the row;
// Where makes it a partial index.
type UniqueDef struct {
	Name             string
	Primary          bool
	Elems            []Expr
	Where            Expr
	NullsNotDistinct bool
	// Deferrable is a constraint DEFERRABLE, whose check Postgres runs at
	// the end of the statement or at commit.
	Deferrable bool
	// Index is a unique index made by CREATE UNIQUE INDEX, which is no
	// constraint ON CONFLICT ON CONSTRAINT may name.
	Index bool
}

// ForeignKey is a FOREIGN KEY or REFERENCES constraint. RefColumns empty
// means the referenced table's primary key. OnDelete and OnUpdate are
// "no action", "restrict", "cascade", "set null" or "set default".
type ForeignKey struct {
	Name       string
	Columns    []string
	RefTable   string
	RefColumns []string
	OnDelete   string
	OnUpdate   string
	Deferrable bool // DEFERRABLE: SET CONSTRAINTS may defer its checks to commit
	Deferred   bool // INITIALLY DEFERRED: checked at commit unless SET CONSTRAINTS says otherwise
	MatchFull  bool // MATCH FULL: a key with some but not all columns NULL is a violation
}

// Assignment is col = expr in SET.
type Assignment struct {
	Column string
	Value  Expr
}

// Script is several statements run in order, as a migration file runs. A
// failing statement stops the script; the statements before it stay applied
// unless an enclosing transaction rolls them back.
type Script struct {
	Stmts []Statement
}

// CreateTableAsStmt is CREATE TABLE ... AS SELECT, or CREATE MATERIALIZED
// VIEW, which keeps the query for REFRESH.
type CreateTableAsStmt struct {
	Table        string
	Columns      []string // the column names given, if any
	Select       *SelectStmt
	NoData       bool
	IfNotExists  bool
	Materialized bool
}

// SavepointStmt is SAVEPOINT, RELEASE SAVEPOINT or ROLLBACK TO SAVEPOINT.
type SavepointStmt struct {
	Op   string // "savepoint", "release" or "rollback_to"
	Name string
}

// SetStmt is SET [LOCAL] name = value. detest acts on lock_timeout, and on
// database, which MySQL's USE sets; other settings do nothing.
type SetStmt struct {
	Name  string
	Value string
	Local bool
}

// SetConstraintsStmt is SET CONSTRAINTS ... DEFERRED | IMMEDIATE. No names
// means ALL.
type SetConstraintsStmt struct {
	Names    []string
	Deferred bool
}

// RefreshStmt is REFRESH MATERIALIZED VIEW.
type RefreshStmt struct {
	Table  string
	NoData bool
}

func (*Script) isStatement()             {}
func (*RefreshStmt) isStatement()        {}
func (*SavepointStmt) isStatement()      {}
func (*SetStmt) isStatement()            {}
func (*SetConstraintsStmt) isStatement() {}
func (*CreateTableAsStmt) isStatement()  {}
func (*SelectStmt) isStatement()         {}
func (*SchemaStmt) isStatement()         {}
func (*InsertStmt) isStatement()         {}
func (*UpdateStmt) isStatement()         {}
func (*DeleteStmt) isStatement()         {}

// Expr is an expression node.
type Expr interface{ isExpr() }

// ColumnRef is [table.]column. Table "excluded" refers to the proposed row in
// ON CONFLICT DO UPDATE.
type ColumnRef struct{ Table, Column string }

// Param is a positional parameter, 0-based.
type Param struct{ Index int }

// Const is a literal: int64, float64, string, bool or nil.
type Const struct{ Value any }

// BinaryExpr is a binary operator: comparison, arithmetic, AND, OR, LIKE.
type BinaryExpr struct {
	Op   string // = <> < <= > >= + - * / % AND OR LIKE ILIKE NOT LIKE
	L, R Expr
}

// UnaryExpr is NOT or unary minus.
type UnaryExpr struct {
	Op string
	X  Expr
}

// InExpr is x [NOT] IN (list | subquery).
type InExpr struct {
	X    Expr
	List []Expr
	Sub  *SelectStmt
	Not  bool
}

// IsNull is x IS [NOT] NULL.
type IsNull struct {
	X   Expr
	Not bool
}

// FuncCall is a function or aggregate call.
type FuncCall struct {
	Name     string // lower case
	Args     []Expr
	Star     bool // count(*)
	Distinct bool
}

// SubQuery is a scalar subquery.
type SubQuery struct{ Select *SelectStmt }

// Exists is [NOT] EXISTS (subquery).
type Exists struct {
	Select *SelectStmt
	Not    bool
}

// RowExpr is (a, b, c).
type RowExpr struct{ Items []Expr }

// Cast is x::type or CAST(x AS type). Type is lower case.
type Cast struct {
	X    Expr
	Type string
}

// CaseExpr is CASE [x] WHEN ... THEN ... ELSE ... END.
type CaseExpr struct {
	Arg   Expr
	Whens []CaseWhen
	Else  Expr
}

// CaseWhen is one WHEN/THEN pair.
type CaseWhen struct{ When, Then Expr }

// Default is the DEFAULT keyword in VALUES or SET.
type Default struct{}

// Unconverted stands for a schema expression the dialect could not convert.
// The schema loads with it, and evaluating it fails as an expression detest
// cannot evaluate. It is a node of its own so that no literal, such as the
// Unknown string, can be taken for it.
type Unconverted struct{}

func (*ColumnRef) isExpr()   {}
func (*Param) isExpr()       {}
func (*Const) isExpr()       {}
func (*Unconverted) isExpr() {}
func (*BinaryExpr) isExpr()  {}
func (*UnaryExpr) isExpr()   {}
func (*InExpr) isExpr()      {}
func (*IsNull) isExpr()      {}
func (*FuncCall) isExpr()    {}
func (*SubQuery) isExpr()    {}
func (*Exists) isExpr()      {}
func (*RowExpr) isExpr()     {}
func (*Cast) isExpr()        {}
func (*CaseExpr) isExpr()    {}
func (*Default) isExpr()     {}
func (*WindowFunc) isExpr()  {}

// ParseError is SQL the dialect's grammar rejects, which the server reports as
// a syntax error.
type ParseError struct {
	Query string
	Err   error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("detest: cannot parse SQL %q: %v", e.Query, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// ErrUnsupportedSQL marks a statement the dialect parsed but detest cannot run.
type ErrUnsupportedSQL struct {
	What  string
	Query string
}

func (e *ErrUnsupportedSQL) Error() string {
	return fmt.Sprintf("detest: unsupported SQL (%s): %s", e.What, e.Query)
}

// Unsupported reports a statement or expression detest cannot run.
func Unsupported(what, query string) error { return &ErrUnsupportedSQL{What: what, Query: query} }

// Unknown is the value of an expression detest cannot evaluate.
const Unknown = "<unknown expression>"

// IsolationLevel is a transaction isolation level.
type IsolationLevel int

const (
	ReadCommitted IsolationLevel = iota + 1
	RepeatableRead
	Serializable
)

func (l IsolationLevel) String() string {
	switch l {
	case ReadCommitted:
		return "Read Committed"
	case RepeatableRead:
		return "Repeatable Read"
	case Serializable:
		return "Serializable"
	}
	return fmt.Sprintf("IsolationLevel(%d)", int(l))
}

// Impl describes a kind of database server to detest: the SQL it parses,
// the level its transactions run at unless they ask for another, the levels
// detest implements for it, its search_path, and its error codes. The
// concurrency semantics of a level differ between kinds, so detest implements
// each kind and level pair on its own.
type Impl struct {
	name       string
	parser     Parser
	isolation  IsolationLevel
	supported  []IsolationLevel
	searchPath []string
	codes      func(DBErrorKind) (sqlstate string, number int)
	convert    func(*DBError) error
	innodb     bool
	collation  string
}

// ServerSpec is what the package of a kind, such as postgres.New, tells
// detest about the server.
type ServerSpec struct {
	Name       string
	Parser     Parser
	Isolation  IsolationLevel
	Supported  []IsolationLevel
	SearchPath []string
	// Codes returns the server's SQLSTATE and, for MySQL, its error number
	// for a kind of error.
	Codes func(DBErrorKind) (sqlstate string, number int)
	// Convert turns a database error into the error the driver of the
	// production code returns, such as *pgconn.PgError. nil returns the
	// DBError itself.
	Convert func(*DBError) error
	// InnoDB selects InnoDB's semantics over PostgreSQL's: shared and
	// exclusive row locks only, gap locks at Repeatable Read, reads from a
	// snapshot taken at the transaction's first read, and a failed statement
	// rolling back only itself.
	InnoDB bool
	// Collation is MySQL's server default collation, which a text column
	// that neither it nor its table declares one for takes.
	Collation string
}

// Server is a kind of database server as the code using detest holds it,
// such as postgres.New() returns: a handle with nothing to call on it. detest
// reaches the description through ImplOf.
type Server struct{ impl *Impl }

// NewServer describes a server.
func NewServer(spec ServerSpec) Server {
	return Server{impl: &Impl{name: spec.Name, parser: spec.Parser, isolation: spec.Isolation, supported: spec.Supported,
		searchPath: spec.SearchPath, codes: spec.Codes, convert: spec.Convert, innodb: spec.InnoDB, collation: spec.Collation}}
}

// ImplOf returns the description behind s, nil for the zero Server.
func ImplOf(s Server) *Impl { return s.impl }

// Error returns a database error of kind with the server's codes.
func (s *Impl) Error(kind DBErrorKind, message, table, column, constraint string) *DBError {
	e := &DBError{Kind: kind, Message: message, Table: table, Column: column, Constraint: constraint}
	if s.codes != nil {
		e.Code, e.Number = s.codes(kind)
	}
	return e
}

// Convert returns err as the driver of the production code would: an
// DBError in it goes through the server's Convert.
func (s *Impl) Convert(err error) error {
	var se *DBError
	if s.convert == nil || !errors.As(err, &se) {
		return err
	}
	return s.convert(se)
}

// DBErrorKind is a class of database error the code under test may branch on.
type DBErrorKind int

const (
	UniqueViolation DBErrorKind = iota + 1
	NotNullViolation
	Deadlock
	InFailedTransaction
	LockNotAvailable
	UndefinedTable
	ForeignKeyViolation
	DivisionByZero
	NumericValueOutOfRange
	InvalidTextRepresentation
	SyntaxError
	UndefinedParameter
	InvalidColumnReference
	DuplicateTable
	WrongObjectType
	InvalidTableDefinition
	NoActiveTransaction
	InvalidSavepoint
	CheckViolation
	InvalidParameterValue
	// LockWaitTimeout is a lock wait that ran out its lock timeout, which
	// MySQL reports apart from NOWAIT's LockNotAvailable.
	LockWaitTimeout
	// InvalidRowCountInLimit and InvalidRowCountInOffset are a LIMIT or an
	// OFFSET that is not a count, which Postgres tells apart.
	InvalidRowCountInLimit
	InvalidRowCountInOffset
	// ArithmeticOutOfRange is an expression whose integer result overflows,
	// which MySQL reports apart from a value out of a column's range.
	ArithmeticOutOfRange
	// ForeignKeyParentViolation is an update or delete of a parent row that
	// children still reference, which MySQL reports apart from a child row
	// without its parent.
	ForeignKeyParentViolation
	// CardinalityViolation is a scalar subquery that returns more than one
	// row.
	CardinalityViolation
	// StringDataRightTruncation is a string longer than its column allows.
	StringDataRightTruncation
	// DataTruncated is a value that is none of an ENUM's or a SET's members,
	// which MySQL's strict mode refuses.
	DataTruncated
	// RestrictViolation is an update or delete of a parent row that children
	// reference through an ON DELETE or ON UPDATE RESTRICT foreign key, which
	// Postgres reports apart from NO ACTION.
	RestrictViolation
	// UndefinedColumn is a column reference no table in scope has.
	UndefinedColumn
	// AmbiguousColumn is an unqualified column reference more than one
	// table in scope has.
	AmbiguousColumn
)

// The errors a DBError of each kind matches with errors.Is.
var (
	ErrUniqueViolation           = errors.New("detest: unique violation")
	ErrNotNullViolation          = errors.New("detest: not-null violation")
	ErrDeadlock                  = errors.New("detest: deadlock detected, transaction aborted")
	ErrInFailedTx                = errors.New("detest: transaction already aborted")
	ErrLockNotAvailable          = errors.New("detest: lock not available")
	ErrUndefinedTable            = errors.New("detest: relation does not exist")
	ErrForeignKeyViolation       = errors.New("detest: foreign key violation")
	ErrDivisionByZero            = errors.New("detest: division by zero")
	ErrNumericValueOutOfRange    = errors.New("detest: numeric value out of range")
	ErrInvalidTextRepresentation = errors.New("detest: invalid input syntax")
	ErrSyntaxError               = errors.New("detest: syntax error")
	ErrUndefinedParameter        = errors.New("detest: undefined parameter")
	ErrInvalidColumnReference    = errors.New("detest: invalid column reference")
	ErrDuplicateTable            = errors.New("detest: relation already exists")
	ErrWrongObjectType           = errors.New("detest: wrong object type")
	ErrInvalidTableDefinition    = errors.New("detest: invalid table definition")
	ErrNoActiveTransaction       = errors.New("detest: no active transaction")
	ErrInvalidSavepoint          = errors.New("detest: invalid savepoint")
	ErrCheckViolation            = errors.New("detest: check violation")
	ErrInvalidParameterValue     = errors.New("detest: invalid parameter value")
	ErrCardinalityViolation      = errors.New("detest: more than one row returned by a subquery used as an expression")
	ErrStringDataRightTruncation = errors.New("detest: value too long for the column")
	ErrDataTruncated             = errors.New("detest: data truncated for the column")
	ErrUndefinedColumn           = errors.New("detest: column does not exist")
	ErrAmbiguousColumn           = errors.New("detest: column reference is ambiguous")
	kindErrors                   = map[DBErrorKind]error{UniqueViolation: ErrUniqueViolation, NotNullViolation: ErrNotNullViolation, Deadlock: ErrDeadlock, InFailedTransaction: ErrInFailedTx, LockNotAvailable: ErrLockNotAvailable, UndefinedTable: ErrUndefinedTable, ForeignKeyViolation: ErrForeignKeyViolation,
		DivisionByZero: ErrDivisionByZero, NumericValueOutOfRange: ErrNumericValueOutOfRange, InvalidTextRepresentation: ErrInvalidTextRepresentation,
		SyntaxError: ErrSyntaxError, UndefinedParameter: ErrUndefinedParameter, InvalidColumnReference: ErrInvalidColumnReference, DuplicateTable: ErrDuplicateTable,
		WrongObjectType: ErrWrongObjectType, InvalidTableDefinition: ErrInvalidTableDefinition, NoActiveTransaction: ErrNoActiveTransaction, InvalidSavepoint: ErrInvalidSavepoint,
		CheckViolation: ErrCheckViolation, InvalidParameterValue: ErrInvalidParameterValue, LockWaitTimeout: ErrLockNotAvailable,
		InvalidRowCountInLimit: ErrInvalidParameterValue, InvalidRowCountInOffset: ErrInvalidParameterValue,
		ArithmeticOutOfRange: ErrNumericValueOutOfRange, ForeignKeyParentViolation: ErrForeignKeyViolation,
		CardinalityViolation: ErrCardinalityViolation, StringDataRightTruncation: ErrStringDataRightTruncation, DataTruncated: ErrDataTruncated,
		RestrictViolation: ErrForeignKeyViolation, UndefinedColumn: ErrUndefinedColumn,
		AmbiguousColumn: ErrAmbiguousColumn}
)

// DBError is a database error detest's simulated database raises, with what drivers
// report about it: the SQLSTATE, the MySQL error number, the table, column
// and constraint.
type DBError struct {
	Kind       DBErrorKind
	Code       string // SQLSTATE, such as "23505"
	Number     int    // MySQL's error number, such as 1062; 0 for other servers
	Message    string
	Table      string
	Column     string
	Constraint string
}

func (e *DBError) Error() string {
	if e.Code == "" {
		return "detest: " + e.Message
	}
	return fmt.Sprintf("detest: %s (SQLSTATE %s)", e.Message, e.Code)
}

// Is matches the error of the DBError's kind, such as ErrUniqueViolation.
func (e *DBError) Is(target error) bool { return kindErrors[e.Kind] == target }

// Name identifies the kind, such as "postgres".
func (s *Impl) Name() string { return s.name }

// InnoDB reports whether the server has InnoDB's semantics.
func (s *Impl) InnoDB() bool { return s.innodb }

// Collation is the server's default collation, for MySQL.
func (s *Impl) Collation() string { return s.collation }

// Parser parses the server's SQL.
func (s *Impl) Parser() Parser { return s.parser }

// Isolation is the level transactions run at unless they ask for another.
func (s *Impl) Isolation() IsolationLevel { return s.isolation }

// SearchPath is the schemas an unqualified table name is looked up in.
func (s *Impl) SearchPath() []string { return s.searchPath }

// Check reports whether detest implements level for the server.
func (s *Impl) Check(level IsolationLevel) error {
	if !slices.Contains(s.supported, level) {
		return fmt.Errorf("detest: %s at %s is not implemented", s.name, level)
	}
	return nil
}

// OtherAggregates are the built-in Postgres aggregates detest does not
// implement. They are known as aggregates so that a query using one is
// grouped, or refused before it runs, rather than evaluated per row.
var OtherAggregates = map[string]bool{
	"any_value": true, "array_agg": true, "bit_and": true, "bit_or": true, "bit_xor": true,
	"bool_and": true, "bool_or": true, "every": true, "json_agg": true, "jsonb_agg": true,
	"json_object_agg": true, "jsonb_object_agg": true, "json_arrayagg": true, "json_objectagg": true,
	"range_agg": true, "range_intersect_agg": true, "string_agg": true, "xmlagg": true,
	"corr": true, "covar_pop": true, "covar_samp": true, "regr_avgx": true, "regr_avgy": true,
	"regr_count": true, "regr_intercept": true, "regr_r2": true, "regr_slope": true,
	"regr_sxx": true, "regr_sxy": true, "regr_syy": true, "stddev": true, "stddev_pop": true,
	"stddev_samp": true, "variance": true, "var_pop": true, "var_samp": true,
	"json_agg_strict": true, "jsonb_agg_strict": true, "json_object_agg_strict": true,
	"json_object_agg_unique": true, "json_object_agg_unique_strict": true,
	"jsonb_object_agg_strict": true, "jsonb_object_agg_unique": true, "jsonb_object_agg_unique_strict": true,
	"mode": true, "percentile_cont": true, "percentile_disc": true,
}
