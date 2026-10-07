package ddl_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/k1LoW/detest"
	"github.com/k1LoW/detest/db/ddl"
	"github.com/k1LoW/detest/db/postgres"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// TestFromPostgres runs Postgres in a container, so it needs Docker.
func TestFromPostgres(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:18", tcpostgres.BasicWaitStrategies())
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatal(err)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	real, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer real.Close()
	for _, q := range []string{
		`CREATE SCHEMA billing`,
		`CREATE TABLE public.users (id bigserial PRIMARY KEY, email text NOT NULL UNIQUE, tags text[] DEFAULT ARRAY[]::text[], created_at timestamptz DEFAULT now())`,
		`CREATE UNIQUE INDEX users_lower_email ON public.users (lower(email))`,
		`CREATE TABLE public.orders (tenant_id text, id uuid DEFAULT gen_random_uuid(), ref text, deleted_at timestamptz, PRIMARY KEY (tenant_id, id))`,
		`CREATE UNIQUE INDEX orders_ref_live ON public.orders (tenant_id, ref) WHERE deleted_at IS NULL`,
		`CREATE TABLE billing.invoices (id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY, total int, doubled int GENERATED ALWAYS AS (total * 2) STORED)`,
		`CREATE TABLE public.codes (code text COLLATE "C" PRIMARY KEY, label text)`,
	} {
		if _, err := real.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	schema, err := ddl.From(ctx, real, postgres.New(), ddl.Schemas("public", "billing"))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(schema)
	if strings.Contains(schema, "label text COLLATE") {
		t.Error("DDL writes the database's collation on a column that declares none")
	}
	// A column's own collation is written, and one that is the database's is
	// not.
	for _, want := range []string{"public.users", "public.orders", "billing.invoices", "users_lower_email", "orders_ref_live", "PRIMARY KEY (tenant_id, id)", `code text COLLATE pg_catalog."C"`} {
		if !strings.Contains(schema, want) {
			t.Errorf("DDL lacks %q", want)
		}
	}
	only, err := ddl.From(ctx, real, postgres.New(), ddl.Tables("users"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(only, "public.users") || strings.Contains(only, "orders") {
		t.Errorf("Tables(users):\n%s", only)
	}

	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		db, _ := s.DB("app", postgres.New())
		exec := func(q string, args ...any) error {
			_, err := db.Exec(q, args...)
			return err
		}
		if err := exec(schema); err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := db.QueryRow(`INSERT INTO users (email) VALUES ($1) RETURNING id`, "a@x").Scan(&id); err != nil || id != 1 {
			t.Fatalf("bigserial: %d %v", id, err)
		}
		if err := exec(`INSERT INTO users (email) VALUES ($1)`, "A@X"); !errors.Is(err, detest.ErrUniqueViolation) {
			t.Errorf("expression index: %v", err)
		}
		if err := exec(`INSERT INTO orders (tenant_id, ref) VALUES ('t1', 'r1')`); err != nil {
			t.Errorf("uuid default on a composite key: %v", err)
		}
		if err := exec(`INSERT INTO orders (tenant_id, ref) VALUES ('t1', 'r1')`); !errors.Is(err, detest.ErrUniqueViolation) {
			t.Errorf("partial unique index: %v", err)
		}
		if err := exec(`INSERT INTO orders (tenant_id, ref) VALUES ('t2', 'r1')`); err != nil {
			t.Errorf("another tenant: %v", err)
		}
		var doubled int64
		if err := db.QueryRow(`INSERT INTO billing.invoices (total) VALUES (10) RETURNING id, doubled`).Scan(&id, &doubled); err != nil || id != 1 || doubled != 20 {
			t.Errorf("identity and generated column: %d %d %v", id, doubled, err)
		}
	})
}
