package difftest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k1LoW/detest"
)

// Run runs c on detest and on the real server behind b, and fails t when
// the outcomes differ. After each step both sides wait until every statement
// in flight has finished or waits for a lock, so a step marked as waited
// waited on both.
//
// detest explores every interleaving the steps leave open. They must all end
// the same way as the real server, since an outcome the real server never
// gives is one an application test would report wrongly.
func Run(t *testing.T, b Backend, c Case) {
	t.Helper()
	want := runReal(t, b, c)
	if t.Failed() {
		return
	}
	got := runDetest(t, b, c)
	if t.Failed() {
		t.Logf("the real server gives\n%s", want)
		return
	}
	if len(got) == 1 && got[0] == want {
		return
	}
	if c.Racy && slices.Contains(got, want) {
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "detest differs from the real server\n--- real\n%s", want)
	for i, g := range got {
		if g == want {
			continue
		}
		fmt.Fprintf(&sb, "--- detest (%d of %d outcomes)\n%s", i+1, len(got), g)
	}
	t.Error(sb.String())
}

type stepResult struct {
	text    string
	blocked bool
}

func transcript(c Case, res []stepResult) string {
	var sb strings.Builder
	for k, st := range c.Steps {
		waited := ""
		if res[k].blocked {
			waited = "(waited) "
		}
		fmt.Fprintf(&sb, "%2d conn%d %s\n     => %s%s\n", k, st.Conn, st.SQL, waited, res[k].text)
	}
	return sb.String()
}

// session is one connection with the transaction it has open.
type session struct {
	conn *sql.Conn
	tx   *sql.Tx
}

func (s *session) run(ctx context.Context, st Step, outcome func(error) (string, bool)) string {
	text, err := s.exec(ctx, st)
	if err == nil {
		return text
	}
	if o, ok := outcome(err); ok {
		if st.kind() == kindCommit && o == "ERROR 25P02" {
			// Postgres answers COMMIT of a failed transaction with ROLLBACK and
			// no error, which drivers turn into errors of their own.
			return "COMMIT ROLLED BACK"
		}
		return o
	}
	return "UNEXPECTED " + err.Error()
}

func (s *session) exec(ctx context.Context, st Step) (string, error) {
	switch st.kind() {
	case kindBegin:
		opts, err := txOptions(st.SQL)
		if err != nil {
			return "", err
		}
		tx, err := s.conn.BeginTx(ctx, opts)
		if err != nil {
			return "", err
		}
		s.tx = tx
		return "OK", nil
	case kindCommit, kindRollback:
		if s.tx == nil {
			return "", errors.New("difftest: no transaction is open")
		}
		tx := s.tx
		s.tx = nil
		if st.kind() == kindCommit {
			return "OK", tx.Commit()
		}
		return "OK", tx.Rollback()
	case kindQuery:
		var rows *sql.Rows
		var err error
		if s.tx != nil {
			rows, err = s.tx.QueryContext(ctx, st.SQL)
		} else {
			rows, err = s.conn.QueryContext(ctx, st.SQL)
		}
		if err != nil {
			return "", err
		}
		defer rows.Close()
		return renderRows(rows)
	}
	var res sql.Result
	var err error
	if s.tx != nil {
		res, err = s.tx.ExecContext(ctx, st.SQL)
	} else {
		res, err = s.conn.ExecContext(ctx, st.SQL)
	}
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("OK %d", n), nil
}

func txOptions(q string) (*sql.TxOptions, error) {
	level := strings.TrimSpace(strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(q)), "BEGIN"))
	switch level {
	case "", "ISOLATION LEVEL READ COMMITTED":
		return &sql.TxOptions{Isolation: sql.LevelReadCommitted}, nil
	case "ISOLATION LEVEL REPEATABLE READ":
		return &sql.TxOptions{Isolation: sql.LevelRepeatableRead}, nil
	case "ISOLATION LEVEL SERIALIZABLE":
		return &sql.TxOptions{Isolation: sql.LevelSerializable}, nil
	}
	return nil, fmt.Errorf("difftest: unknown %q", q)
}

func renderRows(rows *sql.Rows) (string, error) {
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		fields := make([]string, len(vals))
		for i, v := range vals {
			fields[i] = renderValue(v)
		}
		out = append(out, "("+strings.Join(fields, ", ")+")")
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return fmt.Sprintf("ROWS %d [%s]", len(out), strings.Join(out, " ")), nil
}

func renderValue(v any) string {
	switch v := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return strconv.Quote(string(v))
	case string:
		return strconv.Quote(v)
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	case float32:
		return strconv.FormatFloat(float64(v), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	return fmt.Sprint(v)
}

func detestOutcome(err error) (string, bool) {
	if e, ok := errors.AsType[*detest.DBError](err); ok && e.Code != "" {
		return "ERROR " + e.Code, true
	}
	return "", false
}

func runDetest(t *testing.T, b Backend, c Case) []string {
	t.Helper()
	var (
		mu       sync.Mutex
		seen     = map[string]bool{}
		outcomes []string
	)
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		db, _ := s.DB("app", b.Server())
		for _, q := range c.Schema {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("detest schema: %s: %v", q, err)
			}
		}
		var (
			res  []stepResult
			done []bool
			cmds []chan int
		)
		s.Seed(func() {
			for _, q := range c.Seed {
				if _, err := db.Exec(q); err != nil {
					t.Errorf("detest seed: %s: %v", q, err)
				}
			}
			res = make([]stepResult, len(c.Steps))
			done = make([]bool, len(c.Steps))
			cmds = make([]chan int, c.Conns)
			for i := range cmds {
				cmds[i] = make(chan int, 1)
			}
		})
		for i := range c.Conns {
			s.Manual(fmt.Sprintf("conn%d", i), 1, func(p *detest.Proc) error {
				conn, err := db.Conn(p.Context())
				if err != nil {
					return err
				}
				defer conn.Close()
				se := &session{conn: conn}
				for {
					// A run that ends early cancels the context, and the process
					// has to return for the run to unwind.
					select {
					case k, ok := <-cmds[i]:
						if !ok {
							return nil
						}
						res[k].text = se.run(p.Context(), c.Steps[k], detestOutcome)
						done[k] = true
					case <-p.Context().Done():
						return nil
					}
				}
			})
		}
		// The driver issues the steps in order. WaitUntil returns only once no
		// other process can run, which is when every statement in flight has
		// finished or waits for a lock.
		s.Manual("driver", 1, func(p *detest.Proc) error {
			last := make([]int, c.Conns)
			for i := range last {
				last[i] = -1
			}
			for k, st := range c.Steps {
				if j := last[st.Conn]; j >= 0 && !done[j] {
					return fmt.Errorf("step %d: conn%d still waits in step %d", k, st.Conn, j)
				}
				last[st.Conn] = k
				cmds[st.Conn] <- k
				p.WaitUntil(p.Now() + 1)
				res[k].blocked = !done[k]
			}
			for _, ch := range cmds {
				close(ch)
			}
			return nil
		})
		s.AtQuiescence(func(*detest.State) error {
			tr := transcript(c, res)
			mu.Lock()
			defer mu.Unlock()
			if !seen[tr] {
				seen[tr] = true
				outcomes = append(outcomes, tr)
			}
			return nil
		})
	})
	return outcomes
}

func runReal(t *testing.T, b Backend, c Case) string {
	t.Helper()
	ctx := t.Context()
	db := b.Open(t)
	for _, q := range append(append([]string{}, c.Schema...), c.Seed...) {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("real setup: %s: %v", q, err)
		}
	}

	results := make(chan stepDone, len(c.Steps))
	cmds := make([]chan int, c.Conns)
	ids := make([]int64, c.Conns)
	var wg sync.WaitGroup
	for i := range c.Conns {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ids[i], err = b.SessionID(ctx, conn); err != nil {
			t.Fatal(err)
		}
		cmds[i] = make(chan int, 1)
		wg.Go(func() {
			defer conn.Close()
			se := &session{conn: conn}
			for k := range cmds[i] {
				results <- stepDone{k, se.run(ctx, c.Steps[k], b.Outcome)}
			}
			if se.tx != nil {
				_ = se.tx.Rollback()
			}
		})
	}
	defer func() {
		for _, ch := range cmds {
			close(ch)
		}
		wg.Wait()
	}()

	res := make([]stepResult, len(c.Steps))
	pending := map[int]int{} // step -> conn
	last := make([]int, c.Conns)
	for i := range last {
		last[i] = -1
	}
	for k, st := range c.Steps {
		if j := last[st.Conn]; j >= 0 {
			if _, ok := pending[j]; ok {
				t.Fatalf("step %d: conn%d still waits in step %d", k, st.Conn, j)
			}
		}
		last[st.Conn] = k
		time.Sleep(st.Pause)
		pending[k] = st.Conn
		cmds[st.Conn] <- k
		if err := settle(ctx, b, db, ids, pending, results, res); err != nil {
			t.Fatalf("step %d: %v", k, err)
		}
		_, res[k].blocked = pending[k]
	}
	if len(pending) > 0 {
		t.Fatalf("steps still wait at the end: %v", pending)
	}
	return transcript(c, res)
}

type stepDone struct {
	k    int
	text string
}

// settle waits until every pending statement has finished or waits for a
// lock held by a session that is not part of a deadlock. A deadlock is left
// to the server to break, so that its choice of victim is what the step
// reports.
func settle(ctx context.Context, b Backend, db *sql.DB, ids []int64, pending map[int]int, results <-chan stepDone, res []stepResult) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
	drain:
		for {
			select {
			case d := <-results:
				res[d.k].text = d.text
				delete(pending, d.k)
			default:
				break drain
			}
		}
		if len(pending) == 0 {
			return nil
		}
		waiting := make([]int64, 0, len(pending))
		for _, conn := range pending {
			waiting = append(waiting, ids[conn])
		}
		blockers, err := b.Blockers(ctx, db, waiting)
		if err != nil {
			return err
		}
		settled := true
		for _, id := range waiting {
			if len(blockers[id]) == 0 {
				settled = false
				break
			}
		}
		if settled && !hasCycle(blockers) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("statements did not settle: %v", pending)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func hasCycle(g map[int64][]int64) bool {
	const (
		unseen = iota
		onPath
		finished
	)
	state := map[int64]int{}
	var visit func(int64) bool
	visit = func(n int64) bool {
		switch state[n] {
		case onPath:
			return true
		case finished:
			return false
		}
		state[n] = onPath
		if slices.ContainsFunc(g[n], visit) {
			return true
		}
		state[n] = finished
		return false
	}
	for n := range g {
		if visit(n) {
			return true
		}
	}
	return false
}
