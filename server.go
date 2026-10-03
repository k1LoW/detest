package detest

import (
	"database/sql"
	"fmt"

	"github.com/k1LoW/detest/internal/sqlir"
)

// Server is the kind of database server a DB models, such as postgres.New().
// The packages of the kinds (postgres, mysql) describe it: the SQL it parses,
// the isolation level its transactions run at, its search_path.
type Server = sqlir.Server

// IsolationLevel is a transaction isolation level.
type IsolationLevel = sqlir.IsolationLevel

// The isolation levels. The semantics of a level depend on the kind of
// server, and detest implements a level only for the kinds it lists.
const (
	ReadCommitted  = sqlir.ReadCommitted
	RepeatableRead = sqlir.RepeatableRead
	Serializable   = sqlir.Serializable
)

// SQLError is a database error the simulated database raises: a unique violation, a
// deadlock and so on, with the server's SQLSTATE and the table, column and
// constraint involved. The server's Errors option converts it into the error
// type of the production code's driver.
type SQLError = sqlir.SQLError

// ErrorKind is the class of an SQLError.
type ErrorKind = sqlir.ErrorKind

// The kinds of SQLError.
const (
	UniqueViolation           = sqlir.UniqueViolation
	NotNullViolation          = sqlir.NotNullViolation
	Deadlock                  = sqlir.Deadlock
	InFailedTransaction       = sqlir.InFailedTransaction
	LockNotAvailable          = sqlir.LockNotAvailable
	UndefinedTable            = sqlir.UndefinedTable
	ForeignKeyViolation       = sqlir.ForeignKeyViolation
	DivisionByZero            = sqlir.DivisionByZero
	NumericValueOutOfRange    = sqlir.NumericValueOutOfRange
	InvalidTextRepresentation = sqlir.InvalidTextRepresentation
	SyntaxError               = sqlir.SyntaxError
	UndefinedParameter        = sqlir.UndefinedParameter
	InvalidColumnReference    = sqlir.InvalidColumnReference
	DuplicateTable            = sqlir.DuplicateTable
	WrongObjectType           = sqlir.WrongObjectType
	InvalidTableDefinition    = sqlir.InvalidTableDefinition
	NoActiveTransaction       = sqlir.NoActiveTransaction
	InvalidSavepoint          = sqlir.InvalidSavepoint
)

// ErrUnsupportedSQL reports SQL detest cannot run.
type ErrUnsupportedSQL = sqlir.ErrUnsupportedSQL

// Unknown is the value of an expression detest cannot evaluate.
const Unknown = sqlir.Unknown

func unsupported(what, query string) error { return sqlir.Unsupported(what, query) }

// txIsolation is the level a transaction runs at on s: the server's default,
// or the level BeginTx asked for.
func txIsolation(s Server, level sql.IsolationLevel) (IsolationLevel, error) {
	var l IsolationLevel
	switch level {
	case sql.LevelDefault:
		l = s.Isolation()
	case sql.LevelReadCommitted:
		l = ReadCommitted
	case sql.LevelRepeatableRead:
		l = RepeatableRead
	case sql.LevelSerializable:
		l = Serializable
	default:
		return 0, fmt.Errorf("detest: isolation level %s is not implemented", level)
	}
	return l, s.Check(l)
}
