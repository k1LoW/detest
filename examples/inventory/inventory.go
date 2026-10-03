// Package inventory reserves items from stock. ReserveUnlocked reads the
// stock and writes it back in two statements, so two concurrent
// reservations can both take the last item; Reserve locks the row first.
package inventory

import (
	"context"
	"database/sql"
	"errors"
)

// ErrOutOfStock is returned when nothing is left to reserve.
var ErrOutOfStock = errors.New("out of stock")

// ReserveUnlocked takes one item of sku. It loses updates under concurrency.
func ReserveUnlocked(ctx context.Context, db *sql.DB, sku string) error {
	return reserve(ctx, db, sku, `SELECT n FROM stock WHERE sku = $1`)
}

// Reserve takes one item of sku, holding the row lock from the read to the
// commit.
func Reserve(ctx context.Context, db *sql.DB, sku string) error {
	return reserve(ctx, db, sku, `SELECT n FROM stock WHERE sku = $1 FOR UPDATE`)
}

func reserve(ctx context.Context, db *sql.DB, sku, read string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, read, sku).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrOutOfStock
	}
	if _, err := tx.ExecContext(ctx, `UPDATE stock SET n = $1 WHERE sku = $2`, n-1, sku); err != nil {
		return err
	}
	return tx.Commit()
}
