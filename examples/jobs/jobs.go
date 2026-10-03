// Package jobs runs queued jobs. RunTwoStep commits the claim of a job before
// doing it, so a worker that dies in between leaves the job claimed forever;
// Run claims, does and completes a job in one transaction, which a dying
// worker rolls back.
package jobs

import (
	"context"
	"database/sql"
	"errors"
)

// ErrNoJob is returned when no job is pending.
var ErrNoJob = errors.New("no job pending")

const next = `SELECT id FROM jobs WHERE status = 'pending' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`

// RunTwoStep claims the next pending job, does it, and marks it done.
func RunTwoStep(ctx context.Context, db *sql.DB, do func(id string) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	if err := tx.QueryRowContext(ctx, next).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return ErrNoJob
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'running' WHERE id = $1`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := do(id); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `UPDATE jobs SET status = 'done' WHERE id = $1`, id)
	return err
}

// Run does the next pending job within the transaction that claims it.
func Run(ctx context.Context, db *sql.DB, do func(id string) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	if err := tx.QueryRowContext(ctx, next).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return ErrNoJob
	} else if err != nil {
		return err
	}
	if err := do(id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'done' WHERE id = $1`, id); err != nil {
		return err
	}
	return tx.Commit()
}
