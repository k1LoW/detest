// Package kvlock bills an account once per period under a lock held in a
// key-value store such as Redis, taken with SET NX and a TTL. Bill trusts
// the lock, so a worker that stalls past the TTL bills alongside the worker
// that took the lock after it expired. BillLocked takes the account's row
// lock in the database as well, which holds for as long as the transaction
// does, so the KV lock only keeps workers from contending.
package kvlock

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// KV is the part of a key-value store client the billing uses.
type KV interface {
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Del(ctx context.Context, key string) error
}

// LockTTL is how long the KV lock lasts if its holder never releases it.
const LockTTL = 10 * time.Second

// ErrBusy is returned when another worker kept the lock through every retry.
var ErrBusy = errors.New("kvlock: lock is busy")

// Bill bills account for period unless it is already billed.
func Bill(ctx context.Context, db *sql.DB, kv KV, account, period string) error {
	return withLock(ctx, kv, account, period, func() error {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM invoices WHERE account = $1 AND period = $2`, account, period).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		_, err := db.ExecContext(ctx, `INSERT INTO invoices (account, period) VALUES ($1, $2)`, account, period)
		return err
	})
}

// BillLocked bills as Bill does, checking and inserting in one transaction
// that holds the account's row lock.
func BillLocked(ctx context.Context, db *sql.DB, kv KV, account, period string) error {
	return withLock(ctx, kv, account, period, func() error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `SELECT id FROM accounts WHERE id = $1 FOR UPDATE`, account); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM invoices WHERE account = $1 AND period = $2`, account, period).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO invoices (account, period) VALUES ($1, $2)`, account, period); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

func withLock(ctx context.Context, kv KV, account, period string, fn func() error) error {
	key := "bill:" + account + ":" + period
	for range 30 {
		ok, err := kv.SetNX(ctx, key, []byte("1"), LockTTL)
		if err != nil {
			return err
		}
		if ok {
			// A Del after the TTL ran out may remove another worker's lock,
			// which only lets a third worker in sooner.
			defer func() { _ = kv.Del(ctx, key) }()
			return fn()
		}
		time.Sleep(time.Second)
	}
	return ErrBusy
}
