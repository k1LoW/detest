package detest

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"slices"
	"testing"

	"github.com/lib/pq"

	"github.com/k1LoW/detest/mysql"
	"github.com/k1LoW/detest/postgres"
)

func arrayDB(t *testing.T) *sql.DB {
	t.Helper()
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id bigint PRIMARY KEY, name text, n int, u uuid)`)
	mustExec(t, db, `INSERT INTO t VALUES
		(1, 'a', 10, '00000000-0000-0000-0000-000000000001'),
		(2, 'b', NULL, '00000000-0000-0000-0000-00000000000a'),
		(3, 'a b', 30, NULL)`)
	return db
}

func queryIDs(t *testing.T, db *sql.DB, q string, args ...any) []int64 {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return ids
}

type uuidValuer string

func (u uuidValuer) Value() (driver.Value, error) { return string(u), nil }

// x = ANY ($1) and x <> ALL ($1) match the rows Postgres matches, for the
// array a Go slice or pq.Array binds, and for array text written in the
// statement.
func TestArrayComparison(t *testing.T) {
	db := arrayDB(t)
	one := int64(1)
	tests := []struct {
		q    string
		args []any
		want []int64
	}{
		{`SELECT id FROM t WHERE id = ANY($1) ORDER BY id`, []any{[]int64{1, 3, 9}}, []int64{1, 3}},
		{`SELECT id FROM t WHERE id <> ALL($1) ORDER BY id`, []any{[]int64{1, 3}}, []int64{2}},
		{`SELECT id FROM t WHERE id = ANY($1) ORDER BY id`, []any{[]int{2}}, []int64{2}},
		{`SELECT id FROM t WHERE id = ANY($1) ORDER BY id`, []any{pq.Array([]int64{1, 2})}, []int64{1, 2}},
		{`SELECT id FROM t WHERE name = ANY($1) ORDER BY id`, []any{pq.Array([]string{"a b", "b"})}, []int64{2, 3}},
		{`SELECT id FROM t WHERE name = ANY($1) ORDER BY id`, []any{[]string{"a"}}, []int64{1}},
		{`SELECT id FROM t WHERE id = ANY($1::bigint[]) ORDER BY id`, []any{[]int64{2, 3}}, []int64{2, 3}},
		{`SELECT id FROM t WHERE n = ANY($1::int[]) ORDER BY id`, []any{pq.Array([]int64{10, 30})}, []int64{1, 3}},
		{`SELECT id FROM t WHERE u = ANY($1::uuid[]) ORDER BY id`, []any{[]uuidValuer{"00000000-0000-0000-0000-000000000001"}}, []int64{1}},
		{`SELECT id FROM t WHERE name = ANY($1::text[]) ORDER BY id`, []any{[]string{"b"}}, []int64{2}},
		// The empty array decides it whatever x is, and a NULL one or a NULL
		// element without a match makes it NULL.
		{`SELECT id FROM t WHERE id = ANY($1) ORDER BY id`, []any{[]int64{}}, []int64{}},
		{`SELECT id FROM t WHERE n <> ALL($1) ORDER BY id`, []any{[]int64{}}, []int64{1, 2, 3}},
		{`SELECT id FROM t WHERE id = ANY($1) ORDER BY id`, []any{[]int64(nil)}, []int64{}},
		{`SELECT id FROM t WHERE id <> ALL($1) ORDER BY id`, []any{[]int64(nil)}, []int64{}},
		{`SELECT id FROM t WHERE (id = ANY($1)) IS NULL ORDER BY id`, []any{[]*int64{&one, nil}}, []int64{2, 3}},
		{`SELECT id FROM t WHERE (n = ANY($1)) IS NULL ORDER BY id`, []any{[]int64{10}}, []int64{2}},
		{`SELECT id FROM t WHERE (id <> ALL($1)) IS NULL ORDER BY id`, []any{[]*int64{&one, nil}}, []int64{2, 3}},
		// Array text: space around unquoted elements is dropped, quoted ones
		// keep it, a backslash escapes, and only an unquoted NULL is NULL.
		{`SELECT id FROM t WHERE id = ANY('{ 1 , 2 }') ORDER BY id`, nil, []int64{1, 2}},
		{`SELECT id FROM t WHERE name = ANY('{ a b }') ORDER BY id`, nil, []int64{3}},
		{`SELECT id FROM t WHERE name = ANY('{a\ b}') ORDER BY id`, nil, []int64{3}},
		{`SELECT id FROM t WHERE name = ANY('{"a b", "b"}') ORDER BY id`, nil, []int64{2, 3}},
		{`SELECT id FROM t WHERE (name = ANY('{"NULL",null}')) IS NULL ORDER BY id`, nil, []int64{1, 2, 3}},
		{`SELECT id FROM t WHERE id = ANY(' { } ') ORDER BY id`, nil, []int64{}},
		{`SELECT id FROM t WHERE id = ANY('{01}') ORDER BY id`, nil, []int64{1}},
		{`SELECT id FROM t WHERE id = ANY('{1,3}'::bigint[]) ORDER BY id`, nil, []int64{1, 3}},
	}
	for _, tt := range tests {
		if got := queryIDs(t, db, tt.q, tt.args...); !slices.Equal(got, tt.want) {
			t.Errorf("%s %v: got %v, want %v", tt.q, tt.args, got, tt.want)
		}
	}
	res, err := db.Exec(`UPDATE t SET n = 0 WHERE id = ANY($1)`, []int64{1, 2, 9})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Errorf("UPDATE ... WHERE id = ANY($1): affected %d rows, want 2", n)
	}
}

// Postgres reads the array before the statement runs, so a malformed one, or
// an element its cast refuses, fails it on an empty table too.
func TestArrayReadBeforeTheStatement(t *testing.T) {
	db := arrayDB(t)
	mustExec(t, db, `CREATE TABLE empty (id bigint PRIMARY KEY)`)
	for _, tt := range []struct {
		q    string
		args []any
	}{
		{`SELECT id FROM empty WHERE id = ANY('1,2')`, nil},
		{`SELECT id FROM empty WHERE id = ANY('{1,2')`, nil},
		{`SELECT id FROM empty WHERE id = ANY('{1,,2}')`, nil},
		{`SELECT id FROM empty WHERE id = ANY('{1,}')`, nil},
		{`SELECT id FROM empty WHERE id = ANY('{1,"2"x}')`, nil},
		{`SELECT id FROM empty WHERE id = ANY('{a"b"}')`, nil},
		{`SELECT id FROM empty WHERE id = ANY('{}x')`, nil},
		{`SELECT id FROM empty WHERE id = ANY('{"1"')`, nil},
		{`SELECT id FROM empty WHERE id = ANY('{"1" ')`, nil},
		{`SELECT id FROM empty WHERE id = ANY('{"1')`, nil},
		{`SELECT id FROM empty WHERE id = ANY($1)`, []any{"1,2"}},
		{`SELECT id FROM empty WHERE id = ANY('{abc}'::bigint[])`, nil},
		{`SELECT id FROM empty WHERE id = ANY($1::bigint[])`, []any{pq.Array([]string{"abc"})}},
	} {
		if _, err := db.Exec(tt.q, tt.args...); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%s: got %v, want 22P02", tt.q, err)
		}
	}
	// An element that does not read as x's type fails though another matches.
	if _, err := db.Exec(`SELECT id FROM t WHERE id = ANY('{1,abc}')`); !errors.Is(err, ErrInvalidTextRepresentation) {
		t.Errorf("an element that is not a bigint: got %v, want 22P02", err)
	}
}

func TestArrayComparisonUnsupported(t *testing.T) {
	db := arrayDB(t)
	for _, tt := range []struct {
		q    string
		args []any
	}{
		{`SELECT id FROM t WHERE id = ANY('{{1,2},{3}}')`, nil},
		{`SELECT id FROM t WHERE id = ANY('[0:1]={1,2}')`, nil},
		{`SELECT id FROM t WHERE id = ANY($1::bigint[])`, []any{[]float64{1.5}}},
		{`SELECT id FROM t WHERE id = ANY($1::bigint[])`, []any{[]string{"1"}}},
		{`SELECT id FROM t WHERE id = ANY($1)`, []any{int64(1)}},
		{`INSERT INTO t (id, name) VALUES (4, $1)`, []any{[]string{"a"}}},
		{`SELECT id FROM t WHERE id = $1`, []any{[]int64{1}}},
	} {
		if _, err := db.Exec(tt.q, tt.args...); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s %v: got %v, want ErrUnsupportedSQL", tt.q, tt.args, err)
		}
	}
	for _, q := range []string{
		`SELECT $1 = ANY($2)`,
		`SELECT id FROM t WHERE id = ANY($1::numeric[])`,
		`SELECT id FROM t WHERE id = ANY($1::varchar(3)[])`,
		`SELECT id FROM t WHERE id = ANY($1::bigint)`,
		`SELECT id FROM t WHERE id < ANY($1)`,
		`SELECT id FROM t WHERE id = ALL($1)`,
	} {
		if err := CheckSQL(postgres.New(), q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	for _, q := range []string{
		`SELECT id FROM t WHERE id = ANY($1)`,
		`SELECT id FROM t WHERE id <> ALL($1::bigint[])`,
		`UPDATE t SET n = 0 WHERE id = ANY($1) RETURNING id`,
		`SELECT id FROM t WHERE u = ANY('{00000000-0000-0000-0000-000000000001}'::uuid[])`,
	} {
		if err := CheckSQL(postgres.New(), q); err != nil {
			t.Errorf("%s: got %v, want nil", q, err)
		}
	}
}

// A Go slice is kept for Postgres only. go-sql-driver refuses one, so
// database/sql's conversion refuses it for MySQL as before.
func TestArrayParameterOnMySQL(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysql.New())
	mustExec(t, db, `CREATE TABLE t (id bigint PRIMARY KEY)`)
	_, err := db.Exec(`SELECT id FROM t WHERE id = ?`, []int64{1})
	if err == nil || errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a slice bound for MySQL: got %v, want database/sql's conversion error", err)
	}
}
