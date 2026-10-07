package kvlock

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/k1LoW/detest"
	"github.com/k1LoW/detest/db/postgres"
)

// fakeKV is a key-value store with the commands a cache or a lock uses, GET,
// SET, SET NX and DEL with a TTL, built on detest's external calls. Each
// command is one ext.Do, which makes it a yield point and atomic, and lets
// the explorer fail it before or after its effect. Expiry is read from the
// bubble's fake clock, which MaxStalls and sleeping processes advance. It
// has no transactions, scripts or data types beyond strings, so it stands
// for Redis or memcached only where the code under test sends these
// commands. Values are copied in and out, as a client sends and receives
// bytes, so a caller reusing its buffer changes nothing in the store.
type fakeKV struct {
	s     *detest.Sim
	write *detest.External
	read  *detest.External
	m     map[string]entry
}

type entry struct {
	value   []byte
	expires time.Time // zero for no TTL
}

var errNotFound = errors.New("fakekv: not found")

func newFakeKV(s *detest.Sim, name string) *fakeKV {
	kv := &fakeKV{
		s:     s,
		write: s.External(name),
		read:  s.External(name+" read", detest.ReadOnly()),
		m:     map[string]entry{},
	}
	// detest resets its own resources before every run, and a map of the
	// test's own is not one of them.
	s.Seed(func() { clear(kv.m) })
	return kv
}

func (kv *fakeKV) Get(_ context.Context, key string) ([]byte, error) {
	var v []byte
	err := kv.do(kv.read, "GET "+key, func() error {
		e, ok := kv.live(key)
		if !ok {
			return errNotFound
		}
		v = bytes.Clone(e.value)
		return nil
	})
	return v, err
}

func (kv *fakeKV) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	return kv.do(kv.write, "SET "+key, func() error {
		kv.m[key] = entry{value: bytes.Clone(value), expires: expiry(ttl)}
		return nil
	})
}

// SetNX reports a key already set as false with no error, as Redis does, so
// the trace shows the call as returned ok either way.
func (kv *fakeKV) SetNX(_ context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	var set bool
	err := kv.do(kv.write, "SET NX "+key, func() error {
		if _, ok := kv.live(key); ok {
			return nil
		}
		kv.m[key] = entry{value: bytes.Clone(value), expires: expiry(ttl)}
		set = true
		return nil
	})
	return set, err
}

func (kv *fakeKV) Del(_ context.Context, key string) error {
	return kv.do(kv.write, "DEL "+key, func() error {
		delete(kv.m, key)
		return nil
	})
}

// do runs cmd as one external call of the calling process, or at once
// outside any process, such as in a seed.
func (kv *fakeKV) do(ext *detest.External, desc string, cmd func() error) error {
	p := kv.s.Current()
	if p == nil {
		return cmd()
	}
	return ext.Do(p, desc, cmd)
}

func (kv *fakeKV) live(key string) (entry, bool) {
	e, ok := kv.m[key]
	if ok && !e.expires.IsZero() && !time.Now().Before(e.expires) {
		delete(kv.m, key)
		return entry{}, false
	}
	return e, ok
}

func expiry(ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(ttl)
}

// simulate declares two workers billing the same account and period, and
// the invariant that it is billed once.
func simulate(bill func(context.Context, *sql.DB, KV, string, string) error) func(t *testing.T, s *detest.Sim) {
	return func(t *testing.T, s *detest.Sim) {
		db, store := s.DB("app", postgres.New())
		for _, q := range []string{
			`CREATE TABLE accounts (id text PRIMARY KEY)`,
			`CREATE TABLE invoices (id bigserial PRIMARY KEY, account text NOT NULL, period text NOT NULL)`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		kv := newFakeKV(s, "redis")
		s.Seed(func() {
			if _, err := db.Exec(`INSERT INTO accounts VALUES ('a1')`); err != nil {
				t.Fatal(err)
			}
		})
		s.Manual("worker", 2, func(p *detest.Proc) error {
			err := bill(p.Context(), db, kv, "a1", "2026-10")
			if errors.Is(err, detest.ErrUnavailable) {
				return nil // the job is retried later, which the invariant does not wait for
			}
			return err
		}, detest.Instances(2))
		s.AtQuiescence(func(st *detest.State) error {
			n := 0
			for _, row := range st.Rows(store, "invoices") {
				if row.Str("account") == "a1" && row.Str("period") == "2026-10" {
					n++
				}
			}
			if n > 1 {
				return fmt.Errorf("a1 billed %d times for 2026-10", n)
			}
			return nil
		})
	}
}

func TestBillTwiceWhenTheLockExpires(t *testing.T) {
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		simulate(Bill)(t, s)
		s.ExpectViolation("a1 billed 2 times")
	}, detest.MaxStalls(1, time.Minute))
}

func TestBillWithoutStalls(t *testing.T) {
	detest.Explore(t, simulate(Bill))
}

func TestBillLocked(t *testing.T) {
	detest.Explore(t, simulate(BillLocked), detest.MaxStalls(1, time.Minute))
}
