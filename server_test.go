package detest

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/k1LoW/detest/internal/sqlir"
	"github.com/k1LoW/detest/mysql"
	"github.com/k1LoW/detest/postgres"
)

// A transaction asking for a level detest does not implement for the server
// fails instead of running with other semantics than production's.
func TestBeginTxIsolation(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	for _, tc := range []struct {
		level sql.IsolationLevel
		ok    bool
	}{
		{sql.LevelDefault, true},
		{sql.LevelReadCommitted, true},
		{sql.LevelRepeatableRead, false},
		{sql.LevelSerializable, false},
		{sql.LevelSnapshot, false},
	} {
		tx, err := db.BeginTx(t.Context(), &sql.TxOptions{Isolation: tc.level})
		if tc.ok != (err == nil) {
			t.Errorf("%s: %v", tc.level, err)
		}
		if tx != nil {
			_ = tx.Rollback()
		}
	}
}

func TestServerIsolation(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Server
		want string
	}{
		{"postgres default", postgres.New(), ""},
		{"postgres repeatable read", postgres.New(postgres.Isolation(RepeatableRead)), "postgres at Repeatable Read is not implemented"},
		{"mysql default", mysql.New(), ""},
		{"mysql read committed", mysql.New(mysql.Isolation(ReadCommitted)), ""},
	} {
		kind := sqlir.ImplOf(tc.s)
		err := kind.Check(kind.Isolation())
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// Without an Errors option the code under test sees *DBError, which carries
// the server's SQLSTATE and matches the kind's error with errors.Is.
func TestDBError(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE users (id text PRIMARY KEY, email text UNIQUE)`)
	mustExec(t, db, `INSERT INTO users (id, email) VALUES ('u1', 'a@x')`)
	for _, tc := range []struct {
		q          string
		is         error
		code       string
		constraint string
	}{
		{`INSERT INTO users (id, email) VALUES ('u1', 'b@x')`, ErrUniqueViolation, "23505", "users_pkey"},
		{`INSERT INTO users (id, email) VALUES ('u2', 'a@x')`, ErrUniqueViolation, "23505", "users_email_key"},
		{`INSERT INTO users (email) VALUES ('c@x')`, ErrNotNullViolation, "23502", ""},
	} {
		_, err := db.Exec(tc.q)
		var se *DBError
		if !errors.Is(err, tc.is) || !errors.As(err, &se) || se.Code != tc.code || se.Constraint != tc.constraint {
			t.Errorf("%s: %#v", tc.q, err)
		}
	}
}
