package detest

// The partial order reduction: after each run, from its trace (see
// dependence.go), the subtrees the frontier still has to explore. It follows
// bounded partial order reduction (Coons, Musuvathi and McKinley, OOPSLA
// 2013), which is dynamic partial order reduction (Flanagan and Godefroid,
// POPL 2005) made sound under a preemption bound, computed after the run
// rather than at each state.
//
// For a step j of process p, every step i of another process that j depends
// on, between p's previous step (or its creation) and j, and the most recent
// one before them, gets two backtrack points: p's next transition at the
// state before i, and at the state before the last context switch before i,
// which the paper adds for the bound (its Algorithm 3). Over the states the
// paper visits one by one, those are the same points. A point names an
// option by what it is (see depIdent), not by its index, so that a later
// run finds it at the same state; where the process does not exist or
// cannot run, every option is added, and where only the bound keeps it out,
// none. The running process's next step is explored first at every state
// (the paper's Initialize), as it costs no preemption. An option enabled at
// a choice and no longer at the next without having run was disabled by
// what ran, which the paper's analysis never sees since the option never
// runs; it is backtracked as a dependent step. A run cut short, by a
// violation, MaxIdleTicks or a fatal error, may have left processes enabled
// that never took the step whose dependences the points come from, so it
// pushes every sibling in its own region as well. Data and fault choices are
// pushed in full. The same subtree is pushed once.

import (
	"slices"
	"strconv"
	"strings"
)

// subtree is a prefix the frontier holds, with what the reduction knows of
// the option it ends in.
type subtree struct {
	prefix       []choice
	ident        string // the option's identity, "" for a data or fault choice
	conservative bool   // added for the bound alone
}

type backtrackPoint struct{ ci, v int }

// prefixKey tells prefixes apart for the frontier's set of pushed ones.
func prefixKey(p []choice) string {
	var b strings.Builder
	for _, c := range p {
		b.WriteString(strconv.Itoa(c.picked))
		b.WriteByte(',')
	}
	return b.String()
}

// reduce returns the subtrees the run reveals under PartialOrder.
func (s *Sim) reduce(r *run, prefix int, truncated bool, sleepAt func(ci int) map[string]bool) []subtree {
	events := r.dep().events
	n := len(events)
	for k, e := range events {
		e.index = k
	}
	global := len(s.always) > 0 || len(s.sometimes) > 0
	// The step events the paper's transitions are, in order, with eager
	// starts left out: the schedule does not pick them and startEager keeps
	// the process that ran before as the current one, so they are no switch.
	var steps []int
	for k, e := range events {
		if e.isStep() && !e.eager {
			steps = append(steps, k)
		}
	}
	first := map[string]int{} // proc -> its first event
	for k, e := range events {
		if _, ok := first[e.proc]; !ok {
			first[e.proc] = k
		}
	}
	byCI := map[int]*depEvent{}
	for _, e := range events {
		if e.ci >= 0 {
			byCI[e.ci] = e
		}
	}
	// points maps each point to whether it was added for the bound alone,
	// and all the choices where every option is added, to the same.
	points := map[backtrackPoint]bool{}
	all := map[int]bool{}
	conservative := false
	addPoint := func(ci, v int) {
		if v == r.choices[ci].picked {
			return // the run itself
		}
		if z := sleepAt(ci); z != nil && v < len(byCI[ci].opts) && z[byCI[ci].opts[v]] {
			return // asleep there: its siblings' subtrees reach what it would
		}
		if was, ok := points[backtrackPoint{ci, v}]; !ok || was && !conservative {
			points[backtrackPoint{ci, v}] = conservative
		}
	}
	addAll := func(ci int) {
		if was, ok := all[ci]; !ok || was && !conservative {
			all[ci] = conservative
		}
	}
	// addIdent puts the option into the backtrack set of the state before
	// event i (the paper's AddBacktrackPoint): the option itself when it is
	// enabled there, nothing when only the bound keeps it out, else every
	// option. Nothing is added before an event the schedule did not pick:
	// there the only transition is the one taken, or the bound left the
	// others out.
	addIdent := func(i int, ident string) {
		e := events[i]
		if e.ci < 0 {
			return
		}
		if idx := slices.Index(e.opts, ident); idx >= 0 {
			addPoint(e.ci, idx)
			return
		}
		if e.enabled[ident] {
			return
		}
		addAll(e.ci)
	}
	// add is addIdent for process p's next transition: its resume, or the
	// option that creates it when it does not exist yet.
	add := func(i int, p string) {
		if events[i].ci < 0 {
			return
		}
		ident := "r:" + p
		if fe, ok := first[p]; ok && fe > i && !events[fe].eager && (events[fe].kind == "deliver" || events[fe].kind == "start") {
			ident = events[fe].ident
		}
		addIdent(i, ident)
	}
	// lastSwitch returns the last step before i at which the running
	// process changed, or the first step.
	lastSwitch := func(i int) int {
		j := -1
		for si, k := range steps {
			if k >= i {
				break
			}
			if si == 0 || events[steps[si-1]].proc != events[k].proc {
				j = k
			}
		}
		return j
	}
	// backtrack is the paper's Backtrack: before i, and before the last
	// switch before i.
	backtrack := func(i int, ident string) {
		conservative = false
		addIdent(i, ident)
		if j := lastSwitch(i); j >= 0 {
			conservative = true
			addIdent(j, ident)
			conservative = false
		}
	}
	backtrackProc := func(i int, p string) {
		conservative = false
		add(i, p)
		if j := lastSwitch(i); j >= 0 {
			conservative = true
			add(j, p)
			conservative = false
		}
	}
	lastOf := map[string]int{} // proc -> its last step so far
	for _, j := range steps {
		e := events[j]
		p := e.proc
		start := -1
		if k, ok := lastOf[p]; ok {
			start = k
		} else if e.creator != nil {
			start = e.creator.index
		}
		seen := map[string]bool{} // processes whose most recent dependent step before start is found
		for i := j - 1; i >= 0; i-- {
			f := events[i]
			if f.proc == p || !f.depConflicts(e, global) {
				continue
			}
			if i <= start {
				if seen[f.proc] {
					continue
				}
				seen[f.proc] = true
			}
			backtrackProc(i, p)
		}
		lastOf[p] = j
	}
	// An option enabled at a choice and no longer enabled at the next,
	// which did not run in between, was disabled by what ran.
	var cps []int
	for k, e := range events {
		if e.ci >= 0 {
			cps = append(cps, k)
		}
	}
	for ci, k := range cps {
		e := events[k]
		var next map[string]bool
		end := n
		if ci+1 < len(cps) {
			end = cps[ci+1]
			next = events[end].enabled
		}
		for ident := range e.enabled {
			if ident == e.ident || next[ident] {
				continue
			}
			ran := false
			for _, f := range events[k+1 : end] {
				if f.ident == ident {
					ran = true
					break
				}
			}
			if !ran {
				backtrack(k, ident)
			}
		}
	}
	// Initialize: the process that took the step before stays explored at
	// every state, so that the conservative points are reached without a
	// preemption.
	prev := ""
	for _, k := range steps {
		e := events[k]
		if e.ci >= 0 && prev != "" {
			if idx := slices.Index(e.opts, "r:"+prev); idx >= 0 {
				addPoint(e.ci, idx)
			}
		}
		prev = e.proc
	}
	// Data choices and faults past the prefix, in full; everything past the
	// prefix for a run cut short.
	for j := prefix; j < len(r.choices); j++ {
		c := r.choices[j]
		e := byCI[j]
		for v := range c.n {
			if v == c.picked {
				continue
			}
			switch {
			case e == nil || v >= len(e.opts) || depIsFault(e.opts[v]):
				points[backtrackPoint{j, v}] = false
			case truncated:
				addPoint(j, v)
			}
		}
	}
	for j, cons := range all {
		conservative = cons
		for v := range r.choices[j].n {
			if v == r.choices[j].picked {
				continue
			}
			if v < len(byCI[j].opts) && depIsFault(byCI[j].opts[v]) {
				points[backtrackPoint{j, v}] = false
			} else {
				addPoint(j, v)
			}
		}
	}
	// Ordered as children orders them: the stack yields the deepest first,
	// and among the options of one choice the lowest.
	ps := make([]backtrackPoint, 0, len(points))
	for p := range points {
		ps = append(ps, p)
	}
	slices.SortFunc(ps, func(a, b backtrackPoint) int {
		if a.ci != b.ci {
			return a.ci - b.ci
		}
		return b.v - a.v
	})
	out := make([]subtree, 0, len(ps))
	for _, pt := range ps {
		c := r.choices[pt.ci]
		p := make([]choice, pt.ci+1)
		copy(p, r.choices[:pt.ci])
		p[pt.ci] = choice{label: c.label, n: c.n, picked: pt.v, fp: c.fp}
		ident := ""
		if e := byCI[pt.ci]; e != nil && pt.v < len(e.opts) && !depIsFault(e.opts[pt.v]) {
			ident = e.opts[pt.v]
		}
		out = append(out, subtree{prefix: p, ident: ident, conservative: points[pt]})
	}
	return out
}

// defaultPick returns the option a step choice takes past the prefix under
// PartialOrder, or -1 to leave it to the schedule: the running process's
// next step, which costs no preemption, else the first option awake, else
// a fault, which never sleeps. With every option asleep the subtree is the
// siblings' and the run ends (covered).
func (r *run) defaultPick(opts []option) int {
	if !r.s.partialOrder || !r.depOn() || r.pos < len(r.prefix) || r.prio != nil {
		return -1
	}
	cur := r.current
	for k, o := range opts {
		if o.kind == optResume && o.p == cur && !r.asleep(o) {
			return k
		}
	}
	if len(r.sleep) == 0 {
		return -1
	}
	fault := -1
	for k, o := range opts {
		if o.kind.fault() {
			if fault < 0 {
				fault = k
			}
			continue
		}
		if !r.asleep(o) {
			return k
		}
	}
	if fault >= 0 {
		return fault
	}
	r.covered = true
	return -1
}
