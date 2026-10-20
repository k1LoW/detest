package detest

import (
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// Under PartialOrder, an idle loop is woken only by a change that may change
// what its idle tick decided, rather than by any change of committed state.
// A tick's decision depends only on what it observed, so a change to a table
// it did not read cannot change it, which is the assumption PartialOrder
// already makes: that the processes share no state outside detest. Without
// it, a tick that reads a variable another process sets beside an unrelated
// commit would never be woken, so the rule is not applied there.
//
// The rule wakes a loop at a subset of the changes that wake it otherwise,
// never at another, and falls back to every change where what the tick
// decided may depend on more than the tables it read. Its idle ticks are
// then the only runs it leaves out, which no other process can tell from
// the ones it keeps, as a tick that did anything another sees falls back.

// change is one change of committed state: the process that made it, nil
// for the scheduler, and the steps since the change before it, the events
// from to to of the trace, whose writes are what may have changed. all marks
// a change whose steps are not known, which counts as touching everything.
type change struct {
	by       *Proc
	from, to int
	all      bool
}

// idleTick is an idle tick of a loop, with how far idleWoken has read the
// changes since it started, and whether one of them woke the loop.
type idleTick struct {
	p     *Proc
	next  int
	woken bool
}

// idleOn reports whether the rule applies: under PartialOrder, in a replay
// of a schedule explored under it, and for a test that compares the
// reduction with a full tree under the same rule.
func (r *run) idleOn() bool { return r.s.idleByReads && r.depOn() }

// recordChange records the change bump counts. The version is the index of
// a change plus one, as bump is the only place the version moves.
func (r *run) recordChange(p *Proc) {
	if !r.idleOn() {
		return
	}
	d := r.dep()
	c := change{by: p, from: r.changeFrom, to: len(d.events) - 1}
	if p == nil || r.current != p || d.cur == nil {
		c.all = true
	}
	r.changeFrom = len(d.events)
	r.changes = append(r.changes, c)
}

func (r *run) idleRecord(p *Proc) {
	if !r.idleOn() {
		return
	}
	if p.bumps > 0 {
		p.touchedAll = true // its own change, which others see
	}
	if r.idle == nil {
		r.idle = map[*procType]*idleTick{}
	}
	r.idle[p.pt] = &idleTick{p: p, next: p.started}
}

// idleGated reports whether a loop that went idle may not tick yet, as
// nothing changed that could change what its tick finds. Under the rule that
// is a change by another process, since its last idle tick started, to what
// the tick touched, and otherwise any change since. The last idle tick
// stands for the loop's next one, as idleAt does, since a tick that reads
// what an earlier one read decides as it did.
func (r *run) idleGated(pt *procType) bool {
	if !r.idleOn() {
		v, idle := r.idleAt[pt]
		return idle && v == r.version
	}
	t := r.idle[pt]
	return t != nil && !r.idleWoken(t)
}

func (r *run) idleWoken(t *idleTick) bool {
	for ; !t.woken && t.next < len(r.changes); t.next++ {
		c := r.changes[t.next]
		if c.by != t.p && r.idleOverlaps(t.p, c) {
			t.woken = true
		}
	}
	return t.woken
}

// idleOverlaps reports whether the steps of c other than the tick's wrote
// what the tick touched. A
// write to everything, such as a Step, a crash or the clock moving, which a
// tick reading the time depends on, overlaps anything, and so does an
// enqueue, which wakes every loop as before.
func (r *run) idleOverlaps(tick *Proc, c change) bool {
	if c.all || tick.touchedAll || r.idleChildren {
		return true
	}
	events := r.dep().events
	for i := max(c.from, 0); i <= c.to && i < len(events); i++ {
		if events[i].proc == tick.name {
			continue // its own steps, whose effects made it fall back
		}
		for res, w := range events[i].acc {
			if !w {
				continue
			}
			if res == depAll || strings.HasPrefix(res, "queue:") {
				return true
			}
			for t := range tick.touched {
				if depSameRes(res, t) {
					return true
				}
			}
		}
	}
	return false
}

// idleChild records that a loop's tick started a process, spawned or
// adopted, whose steps are not followed back to the tick. Every idle loop is
// then woken by every change, including the ones idleWoken already read,
// since what the process does may come after them.
func (r *run) idleChild() {
	if !r.idleOn() {
		return
	}
	r.idleChildren = true
	for _, t := range r.idle {
		t.woken = true
	}
}

// idleTouch records that p touched res, when p is a loop tick. A write to
// anything but the tables and rows of a statement, whose own target counts as
// written even when it matches no row, is an effect another process may see,
// such as a sequence drawn, a lock waited for or an External called, and the
// tick then falls back to every change. So is any use of a mutex, as a read
// lock, which the reduction takes as a read, still keeps writers out. Its
// own connection is no such effect.
func (r *run) idleTouch(p *Proc, res string, write bool) {
	if p == nil || !r.idleOn() {
		return
	}
	if p.pt.fromLoop {
		return // idleChild fell back when it was created
	}
	if p.pt.kind != trigLoop {
		return
	}
	if res == depAll || strings.HasPrefix(res, "mutex:") || write && !strings.HasPrefix(res, "db:") && !strings.HasPrefix(res, "txconn:") {
		p.touchedAll = true
		return
	}
	if strings.HasPrefix(res, "txconn:") {
		return
	}
	if p.touched == nil {
		p.touched = map[string]bool{}
	}
	p.touched[res] = true
}

// idleEffect records that p, when a loop tick, did something another
// process sees, such as taking a lock.
func (r *run) idleEffect(p *Proc) {
	if p != nil && r.idleOn() && p.pt.kind == trigLoop {
		p.touchedAll = true
	}
}

// idleTouchAll records a touch outside any step, which may be any running
// loop tick's.
func (r *run) idleTouchAll() {
	if !r.idleOn() {
		return
	}
	for _, p := range r.procs {
		if p.pt.kind == trigLoop && p.state != stateDone {
			p.touchedAll = true
		}
	}
}

// idleReads records for an idle tick the tables a statement may read, where
// the reduction takes the statement as reading everything because the
// evaluation decides which rows it reads. A tick's outcome depends on the
// rows of the tables it names, wherever in the statement, and of the tables
// behind the views it names, so those are what a change has to touch to
// wake it. A function in FROM, a function with an effect, such as a probe of
// an advisory lock, whose outcome depends on state no table holds, and a kind
// of statement other than the four DML ones count as reading everything.
func (x *sqlExec) idleReads(stmt sqlir.Statement) {
	tx := x.tx
	r := tx.p.r
	if !r.idleOn() {
		return
	}
	touch := func(res string) {
		r.idleTouch(tx.p, res, false)
		if r.current != tx.p {
			r.idleTouch(r.current, res, false)
		}
	}
	switch stmt.(type) {
	case *sqlir.SelectStmt, *sqlir.InsertStmt, *sqlir.UpdateStmt, *sqlir.DeleteStmt:
	default:
		touch(depAll)
		return
	}
	seen := map[string]bool{}
	var walk func(n any)
	walk = func(n any) {
		for _, f := range sqlir.FuncCallsIn(n) {
			if effectFuncs[f.Name] {
				touch(depAll)
				return
			}
		}
		for _, t := range sqlir.TableRefs(n) {
			if t.Func != nil {
				touch(depAll)
				return
			}
			if t.Name == "" {
				continue
			}
			name := tx.db.resolve(t.Name)
			if seen[name] {
				continue
			}
			seen[name] = true
			if v := tx.db.views[name]; v != nil && v.View != nil {
				walk(v.View)
				continue
			}
			touch("db:" + tx.db.name + ":" + name)
		}
	}
	walk(stmt)
}
