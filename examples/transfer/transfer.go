// Package transfer moves money between accounts. DebitFirst updates
// the payer first, so two transfers in opposite directions lock the same two
// rows in opposite orders and one of them dies in a deadlock; Transfer
// updates the account with the smaller id first.
package transfer

import (
	"context"
	"database/sql"
)

// DebitFirst moves amount from one account to another, debiting first.
func DebitFirst(ctx context.Context, db *sql.DB, from, to string, amount int) error {
	return move(ctx, db, []string{from, to}, from, to, amount)
}

// Transfer moves amount from one account to another, taking the row locks in
// the order of the account ids, which every transfer agrees on.
func Transfer(ctx context.Context, db *sql.DB, from, to string, amount int) error {
	order := []string{from, to}
	if to < from {
		order = []string{to, from}
	}
	return move(ctx, db, order, from, to, amount)
}

func move(ctx context.Context, db *sql.DB, order []string, from, to string, amount int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range order {
		delta := amount
		if id == from {
			delta = -amount
		}
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET balance = balance + $1 WHERE id = $2`, delta, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
