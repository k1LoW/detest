// Package mysql is MySQL (InnoDB) for detest: pass mysql.New() to
// detest.Sim.DB. Neither its parser nor its isolation semantics are
// implemented yet: the IR already covers MySQL's statement shapes (INSERT
// IGNORE, ON DUPLICATE KEY UPDATE, UPDATE/DELETE LIMIT), so a parser only has
// to convert a MySQL AST into it, but Repeatable Read, InnoDB's default, has
// snapshot reads and next-key locks detest does not model.
package mysql

import (
	"fmt"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// Option configures the server New describes.
type Option func(*config)

type config struct {
	isolation sqlir.IsolationLevel
}

// Isolation sets the level transactions run at when they do not ask for one,
// as transaction_isolation does (default Repeatable Read).
func Isolation(level sqlir.IsolationLevel) Option {
	return func(c *config) { c.isolation = level }
}

// New describes a MySQL server. detest implements none of its levels yet.
func New(opts ...Option) sqlir.Server {
	c := &config{isolation: sqlir.RepeatableRead}
	for _, o := range opts {
		o(c)
	}
	return sqlir.NewServer(sqlir.ServerSpec{Name: "mysql", Parser: parser{}, Isolation: c.isolation, Codes: codes})
}

func codes(k sqlir.DBErrorKind) (string, int) {
	switch k {
	case sqlir.UniqueViolation:
		return "23000", 1062 // ER_DUP_ENTRY
	case sqlir.NotNullViolation:
		return "23000", 1048 // ER_BAD_NULL_ERROR
	case sqlir.Deadlock:
		return "40001", 1213 // ER_LOCK_DEADLOCK
	case sqlir.LockNotAvailable:
		return "HY000", 3572 // ER_LOCK_NOWAIT
	case sqlir.UndefinedTable:
		return "42S02", 1146 // ER_NO_SUCH_TABLE
	case sqlir.ForeignKeyViolation:
		return "23000", 1452 // ER_NO_REFERENCED_ROW_2 (1451 when a parent is deleted)
	case sqlir.DivisionByZero:
		return "22012", 1365 // ER_DIVISION_BY_ZERO
	case sqlir.NumericValueOutOfRange:
		return "22003", 1690 // ER_DATA_OUT_OF_RANGE
	case sqlir.InvalidTextRepresentation:
		return "HY000", 1366 // ER_TRUNCATED_WRONG_VALUE_FOR_FIELD
	case sqlir.SyntaxError:
		return "42000", 1064 // ER_PARSE_ERROR
	case sqlir.DuplicateTable:
		return "42S01", 1050 // ER_TABLE_EXISTS_ERROR
	case sqlir.WrongObjectType:
		return "HY000", 1347 // ER_WRONG_OBJECT
	case sqlir.InvalidTableDefinition:
		return "42000", 1068 // ER_MULTIPLE_PRI_KEY
	case sqlir.InvalidSavepoint:
		return "42000", 1305 // ER_SP_DOES_NOT_EXIST
	case sqlir.CheckViolation:
		return "HY000", 3819 // ER_CHECK_CONSTRAINT_VIOLATED
	}
	return "", 0
}

type parser struct{}

func (parser) Name() string { return "mysql" }

func (parser) Parse(query string) (sqlir.Statement, error) {
	return nil, fmt.Errorf("detest: the MySQL parser is not implemented yet: %s", strings.TrimSpace(query))
}
