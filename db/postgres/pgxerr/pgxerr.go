// Package pgxerr makes detest's database errors look like pgx's, for code
// under test that branches on *pgconn.PgError, directly or through GORM's
// TranslateError:
//
//	db, store := s.DB("app", postgres.New(postgres.Errors(pgxerr.Convert)))
package pgxerr

import (
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/k1LoW/detest"
)

// Convert returns e as the *pgconn.PgError Postgres would have sent.
func Convert(e *detest.DBError) error {
	return &pgconn.PgError{
		Severity:            "ERROR",
		SeverityUnlocalized: "ERROR",
		Code:                e.Code,
		Message:             e.Message,
		TableName:           e.Table,
		ColumnName:          e.Column,
		ConstraintName:      e.Constraint,
	}
}
