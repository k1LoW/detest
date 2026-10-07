// Package mysqlerr makes detest's database errors look like go-sql-driver's,
// for code under test that branches on *mysql.MySQLError, directly or
// through GORM's TranslateError:
//
//	db, store := s.DB("app", mysql.New(mysql.Errors(mysqlerr.Convert)))
package mysqlerr

import (
	gomysql "github.com/go-sql-driver/mysql"

	"github.com/k1LoW/detest"
)

// Convert returns e as the *mysql.MySQLError MySQL would have sent.
func Convert(e *detest.DBError) error {
	out := &gomysql.MySQLError{Number: uint16(e.Number), Message: e.Message} //nolint:gosec // MySQL's error numbers fit in 16 bits
	copy(out.SQLState[:], e.Code)
	return out
}
