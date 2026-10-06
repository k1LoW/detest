package detest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
)

// Outcome of an external call chosen by the explorer.
type Outcome int

const (
	// Success: the effect is applied and the call returns the effect's result.
	Success Outcome = iota
	// FailBefore: nothing happened and the caller sees ErrUnavailable.
	FailBefore
	// FailAfter: the effect was applied but the response was lost; the caller
	// sees ErrUnavailable.
	FailAfter
)

func (o Outcome) String() string {
	switch o {
	case Success:
		return "success"
	case FailBefore:
		return "fail before effect"
	default:
		return "fail after effect"
	}
}

// External is a declared external service call.
type External struct {
	name     string
	s        *Sim
	outcomes []Outcome
}

// ExternalOption configures an External.
type ExternalOption func(*External)

// Failures sets which failure outcomes the explorer tries (default: FailBefore
// and FailAfter).
func Failures(o ...Outcome) ExternalOption {
	return func(e *External) { e.outcomes = append([]Outcome{Success}, o...) }
}

// ReadOnly limits failures to FailBefore, since a read has no effect to lose.
func ReadOnly() ExternalOption { return Failures(FailBefore) }

// External declares an external call whose effect is modeled by hand.
func (s *Sim) External(name string, opts ...ExternalOption) *External {
	s.declare("External")
	e := &External{name: name, s: s, outcomes: []Outcome{Success, FailBefore, FailAfter}}
	for _, o := range opts {
		o(e)
	}
	s.externals = append(s.externals, e)
	return e
}

// Call performs the external call. effect runs atomically in a transaction on
// db (the remote service's own database); the explorer picks the outcome. The
// effect's returned error is an application error (NotFound, FailedPrecondition)
// and rolls the effect back; ErrUnavailable is a transport failure.
func (e *External) Call(p *Proc, db *DB, desc string, effect func(tx *Tx) error) (err error) {
	defer e.s.leave()
	defer func() { absorbAbort(recover(), p, &err) }()
	p = p.resolve(func() string { return e.name + " " + desc })
	if p.stale() {
		return errRunOver
	}
	n := len(e.outcomes)
	if p.r.failures >= p.r.s.maxFailures {
		n = 1
	}
	i := 0
	if n > 1 {
		i = p.r.choose("outcome of "+e.name, n)
	}
	out := e.outcomes[i]
	if out != Success {
		p.r.failures++
	}
	p.yieldf("calls %s(%s)", e.name, desc)
	if out == FailBefore {
		p.r.note(p, "%s(%s): %s", e.name, desc, out)
		return ErrUnavailable
	}
	tx := db.newTx(p)
	tx.atomic = true
	err = effect(tx)
	if err != nil {
		tx.rollback()
		p.r.note(p, "%s(%s): %s, effect rejected: %v", e.name, desc, out, err)
		return err
	}
	tx.commit()
	p.r.note(p, "%s(%s): %s, effect applied", e.name, desc, out)
	if out == FailAfter {
		return ErrUnavailable
	}
	return nil
}

// Do performs an external call whose effect is an arbitrary Go function, such
// as a real handler of another service invoked in-process. The explorer picks
// the outcome: Success runs call and returns its result; FailBefore returns
// ErrUnavailable without running it; FailAfter runs it (its effects, including
// SQL issued through detest's driver, happen and yield as usual) and then
// returns ErrUnavailable, modeling a response lost after the callee committed.
func (e *External) Do(p *Proc, desc string, call func() error) (err error) {
	defer e.s.leave()
	defer func() { absorbAbort(recover(), p, &err) }()
	p = p.resolve(func() string { return e.name + " " + desc })
	if p.stale() {
		return errRunOver
	}
	n := len(e.outcomes)
	if p.r.failures >= p.r.s.maxFailures {
		n = 1
	}
	i := 0
	if n > 1 {
		i = p.r.choose("outcome of "+e.name, n)
	}
	out := e.outcomes[i]
	if out != Success {
		p.r.failures++
	}
	p.yieldf("calls %s(%s)", e.name, desc)
	if out == FailBefore {
		p.r.note(p, "%s(%s): %s", e.name, desc, out)
		return ErrUnavailable
	}
	err = call()
	switch {
	case err != nil:
		p.r.note(p, "%s(%s): %s, callee returned: %v", e.name, desc, out, err)
	default:
		p.r.note(p, "%s(%s): %s, callee returned ok", e.name, desc, out)
	}
	if out == FailAfter {
		return ErrUnavailable
	}
	return err
}

// Transport returns an http.RoundTripper that serves each request with h in
// the calling process, the in-process way to use a generated HTTP client
// (connect, gRPC-Web, a REST client) against the real handler of another
// service. The explorer picks each request's outcome as Do does: FailBefore
// returns ErrUnavailable without calling h, FailAfter calls h and then loses
// its response. The response is buffered before it is returned, so streaming
// works only from server to client.
func (e *External) Transport(h http.Handler) http.RoundTripper {
	return &transport{e: e, h: h}
}

type transport struct {
	e *External
	h http.Handler
}

func (t *transport) RoundTrip(req *http.Request) (_ *http.Response, err error) {
	p := t.e.s.currentAs(func() string { return requestKey(req) })
	if p == nil {
		return t.serve(req), nil // outside any process, such as in a seed
	}
	if p.stale() {
		return nil, errRunOver
	}
	var resp *http.Response
	err = t.e.Do(p, req.Method+" "+req.URL.Path, func() error {
		resp = t.serve(req)
		if resp.StatusCode >= 400 {
			return fmt.Errorf("%s", resp.Status)
		}
		return nil
	})
	if errors.Is(err, ErrUnavailable) {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	if resp == nil {
		// The call ended without a response, such as errRunOver when the
		// run ended while it waited, which an http.RoundTripper must return
		// rather than a nil response with no error.
		return nil, err
	}
	return resp, nil
}

// serve runs h on a server-side copy of the request and records its response.
func (t *transport) serve(req *http.Request) *http.Response {
	sreq := req.Clone(req.Context())
	sreq.RequestURI = req.URL.RequestURI()
	if sreq.Body == nil {
		sreq.Body = http.NoBody
	}
	if sreq.RemoteAddr == "" {
		sreq.RemoteAddr = "192.0.2.1:1234" // TEST-NET-1, as httptest uses
	}
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, sreq)
	resp := rec.Result()
	resp.Request = req
	return resp
}

// requestKey describes a request for ordering goroutines adopted at it, by
// its method, URL and body, which tells apart the requests goroutines of a
// fan-out send for different items. The body is read only from the copy
// GetBody gives, as connect and http.NewRequest set it for a buffered body,
// since reading the body itself could block on a stream while the scheduler
// does not yet see the goroutine. connect's GetBody rewinds and returns the
// body the request itself reads, so the request is handed a fresh copy
// afterwards, which leaves it as it was either way.
func requestKey(req *http.Request) string {
	key := req.Method + " " + req.URL.String()
	if req.GetBody == nil {
		return key
	}
	body, err := req.GetBody()
	if err != nil {
		return key
	}
	b, err := io.ReadAll(body)
	_ = body.Close()
	if fresh, ferr := req.GetBody(); ferr == nil {
		req.Body = fresh
	} else if err == nil {
		// GetBody cannot give it again, such as one whose Close ended it, so
		// the request is handed what was read.
		req.Body = io.NopCloser(bytes.NewReader(b))
	}
	if err != nil {
		return key
	}
	return key + " " + string(b)
}
