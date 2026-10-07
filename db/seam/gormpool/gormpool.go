// Package gormpool is the connection to give GORM in a test, so that the
// goroutines sharing a GORM transaction are scheduled by detest:
//
//	sqlDB, store := s.DB("app", postgres.New())
//	gdb, err := gorm.Open(gormpostgres.New(gormpostgres.Config{Conn: gormpool.New(sqlDB)}), &gorm.Config{})
//
// GORM runs on the *sql.DB itself too. A goroutine that uses a transaction
// another process began, such as one of an errgroup deleting the rows its
// process locked, then runs each statement at once, without yielding, and a
// statement of it that would wait for a lock fails as unsupported, as the
// README describes. Through the pool, each operation of such a goroutine
// waits for the transaction's connection before it enters database/sql, so
// its statements yield, wait for locks and take part in deadlocks as any
// process's do, and the order of the goroutines is explored.
//
// A commit or a rollback by a goroutine other than the one that began the
// transaction is still refused. GORM's PrepareStmt must be off, as a
// prepared statement runs on database/sql without the pool.
package gormpool

import (
	"context"
	"database/sql"

	"gorm.io/gorm"

	"github.com/k1LoW/detest/internal/seam"
)

// Pool is a gorm.ConnPool over a *sql.DB.
type Pool struct{ db *sql.DB }

// New returns the pool to give GORM as its Conn.
func New(db *sql.DB) *Pool { return &Pool{db: db} }

var (
	_ gorm.ConnPool         = (*Pool)(nil)
	_ gorm.ConnPoolBeginner = (*Pool)(nil)
	_ gorm.GetDBConnector   = (*Pool)(nil)
	_ gorm.ConnPool         = (*Tx)(nil)
	_ gorm.TxCommitter      = (*Tx)(nil)
	_ gorm.GetDBConnector   = (*Tx)(nil)
)

func (p *Pool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return p.db.PrepareContext(ctx, query)
}

func (p *Pool) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return p.db.ExecContext(ctx, query, args...)
}

func (p *Pool) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return p.db.QueryContext(ctx, query, args...)
}

func (p *Pool) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return p.db.QueryRowContext(ctx, query, args...)
}

// Ping is there because gorm.Open pings a pool that has it, as it pings a
// *sql.DB.
func (p *Pool) Ping() error { return p.db.Ping() }

// GetDBConn returns the *sql.DB, which gorm.DB.DB reads.
func (p *Pool) GetDBConn() (*sql.DB, error) { return p.db, nil }

// BeginTx begins a transaction whose operations go through the pool. It
// returns a gorm.ConnPool rather than a *sql.Tx, which GORM would use
// directly.
func (p *Pool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	ctx, slot := seam.WithSlot(ctx)
	tx, err := p.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, db: p.db, seam: slot.Tx}, nil
}

// Tx is a transaction BeginTx began. Each of its operations waits for the
// transaction's connection before it calls the *sql.Tx.
type Tx struct {
	tx   *sql.Tx
	db   *sql.DB
	seam seam.Tx // nil when the database is not detest's
}

func (t *Tx) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	release, err := t.enter("prepare", query)
	if err != nil {
		return nil, err
	}
	defer release()
	return t.tx.PrepareContext(ctx, query)
}

func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	release, err := t.enter("exec", query, args...)
	if err != nil {
		return nil, err
	}
	defer release()
	return t.tx.ExecContext(ctx, query, args...)
}

// QueryContext gives the connection back when it returns, but the driver
// keeps it until the rows are closed.
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	release, err := t.enter("query", query, args...)
	if err != nil {
		return nil, err
	}
	defer release()
	return t.tx.QueryContext(ctx, query, args...)
}

// QueryRowContext is QueryContext for one row. A *sql.Row cannot carry an
// error of the pool's, so when the run is over it calls the *sql.Tx all the
// same, and the driver returns the end of the run.
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	release, err := t.enter("query", query, args...)
	if err == nil {
		defer release()
	}
	return t.tx.QueryRowContext(ctx, query, args...)
}

func (t *Tx) Commit() error {
	release, err := t.enter("commit", "")
	if err != nil {
		return err
	}
	defer release()
	return t.tx.Commit()
}

func (t *Tx) Rollback() error {
	release, err := t.enter("rollback", "")
	if err != nil {
		return err
	}
	defer release()
	return t.tx.Rollback()
}

// GetDBConn returns the *sql.DB the transaction began on, which gorm.DB.DB
// reads.
func (t *Tx) GetDBConn() (*sql.DB, error) { return t.db, nil }

func (t *Tx) enter(op, query string, args ...any) (func(), error) {
	if t.seam == nil {
		return func() {}, nil
	}
	return t.seam.Enter(op, query, args...)
}
