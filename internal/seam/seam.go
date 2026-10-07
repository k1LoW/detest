// Package seam carries what passes between detest's driver and the packages
// under db/seam, which take the place of the connection or the transaction
// a library such as GORM, sqlc or sqlx is given, so that they need not
// import detest itself.
//
// database/sql holds a transaction's locks while the driver runs, and a
// goroutine parked there blocks its siblings on a sync.Mutex, which synctest
// does not count as blocked. A seam package therefore makes a goroutine wait
// for the transaction's connection before it enters database/sql, where
// detest can park it, and the driver then lets the goroutine yield and wait
// for locks as itself.
package seam

import "context"

// Hook is detest's side of a transaction a seam package began.
type Hook interface {
	// Enter waits until the calling goroutine may use the transaction's
	// connection, and takes it. release gives it back. It is a no-op outside
	// a run, and for a transaction no process began. op, query and args
	// describe the operation, which orders goroutines adopted at the same
	// step, as the driver's statement does for a goroutine whose first call
	// is a statement.
	Enter(op, query string, args ...any) (release func(), err error)
}

// Slot receives the Hook of a transaction the driver begins with a context
// WithSlot returned. It stays empty when the database is not detest's.
type Slot struct{ Hook Hook }

type slotKey struct{}

// WithSlot returns a context to begin a transaction with, and the slot the
// driver fills.
func WithSlot(ctx context.Context) (context.Context, *Slot) {
	s := &Slot{}
	return context.WithValue(ctx, slotKey{}, s), s
}

// SlotOf returns the slot ctx carries, nil if it carries none.
func SlotOf(ctx context.Context) *Slot {
	s, _ := ctx.Value(slotKey{}).(*Slot)
	return s
}

// Enter is the Hook's Enter, and a no-op when the slot stayed empty.
func (s *Slot) Enter(op, query string, args ...any) (release func(), err error) {
	if s == nil || s.Hook == nil {
		return func() {}, nil
	}
	return s.Hook.Enter(op, query, args...)
}
