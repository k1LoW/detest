// Package difftest runs the same statements on detest and on a real server
// and compares what the application would see.
package difftest

import (
	"strings"
	"time"
)

// Case is a fixed sequence of statements over several connections.
type Case struct {
	Name string
	// Schema builds the tables before any step runs.
	Schema []string
	// Seed inserts the rows the steps start from.
	Seed []string
	// Conns is the number of connections the steps use.
	Conns int
	Steps []Step
	// Pauses runs the case once more on the real server per entry, sleeping
	// before the steps an entry maps, for a case whose outcome on the server
	// depends on timing, such as which session breaks a deadlock. detest has
	// no time between steps, so it has to give every outcome these runs give
	// and no other.
	Pauses []map[int]time.Duration
}

// Step runs one statement on connection Conn (0-based). BEGIN, COMMIT and
// ROLLBACK go through database/sql's transactions, since that is how
// applications run them. A Query step compares the rows the statement
// returns, and any other step the count of rows it affected.
type Step struct {
	Conn  int
	SQL   string
	Query bool
	// Args are bound to the statement's parameters, on both sides alike.
	Args []any
	// Scan is the type a Query step scans every column into, as an
	// application does: "string", "float64", "int64" or "decimal" (a
	// sql.Scanner over big.Rat), and any when empty. A row that fails to
	// scan is rendered as SCAN FAILED.
	Scan string
	// CancelTx ends the context the open transaction of connection Conn
	// began with, as a request's deadline does while the transaction is
	// idle, so that database/sql rolls it back from a goroutine of its own.
	CancelTx bool
}

// S is a step run with Exec.
func S(conn int, sql string) Step { return Step{Conn: conn, SQL: sql} }

// Q is a step run with Query.
func Q(conn int, sql string) Step { return Step{Conn: conn, SQL: sql, Query: true} }

// QA is a step run with Query, binding args to its parameters.
func QA(conn int, sql string, args ...any) Step {
	return Step{Conn: conn, SQL: sql, Query: true, Args: args}
}

// QS is a step run with Query that scans every column into the type scan
// names (Step.Scan), binding args to its parameters.
func QS(conn int, scan, sql string, args ...any) Step {
	return Step{Conn: conn, SQL: sql, Query: true, Args: args, Scan: scan}
}

// CT is a step that ends the context of connection conn's open transaction.
func CT(conn int) Step { return Step{Conn: conn, SQL: "CANCEL TRANSACTION CONTEXT", CancelTx: true} }

type stepKind int

const (
	kindExec stepKind = iota
	kindQuery
	kindBegin
	kindCommit
	kindRollback
)

func (s Step) kind() stepKind {
	if s.Query {
		return kindQuery
	}
	q := strings.ToUpper(strings.TrimSpace(s.SQL))
	switch {
	case q == "BEGIN" || strings.HasPrefix(q, "BEGIN ISOLATION LEVEL"):
		return kindBegin
	case q == "COMMIT":
		return kindCommit
	case q == "ROLLBACK":
		return kindRollback
	}
	return kindExec
}
