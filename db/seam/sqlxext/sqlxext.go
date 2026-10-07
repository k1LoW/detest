// Package sqlxext is the sqlx.ExtContext to give code on sqlx in a test,
// so that the goroutines sharing a transaction are scheduled by detest. Tx
// has the methods of sqlx.ExtContext and sqlx.PreparerContext, and the
// context methods of *sqlx.Tx that read into a destination, and follows
// sqlx when it adds to them.
//
// *sqlx.DB and *sqlx.Tx are structs that goroutines use directly, so the
// package applies to code that begins its transactions through something
// the test can replace, and hands the transaction on as an interface:
//
//	type Tx interface {
//		sqlx.ExtContext
//		GetContext(ctx context.Context, dest any, query string, args ...any) error
//		Commit() error
//		Rollback() error
//	}
//
//	type Store struct {
//		begin func(ctx context.Context) (Tx, error) // db.BeginTxx in production
//	}
//
// and in the test:
//
//	sqlDB, store := s.DB("app", postgres.New())
//	db := sqlxext.New(sqlDB, "postgres")
//	st := &Store{begin: func(ctx context.Context) (Tx, error) { return db.BeginTxx(ctx, nil) }}
//
// Code that holds a *sqlx.Tx runs on the *sqlx.DB all the same. A
// goroutine that uses a transaction another process began then runs each
// statement at once, without yielding, and a statement of it that would
// wait for a lock fails as unsupported, as the README describes. Through
// the package, each operation of such a goroutine waits for the
// transaction's connection before it enters database/sql, so its
// statements yield, wait for locks and take part in deadlocks as any
// process's do, and the order of the goroutines is explored.
//
// A commit or a rollback by a goroutine other than the one that began the
// transaction is still refused. A statement prepared on the transaction
// runs without the package.
//
// The transactions run with sqlx's default mapper and in safe mode, which
// DB offers no way to change. The functions sqlx.NamedExecContext,
// sqlx.NamedQueryContext and sqlx.PreparexContext read the mapper and the
// mode only from sqlx's own types, and would run a Tx of the package with
// the defaults whatever the *sqlx.DB was set to.
package sqlxext

import (
	"context"
	"database/sql"

	"github.com/jmoiron/sqlx"

	"github.com/k1LoW/detest/internal/seam"
)

// DB begins transactions whose operations go through the package. It
// keeps its *sqlx.DB to itself, so that nothing sets a mapper or unsafe
// mode on it.
type DB struct{ db *sqlx.DB }

// New returns the DB to begin the code's transactions on, on a *sqlx.DB
// that sqlx.NewDb returns for the driver named driverName. It takes the
// *sql.DB rather than a *sqlx.DB, as sqlx's default mapper cannot be told
// from another one on a *sqlx.DB.
func New(db *sql.DB, driverName string) *DB { return &DB{db: sqlx.NewDb(db, driverName)} }

// BeginTxx begins a transaction on the *sqlx.DB.
func (d *DB) BeginTxx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	ctx, slot := seam.WithSlot(ctx)
	tx, err := d.db.BeginTxx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, seam: slot}, nil
}

var (
	_ sqlx.ExtContext      = (*Tx)(nil)
	_ sqlx.PreparerContext = (*Tx)(nil)
)

// Tx is a transaction BeginTxx began. Each of its operations waits for the
// transaction's connection before it calls the *sqlx.Tx.
type Tx struct {
	tx   *sqlx.Tx
	seam *seam.Slot
}

// DriverName, Rebind and BindNamed run nothing on the connection.
func (t *Tx) DriverName() string { return t.tx.DriverName() }

func (t *Tx) Rebind(query string) string { return t.tx.Rebind(query) }

func (t *Tx) BindNamed(query string, arg any) (string, []any, error) {
	return t.tx.BindNamed(query, arg)
}

func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	release, err := t.seam.Enter(ctx, "exec", query, args...)
	if err != nil {
		return nil, err
	}
	defer release()
	return t.tx.ExecContext(ctx, query, args...)
}

// NamedExecContext binds arg before it waits for the connection, as sqlx
// binds it before it reaches the database, so that an arg that fails to
// bind returns at once rather than after a wait production does not have.
func (t *Tx) NamedExecContext(ctx context.Context, query string, arg any) (sql.Result, error) {
	bound, args, err := t.tx.BindNamed(query, arg)
	if err != nil {
		return nil, err
	}
	return t.ExecContext(ctx, bound, args...)
}

func (t *Tx) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	release, err := t.seam.Enter(ctx, "prepare", query)
	if err != nil {
		return nil, err
	}
	defer release()
	return t.tx.PrepareContext(ctx, query)
}

// QueryContext gives the connection back when it returns, but the driver
// keeps it until the rows are closed.
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	release, err := t.seam.Enter(ctx, "query", query, args...)
	if err != nil {
		return nil, err
	}
	defer release()
	return t.tx.QueryContext(ctx, query, args...)
}

// QueryxContext is QueryContext for *sqlx.Rows.
func (t *Tx) QueryxContext(ctx context.Context, query string, args ...any) (*sqlx.Rows, error) {
	release, err := t.seam.Enter(ctx, "query", query, args...)
	if err != nil {
		return nil, err
	}
	defer release()
	return t.tx.QueryxContext(ctx, query, args...)
}

// QueryRowxContext is QueryxContext for one row. A *sqlx.Row cannot carry
// an error of the package's, so when the run is over it calls the
// *sqlx.Tx all the same, and the driver returns the end of the run.
func (t *Tx) QueryRowxContext(ctx context.Context, query string, args ...any) *sqlx.Row {
	release, err := t.seam.Enter(ctx, "query", query, args...)
	if err == nil {
		defer release()
	}
	return t.tx.QueryRowxContext(ctx, query, args...)
}

// GetContext reads one row into dest.
func (t *Tx) GetContext(ctx context.Context, dest any, query string, args ...any) error {
	release, err := t.seam.Enter(ctx, "query", query, args...)
	if err != nil {
		return err
	}
	defer release()
	return t.tx.GetContext(ctx, dest, query, args...)
}

// SelectContext reads every row into dest.
func (t *Tx) SelectContext(ctx context.Context, dest any, query string, args ...any) error {
	release, err := t.seam.Enter(ctx, "query", query, args...)
	if err != nil {
		return err
	}
	defer release()
	return t.tx.SelectContext(ctx, dest, query, args...)
}

func (t *Tx) Commit() error {
	release, err := t.seam.Enter(context.Background(), "commit", "")
	if err != nil {
		return err
	}
	defer release()
	return t.tx.Commit()
}

func (t *Tx) Rollback() error {
	release, err := t.seam.Enter(context.Background(), "rollback", "")
	if err != nil {
		return err
	}
	defer release()
	return t.tx.Rollback()
}
