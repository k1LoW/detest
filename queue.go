package detest

import (
	"fmt"
	"sort"
	"strings"
)

// Msg is a queue message.
type Msg map[string]any

// Str returns a field as a string.
func (m Msg) Str(k string) string {
	if v, ok := m[k]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

func (m Msg) String() string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return "{" + strings.Join(parts, " ") + "}"
}

type qmsg struct {
	id          int
	msg         Msg
	redelivered int
	duplicate   bool
}

func (q *qmsg) String() string {
	s := fmt.Sprintf("msg%d%s", q.id, q.msg)
	if q.duplicate {
		s += " (duplicate)"
	}
	return s
}

// Queue is an at-least-once, unordered message queue.
type Queue struct {
	name       string
	s          *Sim
	msgs       []*qmsg
	dropped    []Msg
	nextID     int
	dupBudget  int
	dups       int
	lossBudget int
	losses     int
	consumers  []*procType
}

// QueueOption configures a Queue.
type QueueOption func(*Queue)

// Duplicates lets the explorer deliver up to n messages twice per run.
func Duplicates(n int) QueueOption { return func(q *Queue) { q.dups = n } }

// Losses lets the explorer lose up to n messages per run: a message waiting
// in the queue may vanish instead of being delivered, as one can when a
// broker fails over or a publish is never confirmed. It checks that a
// backstop, such as a sweeper over the database, covers every path a lost
// message would leave unfinished.
func Losses(n int) QueueOption { return func(q *Queue) { q.losses = n } }

// Queue registers a simulated queue.
func (s *Sim) Queue(name string, opts ...QueueOption) *Queue {
	s.declare("Queue")
	q := &Queue{name: name, s: s}
	for _, o := range opts {
		o(q)
	}
	s.queues = append(s.queues, q)
	return q
}

// Name returns the queue name.
func (q *Queue) Name() string { return q.name }

// SeedMsg enqueues a message during Seed.
func (q *Queue) SeedMsg(msg Msg) { q.push(nil, msg) }

// Enqueue publishes a message immediately (outside a transaction).
func (q *Queue) Enqueue(p *Proc, msg Msg) {
	defer q.s.leave()
	defer func() { absorbAbort(recover(), p, nil) }()
	p = p.resolve(func() string { return canonical("enqueue", q.name, msg) })
	goneStale(p)
	p.yieldf("%s: enqueue %s", q.name, msg)
	q.push(p, msg)
}

func (q *Queue) reset() {
	q.msgs = nil
	q.dropped = nil
	q.nextID = 0
	q.dupBudget = q.dups
	q.lossBudget = q.losses
}

// push enqueues msg. p is the process that published it, nil for a seed.
func (q *Queue) push(p *Proc, msg Msg) {
	if q.s.run != nil {
		q.s.run.bump(p)
		q.s.run.queuesTouched = true
	}
	q.nextID++
	q.msgs = append(q.msgs, &qmsg{id: q.nextID, msg: msg})
}
