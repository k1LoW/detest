package difftest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/k1LoW/detest"
)

// Backend is one kind of real server.
type Backend interface {
	// Server is the detest server that simulates this kind.
	Server() detest.Server
	// Open creates an empty database for one case.
	Open(t *testing.T) *sql.DB
	// SessionID identifies the server session behind conn.
	SessionID(ctx context.Context, conn *sql.Conn) (int64, error)
	// Blockers returns, for each of the sessions, the sessions it waits for
	// on a lock.
	Blockers(ctx context.Context, db *sql.DB, sessions []int64) (map[int64][]int64, error)
	// Outcome renders an error of the real server's driver as the
	// application would branch on it, such as "ERROR 23505".
	Outcome(err error) (string, bool)
}
