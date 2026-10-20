package detest

import "github.com/k1LoW/detest/internal/sqlir"

// Under PartialOrder, an idle loop is woken only by a change to what its
// idle tick touched, rather than by any change of committed state. A tick's
// decision depends only on what it observed, so a change to a table it did
// not read cannot change it, which is the assumption PartialOrder already
// makes: that the processes share no state outside detest. Without it, a
// tick that reads a variable another process sets beside an unrelated commit
// would never be woken, so the rule is not applied there.

// change is one change of committed state: the process that made it, nil
// for the scheduler, and the step it was made in, nil when the step is not
// known, which then counts as a change to everything.
type change struct {
	by *Proc
	ev *depEvent
}

// idleTick is an idle tick of a loop, with how far idleWoken has read the
// changes since it started, and whether one of them woke the loop.
type idleTick struct {
	p     *Proc
	next  int
	woken bool
}

// idleOn reports whether the rule applies: under PartialOrder, and for a
// test that compares the reduction with a full tree under the same rule.
func (r *run) idleOn() bool { return (r.s.partialOrder || r.s.idleByReads) && r.depOn() }

// recordChange records the change bump counts. The version is the index of
// a change plus one, as bump is the only place the version moves.
func (r *run) recordChange(p *Proc) {
	if !r.idleOn() {
		return
	}
	var ev *depEvent
	if p != nil && r.current == p {
		ev = r.depCur()
	}
	r.changes = append(r.changes, change{by: p, ev: ev})
}

func (r *run) idleRecord(p *Proc) {
	if !r.idleOn() {
		return
	}
	if r.idle == nil {
		r.idle = map[*procType]*idleTick{}
	}
	r.idle[p.pt] = &idleTick{p: p, next: p.started}
}

// idleGated reports whether a loop that went idle may not tick yet, as
// nothing changed that could change what its tick finds. Under PartialOrder
// that is a change by another process, since its last idle tick started, to
// what the tick touched, and otherwise any change since. The last idle tick
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

func (r *run) idleOverlaps(tick *Proc, c change) bool {
	if tick.touchedAll || r.idleChildren || c.ev == nil {
		return true
	}
	for res, w := range c.ev.acc {
		if !w {
			continue
		}
		if res == depAll {
			return true
		}
		for t := range tick.touched {
			if depSameRes(res, t) {
				return true
			}
		}
	}
	return false
}

// idleTouch records that p touched res, when p is a loop tick.
func (r *run) idleTouch(p *Proc, res string) {
	if p == nil || !r.idleOn() {
		return
	}
	if p.pt.fromLoop {
		r.idleChildren = true
		return
	}
	if p.pt.kind != trigLoop {
		return
	}
	if res == depAll {
		p.touchedAll = true
		return
	}
	if p.touched == nil {
		p.touched = map[string]bool{}
	}
	p.touched[res] = true
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
// wake it. A function in FROM and a kind of statement other than the four
// DML ones count as reading everything.
func (x *sqlExec) idleReads(stmt sqlir.Statement) {
	tx := x.tx
	r := tx.p.r
	if !r.idleOn() {
		return
	}
	touch := func(res string) {
		r.idleTouch(tx.p, res)
		if r.current != tx.p {
			r.idleTouch(r.current, res)
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
