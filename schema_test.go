package detest

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/k1LoW/detest/postgres"
)

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestSchemaKeysAndUniques(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE memberships (tenant_id text, user_id text, role text, PRIMARY KEY (tenant_id, user_id))`)
	mustExec(t, db, `CREATE TABLE users (id serial PRIMARY KEY, email text UNIQUE, name text)`)
	mustExec(t, db, `CREATE UNIQUE INDEX users_lower_name ON users (lower(name)) WHERE name IS NOT NULL`)

	mustExec(t, db, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ('t1', 'u1', 'admin')`)
	mustExec(t, db, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ('t2', 'u1', 'member')`)
	if _, err := db.Exec(`INSERT INTO memberships (tenant_id, user_id, role) VALUES ('t1', 'u1', 'member')`); !errors.Is(err, ErrUniqueViolation) {
		t.Fatalf("composite primary key: got %v", err)
	}
	if r, ok := (&State{dbs: map[string]map[string]map[string]Row{"app": store.committed}}).Row(store, "memberships", "t2", "u1"); !ok || r.Str("role") != "member" {
		t.Fatalf("lookup by composite key: %v %v", r, ok)
	}

	var id1, id2 int64
	if err := db.QueryRow(`INSERT INTO users (email, name) VALUES ($1, $2) RETURNING id`, "a@example.com", "Alice").Scan(&id1); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`INSERT INTO users (email) VALUES ($1) RETURNING id`, "b@example.com").Scan(&id2); err != nil {
		t.Fatal(err)
	}
	if id1 != 1 || id2 != 2 {
		t.Fatalf("serial ids: %d, %d", id1, id2)
	}
	for _, tc := range []struct {
		q    string
		args []any
		ok   bool
	}{
		{`INSERT INTO users (email) VALUES ($1)`, []any{"a@example.com"}, false},          // unique column
		{`INSERT INTO users (email) VALUES (NULL)`, nil, true},                            // NULLs do not collide
		{`INSERT INTO users (email) VALUES (NULL)`, nil, true},                            //
		{`INSERT INTO users (email, name) VALUES ($1, $2)`, []any{"c@x", "ALICE"}, false}, // expression index
		{`INSERT INTO users (email, name) VALUES ($1, NULL)`, []any{"d@x"}, true},         // outside the partial index
		{`UPDATE users SET email = $1 WHERE id = 2`, []any{"a@example.com"}, false},       // update into a taken value
		{`UPDATE users SET name = $1 WHERE id = 2`, []any{"Bob"}, true},
		{`UPDATE users SET id = 10 WHERE id = 2`, nil, true},  // primary key change moves the row
		{`UPDATE users SET id = 1 WHERE id = 10`, nil, false}, // onto a taken key
	} {
		_, err := db.Exec(tc.q, tc.args...)
		if tc.ok && err != nil {
			t.Errorf("%s %v: %v", tc.q, tc.args, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s %v: expected an error", tc.q, tc.args)
		}
	}
	st := &State{dbs: map[string]map[string]map[string]Row{"app": store.committed}}
	if r, ok := st.Row(store, "users", 10); !ok || r.Str("name") != "Bob" {
		t.Errorf("the row under its new key: %v %v", r, ok)
	}
	if _, ok := st.Row(store, "users", 2); ok {
		t.Error("the old key must be gone")
	}
}

func TestSchemaOnConflict(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE users (id uuid DEFAULT gen_random_uuid() PRIMARY KEY, email text NOT NULL, visits int DEFAULT 0)`)
	mustExec(t, db, `ALTER TABLE ONLY users ADD CONSTRAINT users_email_key UNIQUE (email)`)
	upsert := `INSERT INTO users (email, visits) VALUES ($1, 1) ON CONFLICT (email) DO UPDATE SET visits = users.visits + 1`
	mustExec(t, db, upsert, "a@x")
	mustExec(t, db, upsert, "a@x")
	mustExec(t, db, `INSERT INTO users (email) VALUES ($1) ON CONFLICT DO NOTHING`, "a@x")
	rows := store.Peek("users")
	if len(rows) != 1 || rows[0].Int64("visits") != 2 {
		t.Fatalf("rows: %v", rows)
	}
	if _, err := db.Exec(`INSERT INTO users (email) VALUES ($1) ON CONFLICT (visits) DO NOTHING`, "b@x"); err == nil || !strings.Contains(err.Error(), "no unique or exclusion constraint") {
		t.Fatalf("conflict target without a constraint: %v", err)
	}
}

// Two requests create a user with the same email at the same time, each with
// an id the application generates. With the unique constraint declared, the
// second insert waits for the first transaction and then conflicts, as in
// Postgres; without it both rows would be committed.
func TestSchemaConcurrentInsertsOfOneUniqueValue(t *testing.T) {
	for _, onConflict := range []bool{true, false} {
		t.Run(fmt.Sprintf("on_conflict=%v", onConflict), func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, store := s.DB("app", postgres.New())
				mustExec(t, db, `CREATE TABLE users (id text PRIMARY KEY, email text UNIQUE)`)
				q := `INSERT INTO users (id, email) VALUES ($1, $2)`
				if onConflict {
					q += ` ON CONFLICT (email) DO NOTHING`
				}
				signup := func(id string) func(p *Proc) error {
					return func(p *Proc) error {
						tx, err := db.BeginTx(p.Context(), nil)
						if err != nil {
							return err
						}
						if _, err := tx.Exec(q, id, "a@x"); err != nil {
							_ = tx.Rollback()
							if errors.Is(err, ErrUniqueViolation) && !onConflict {
								return nil // the request reports the duplicate
							}
							return err
						}
						p.Step("does more work in the transaction")
						return tx.Commit()
					}
				}
				s.Manual("signup_a", 1, signup("u1"))
				s.Manual("signup_b", 1, signup("u2"))
				s.AtQuiescence(func(st *State) error {
					if n := len(st.Rows(store, "users")); n != 1 {
						return fmt.Errorf("%d users with one email", n)
					}
					return nil
				})
			})
		})
	}
}

// A schema dump carries statements detest does not need; they run as no-ops.
func TestSchemaDump(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `
SET statement_timeout = 0;
SELECT pg_catalog.set_config('search_path', '', false);
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
CREATE FUNCTION public.touch() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
CREATE TABLE public.orders (
    id bigint NOT NULL,
    customer_id bigint NOT NULL,
    external_ref text,
    deleted_at timestamp with time zone
);
ALTER TABLE public.orders OWNER TO app;
COMMENT ON TABLE public.orders IS 'orders';
CREATE SEQUENCE public.orders_id_seq START WITH 1 INCREMENT BY 1 NO MINVALUE NO MAXVALUE CACHE 1;
ALTER SEQUENCE public.orders_id_seq OWNED BY public.orders.id;
ALTER TABLE ONLY public.orders ALTER COLUMN id SET DEFAULT nextval('public.orders_id_seq'::regclass);
ALTER TABLE ONLY public.orders ADD CONSTRAINT orders_pkey PRIMARY KEY (id);
CREATE UNIQUE INDEX orders_external_ref_live ON public.orders USING btree (external_ref) WHERE (deleted_at IS NULL);
CREATE INDEX orders_customer ON public.orders USING btree (customer_id);
CREATE TRIGGER orders_touch BEFORE UPDATE ON public.orders FOR EACH ROW EXECUTE FUNCTION public.touch();
GRANT SELECT ON TABLE public.orders TO reader;
`)
	var id int64
	if err := db.QueryRow(`INSERT INTO orders (customer_id, external_ref) VALUES (1, 'r1') RETURNING id`).Scan(&id); err != nil || id != 1 {
		t.Fatalf("id %d, %v", id, err)
	}
	if _, err := db.Exec(`INSERT INTO orders (customer_id, external_ref) VALUES (2, 'r1')`); !errors.Is(err, ErrUniqueViolation) {
		t.Fatalf("partial unique index: %v", err)
	}
	mustExec(t, db, `UPDATE orders SET deleted_at = now() WHERE id = 1`)
	mustExec(t, db, `INSERT INTO orders (customer_id, external_ref) VALUES (2, 'r1')`)
	if _, err := db.Exec(`CREATE TABLE orders (id int)`); err == nil {
		t.Fatal("creating an existing table must fail")
	}
}

func TestSchemaSearchPath(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New(postgres.SearchPath("billing", "public")))
	mustExec(t, db, `CREATE TABLE public.invoices (id text PRIMARY KEY, total int)`)
	mustExec(t, db, `CREATE TABLE billing.invoices (id text PRIMARY KEY, total int)`)
	mustExec(t, db, `INSERT INTO invoices (id, total) VALUES ('i1', 10)`)        // billing, first on the path
	mustExec(t, db, `INSERT INTO public.invoices (id, total) VALUES ('i1', 20)`) // a different table
	mustExec(t, db, `CREATE TABLE audit (id text PRIMARY KEY)`)                  // created in billing
	mustExec(t, db, `INSERT INTO billing.audit (id) VALUES ('a1')`)              //
	st := &State{dbs: map[string]map[string]map[string]Row{"app": store.committed}}
	if r, _ := st.Row(store, "invoices", "i1"); r.Int64("total") != 10 {
		t.Fatalf("unqualified: %v", r)
	}
	if r, _ := st.Row(store, "public.invoices", "i1"); r.Int64("total") != 20 {
		t.Fatalf("public: %v", r)
	}
	var total int64
	if err := db.QueryRow(`SELECT i.total FROM public.invoices i WHERE i.id = $1`, "i1").Scan(&total); err != nil || total != 20 {
		t.Fatalf("qualified select: %d %v", total, err)
	}
	if len(st.Rows(store, "billing.audit")) != 1 {
		t.Fatal("audit was not created in billing")
	}
}

// Migrations evolve a schema: views, renames, dropped constraints, indexes
// and columns, all of which must leave detest's definitions as Postgres's.
func TestSchemaEvolution(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New())
	mustExec(t, db, `
CREATE TABLE exports (id text PRIMARY KEY, workspace_id text, name text, legacy text, CONSTRAINT exports_ws_name_key UNIQUE (workspace_id, name));
CREATE UNIQUE INDEX exports_legacy_idx ON exports (legacy);
INSERT INTO exports (id, workspace_id, name, legacy) VALUES ('e1', 'w1', 'a', 'x');
ALTER TABLE exports RENAME TO exporters;
ALTER TABLE exporters RENAME CONSTRAINT exports_ws_name_key TO exporters_ws_name_key;
ALTER INDEX exports_legacy_idx RENAME TO exporters_legacy_idx;
ALTER TABLE exporters RENAME COLUMN name TO title;
CREATE VIEW exports AS SELECT * FROM exporters;
`)
	if got := rowsOf(t, db, `SELECT id, title FROM exports`); !reflect.DeepEqual(got, []string{"e1,a"}) {
		t.Fatalf("view over the renamed table: %v", got)
	}
	if _, err := db.Exec(`INSERT INTO exporters (id, workspace_id, title) VALUES ('e2', 'w1', 'a')`); !errors.Is(err, ErrUniqueViolation) {
		t.Fatalf("the renamed constraint must still hold on the renamed column: %v", err)
	}
	mustExec(t, db, `
ALTER TABLE exporters DROP CONSTRAINT exporters_ws_name_key;
DROP INDEX exporters_legacy_idx;
DROP VIEW IF EXISTS exports;
ALTER TABLE exporters DROP COLUMN legacy;
`)
	mustExec(t, db, `INSERT INTO exporters (id, workspace_id, title, legacy) VALUES ('e2', 'w1', 'a', 'x')`) // both constraints are gone
	if _, err := db.Query(`SELECT * FROM exports`); err == nil {
		t.Fatal("the dropped view must be gone")
	}
	if n := len(store.Peek("exporters")); n != 2 {
		t.Fatalf("rows: %d", n)
	}
}

// A function in FROM is lateral: it sees the row it joins to.
func TestLateralFunction(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE policies (id text PRIMARY KEY, slots int)`)
	mustExec(t, db, `INSERT INTO policies (id, slots) VALUES ('p1', 2), ('p2', 0), ('p3', 1)`)
	got := rowsOf(t, db, `SELECT p.id, gs - 1 FROM policies p CROSS JOIN LATERAL generate_series(1, p.slots) AS gs ORDER BY p.id, gs`)
	if want := []string{"p1,0", "p1,1", "p3,0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestColumnTypes(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE counters (id uuid PRIMARY KEY, small smallint, n integer, big bigint)`)
	const id = "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	mustExec(t, db, `INSERT INTO counters (id, small, n, big) VALUES ($1, 1, 1, 9223372036854775806)`, id)
	mustExec(t, db, `INSERT INTO counters (id) VALUES ('{A0EEBC999C0B4EF8BB6D6BB9BD380A12}')`)
	for _, tc := range []struct {
		q    string
		args []any
		code string
	}{
		{`INSERT INTO counters (id) VALUES ($1)`, []any{"not-a-uuid"}, "22P02"},
		{`UPDATE counters SET small = 40000 WHERE id = $1`, []any{id}, "22003"},
		{`UPDATE counters SET n = 2147483648 WHERE id = $1`, []any{id}, "22003"},
		{`UPDATE counters SET big = big + 2 WHERE id = $1`, []any{id}, "22003"},
		{`SELECT 1 / 0`, nil, "22012"},
	} {
		_, err := db.Exec(tc.q, tc.args...)
		var se *DBError
		if !errors.As(err, &se) || se.Code != tc.code {
			t.Errorf("%s: %v, want SQLSTATE %s", tc.q, err, tc.code)
		}
	}
	// Integer arithmetic stays exact past float64's precision and truncates.
	if got := rowsOf(t, db, `SELECT big + 1, 7 / 2, -7 / 2, 7 % 3 FROM counters WHERE id = $1`, id); got[0] != "9223372036854775807,3,-3,1" {
		t.Errorf("integer arithmetic: %v", got)
	}
}
