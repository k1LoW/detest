package detest

// Sleep sets for PartialOrder (Godefroid, Partial-Order Methods for the
// Verification of Concurrent Systems, 1996). A transition explored from a
// state sleeps in the subtrees of the siblings explored after it: no new
// state is reached by running it until a step dependent with it runs. Its
// access set is the one its step had when it ran from that state, which is
// what it does again from any state the independent steps in between lead
// to. A sleeping option is neither taken nor added as a backtrack point.
//
// Under a preemption bound two more rules keep the equivalent order within
// the bound. The running process's next step is explored first at every
// state (see defaultPick), so a sibling that preempts it leaves it asleep
// and the order with it first costs no more. A step taken where the running
// process could not go on, so that any option there was a free switch, never
// sleeps: with it first, the switch to its sibling would cost a preemption.
// Nor does one added as a backtrack point for the bound alone, as the paper
// on bounded partial order reduction does.

import "strings"

type sleeper struct {
	ident string
	acc   map[string]bool
}

// explored is a transition run from a state, for the sleep sets of the
// siblings run after it.
type explored struct {
	ident        string
	acc          map[string]bool
	conservative bool // never sleeps
}

// wakeBy returns the sleepers a step e leaves asleep: the ones independent
// of it.
func wakeBy(z []sleeper, e *depEvent, global bool) []sleeper {
	var out []sleeper
	for _, s := range z {
		f := &depEvent{proc: sleeperProc(s.ident), acc: s.acc}
		if f.depConflicts(e, global) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// sleeperProc is the process a sleeping transition belongs to, or a name no
// process has for a delivery or a start, whose process does not exist yet.
func sleeperProc(ident string) string {
	if p, ok := strings.CutPrefix(ident, "r:"); ok {
		return p
	}
	return "?" + ident
}

func (r *run) asleep(o option) bool {
	if len(r.sleep) == 0 || o.kind.fault() {
		return false
	}
	id := depIdent(o)
	for _, s := range r.sleep {
		if s.ident == id {
			return true
		}
	}
	return false
}

// wake wakes the sleeping transitions the step e is dependent with.
func (r *run) wake(e *depEvent) {
	if e == nil || len(r.sleep) == 0 {
		return
	}
	r.sleep = wakeBy(r.sleep, e, len(r.s.always) > 0 || len(r.s.sometimes) > 0)
}

func cloneSleep(z []sleeper) []sleeper {
	if len(z) == 0 {
		return nil
	}
	out := make([]sleeper, len(z))
	copy(out, z)
	return out
}

// sleepFor returns the sleep set the run of prefix starts with: what
// arrived at the state its last choice is made in, and the options run
// from that state before, unless they never sleep. f.mu is held.
func (f *frontier) sleepFor(prefix []choice) []sleeper {
	if !f.partial || len(prefix) == 0 {
		return nil
	}
	k := prefixKey(prefix[:len(prefix)-1])
	z := cloneSleep(f.zIn[k])
	me := f.pushed[prefixKey(prefix)]
	for _, x := range f.explored[k] {
		if x.conservative || x.ident == "" || (me != nil && x.ident == me.ident) {
			continue
		}
		z = append(z, sleeper{ident: x.ident, acc: x.acc})
	}
	return z
}

// record keeps what a run tells the sleep sets of the states it went
// through: the sleep set that arrived at each, and the option it ran there.
func (f *frontier) record(r *run, prefix int) {
	if !f.partial {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	me := f.pushed[prefixKey(r.choices[:prefix])]
	for _, e := range r.dep().events {
		if e.ci < 0 || e.ci < prefix-1 {
			continue
		}
		k := prefixKey(r.choices[:e.ci])
		if _, ok := f.zIn[k]; !ok {
			f.zIn[k] = r.sleepIn[e.ci]
		}
		conservative := e.ci == prefix-1 && me != nil && me.conservative
		f.explored[k] = append(f.explored[k], explored{ident: e.ident, acc: e.acc, conservative: conservative || e.free})
	}
}

// sleepAt returns what tells, for a run and a choice index, the options
// asleep at that state, which no backtrack point names.
func (f *frontier) sleepAt(r *run, prefix int) func(ci int) map[string]bool {
	return func(ci int) map[string]bool {
		var z []sleeper
		if ci >= prefix-1 {
			z = r.sleepIn[ci]
		} else {
			f.mu.Lock()
			z = f.zIn[prefixKey(r.choices[:ci])]
			f.mu.Unlock()
		}
		if len(z) == 0 {
			return nil
		}
		out := make(map[string]bool, len(z))
		for _, s := range z {
			out[s.ident] = true
		}
		return out
	}
}
