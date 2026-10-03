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
	name      string
	s         *Sim
	msgs      []*qmsg
	nextID    int
	dupBudget int
	dups      int
	consumers []*procType
}

// QueueOption configures a Queue.
type QueueOption func(*Queue)

// Duplicates lets the explorer deliver up to n messages twice per run.
func Duplicates(n int) QueueOption { return func(q *Queue) { q.dups = n } }

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
func (q *Queue) SeedMsg(msg Msg) { q.push(msg) }

// Enqueue publishes a message immediately (outside a transaction).
func (q *Queue) Enqueue(p *Proc, msg Msg) {
	p.yieldf("%s: enqueue %s", q.name, msg)
	q.push(msg)
}

func (q *Queue) reset() {
	q.msgs = nil
	q.nextID = 0
	q.dupBudget = q.dups
}

func (q *Queue) push(msg Msg) {
	if q.s.run != nil {
		q.s.run.version++
		q.s.run.queuesTouched = true
	}
	q.nextID++
	q.msgs = append(q.msgs, &qmsg{id: q.nextID, msg: msg})
}
