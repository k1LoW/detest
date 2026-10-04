// Package mysql is MySQL with InnoDB for detest: pass mysql.New() to
// detest.Sim.DB. SQL is parsed with the MySQL grammar of TiDB's parser, and
// the semantics are InnoDB's at Repeatable Read (the default), Read
// Committed and Serializable.
package mysql

import (
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// Option configures the server New describes.
type Option func(*config)

type config struct {
	isolation sqlir.IsolationLevel
	database  string
	collation string
	convert   func(*sqlir.DBError) error
}

// Collation sets the server's default collation, as collation_server does
// (default utf8mb4_0900_ai_ci, MySQL 8's), which a text column takes when
// neither it nor its table declares one. detest compares strings exactly,
// as a _bin collation does, and refuses the statements whose outcome
// depends on how a column of a case-insensitive (_ci) collation compares.
func Collation(name string) Option {
	return func(c *config) { c.collation = strings.ToLower(name) }
}

// Database sets the current database, which unqualified table names refer
// to (default "app"). A name qualified with another database refers to that
// database's table.
func Database(name string) Option {
	return func(c *config) { c.database = name }
}

// Errors sets how the database errors detest raises reach the code under
// test. Production code branches on its driver's error type, such as
// go-sql-driver's *mysql.MySQLError; convert builds that type from the error
// number and the details detest reports (mysqlerr.Convert does it for
// go-sql-driver). Without Errors the code under test sees *detest.DBError.
func Errors(convert func(*sqlir.DBError) error) Option {
	return func(c *config) { c.convert = convert }
}

// Isolation sets the level transactions run at when they do not ask for one,
// as transaction_isolation does (default Repeatable Read).
func Isolation(level sqlir.IsolationLevel) Option {
	return func(c *config) { c.isolation = level }
}

// New describes a MySQL server with InnoDB tables. detest implements
// Repeatable Read, Read Committed and Serializable.
func New(opts ...Option) sqlir.Server {
	c := &config{isolation: sqlir.RepeatableRead, database: "app", collation: "utf8mb4_0900_ai_ci"}
	for _, o := range opts {
		o(c)
	}
	return sqlir.NewServer(sqlir.ServerSpec{Name: "mysql", Parser: parser{}, Isolation: c.isolation, Codes: codes, InnoDB: true,
		Supported:  []sqlir.IsolationLevel{sqlir.ReadCommitted, sqlir.RepeatableRead, sqlir.Serializable},
		SearchPath: []string{c.database}, Convert: c.convert, Collation: c.collation})
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
		return "23000", 1452 // ER_NO_REFERENCED_ROW_2
	case sqlir.DivisionByZero:
		return "22012", 1365 // ER_DIVISION_BY_ZERO
	case sqlir.NumericValueOutOfRange:
		return "22003", 1264 // ER_WARN_DATA_OUT_OF_RANGE, as a write out of a column's range reports
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
	case sqlir.InvalidParameterValue:
		return "HY000", 1210 // ER_WRONG_ARGUMENTS
	case sqlir.InvalidRowCountInLimit, sqlir.InvalidRowCountInOffset:
		return "HY000", 1210 // ER_WRONG_ARGUMENTS
	case sqlir.CardinalityViolation:
		return "21000", 1242 // ER_SUBQUERY_NO_1_ROW
	case sqlir.UndefinedColumn:
		return "42S22", 1054 // ER_BAD_FIELD_ERROR
	case sqlir.AmbiguousColumn:
		return "23000", 1052 // ER_NON_UNIQ_ERROR
	case sqlir.ForeignKeyParentViolation, sqlir.RestrictViolation:
		return "23000", 1451 // ER_ROW_IS_REFERENCED_2
	case sqlir.ArithmeticOutOfRange:
		return "22003", 1690 // ER_DATA_OUT_OF_RANGE
	case sqlir.LockWaitTimeout:
		return "HY000", 1205 // ER_LOCK_WAIT_TIMEOUT
	case sqlir.StringDataRightTruncation:
		return "22001", 1406 // ER_DATA_TOO_LONG
	case sqlir.DataTruncated:
		return "01000", 1265 // WARN_DATA_TRUNCATED, an error in strict mode
	}
	return "", 0
}
