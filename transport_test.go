package detest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/k1LoW/detest/db/postgres"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// cancelOrderHandler is another service's real handler: it cancels the order
// in its own transaction on the database both services share.
func cancelOrderHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/orders/")
		if _, err := db.ExecContext(r.Context(), `UPDATE "orders" SET "status"=$1 WHERE "id" = $2`, "CANCELED", id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "canceled")
	}
}

// An HTTP call through External.Transport has the three outcomes of
// External.Do: the response lost after the handler committed (FailAfter)
// leaves the order canceled although the caller saw an error.
func TestTransportOutcomes(t *testing.T) {
	var runs, callerSawError, canceled int // accumulated across runs
	Explore(t, func(t *testing.T, s *Sim) {
		orderDB, db := s.DB("shared", postgres.New())
		svc := s.External("OrderService")
		client := &http.Client{Transport: svc.Transport(cancelOrderHandler(db.Open()))}
		s.Seed(func() {
			runs++
			_, _ = orderDB.Exec(`INSERT INTO "orders" ("id","status") VALUES ($1,$2)`, "o1", "PENDING")
		})
		s.Manual("caller", 1, func(p *Proc) error {
			req, _ := http.NewRequestWithContext(p.Context(), http.MethodPost, "http://orders.internal/orders/o1", nil)
			resp, err := client.Do(req)
			if err != nil {
				if !errors.Is(err, ErrUnavailable) {
					return err
				}
				callerSawError++
				return nil
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK || string(body) != "canceled" {
				return fmt.Errorf("response %d %q", resp.StatusCode, body)
			}
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			if row, _ := st.Row(db, "orders", "o1"); row.Str("status") == "CANCELED" {
				canceled++
			}
			return nil
		})
	}, MaxFailures(1))
	if runs != 3 || callerSawError != 2 || canceled != 2 {
		t.Fatalf("runs=%d callerSawError=%d canceled=%d", runs, callerSawError, canceled)
	}
}

// A connect client calls the real connect handler in-process. The handler's
// error reaches the caller as a connect error; a transport failure reaches it
// as CodeUnavailable.
func TestTransportConnect(t *testing.T) {
	const procedure = "/inventory.v1.InventoryService/Reserve"
	var codes []string
	Explore(t, func(t *testing.T, s *Sim) {
		invDB, db := s.DB("inventory", postgres.New())
		svc := s.External("InventoryService")
		mux := http.NewServeMux()
		mux.Handle(procedure, connect.NewUnaryHandler(procedure, func(ctx context.Context, req *connect.Request[wrapperspb.StringValue]) (*connect.Response[wrapperspb.Int64Value], error) {
			res, err := invDB.ExecContext(ctx, `UPDATE "stock" SET "n" = "n" - 1 WHERE "id" = $1 AND "n" > 0`, req.Msg.GetValue())
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			if k, _ := res.RowsAffected(); k == 0 {
				return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("out of stock"))
			}
			return connect.NewResponse(wrapperspb.Int64(1)), nil
		}))
		client := connect.NewClient[wrapperspb.StringValue, wrapperspb.Int64Value](&http.Client{Transport: svc.Transport(mux)}, "http://inventory.internal"+procedure)
		s.Seed(func() {
			_, _ = invDB.Exec(`INSERT INTO "stock" ("id","n") VALUES ($1,$2)`, "sku1", int64(1))
		})
		reserve := func(p *Proc) error {
			_, err := client.CallUnary(p.Context(), connect.NewRequest(wrapperspb.String("sku1")))
			if err == nil {
				codes = append(codes, "ok")
			} else {
				codes = append(codes, connect.CodeOf(err).String())
			}
			return nil
		}
		s.Manual("reserve_a", 1, reserve)
		s.Manual("reserve_b", 1, reserve)
		s.AtQuiescence(func(st *State) error {
			if row, _ := st.Row(db, "stock", "sku1"); row.Int64("n") < 0 {
				return fmt.Errorf("stock went negative: %d", row.Int64("n"))
			}
			return nil
		})
	}, MaxFailures(1))
	seen := strings.Join(codes, " ")
	for _, want := range []string{"ok", "failed_precondition", "unavailable"} {
		if !strings.Contains(seen, want) {
			t.Errorf("no call ended %s: %s", want, seen)
		}
	}
}
