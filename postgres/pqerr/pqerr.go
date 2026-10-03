// Package pqerr makes detest's database errors look like lib/pq's, for code
// under test that branches on *pq.Error:
//
//	db, store := s.DB("app", postgres.New(postgres.Errors(pqerr.Convert)))
package pqerr

import (
	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"

	"github.com/k1LoW/detest"
)

// Convert returns e as the *pq.Error Postgres would have sent.
func Convert(e *detest.DBError) error {
	return &pq.Error{
		Severity:   "ERROR",
		Code:       pqerror.Code(e.Code),
		Message:    e.Message,
		Table:      e.Table,
		Column:     e.Column,
		Constraint: e.Constraint,
	}
}
