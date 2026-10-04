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
// applications run them.
type Step struct {
	Conn int
	SQL  string
}

// S is a shorthand for Step.
func S(conn int, sql string) Step { return Step{Conn: conn, SQL: sql} }

type stepKind int

const (
	kindExec stepKind = iota
	kindQuery
	kindBegin
	kindCommit
	kindRollback
)

func (s Step) kind() stepKind {
	q := strings.ToUpper(strings.TrimSpace(s.SQL))
	switch {
	case q == "BEGIN" || strings.HasPrefix(q, "BEGIN ISOLATION LEVEL"):
		return kindBegin
	case q == "COMMIT":
		return kindCommit
	case q == "ROLLBACK":
		return kindRollback
	case strings.HasPrefix(q, "SELECT"), strings.HasPrefix(q, "WITH"), strings.HasPrefix(q, "VALUES"), strings.Contains(q, " RETURNING "):
		return kindQuery
	}
	return kindExec
}
