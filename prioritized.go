package detest

import (
	"math/rand/v2"
	"slices"
)

// prioritized is the state of one run under the Prioritized strategy, after
// PCT (Burckhardt et al., "A Randomized Scheduler with Probabilistic
// Guarantees of Finding Bugs", ASPLOS 2010). Every process, and every process
// a start or a delivery would create, has a priority, and each step goes to
// the highest. At depth-1 steps drawn from the first k, the process that ran
// last drops below every other, so a run switches between processes there
// and almost nowhere else.
//
// PCT knows its threads in advance. Here a process is created by an option of
// the scheduler, so the option gets its priority when it first appears and
// passes it on to the process it creates. Crashes and losses are not orders
// of processes, and drawing them at every step as Random does would put them
// near the start of most runs, so they happen at steps drawn from the first k
// as the switches do, to an option drawn among the ones then possible.
type prioritized struct {
	rng      *rand.Rand
	step     int
	switches []int  // the steps where the process that ran last drops, ascending
	lowered  uint64 // switches done; the i-th gives priority i, below every drawn one
	crashAt  []int
	loseAt   []int
	crashDue int
	loseDue  int
	starts   map[startKey]uint64
	delivers map[deliverKey]uint64
	inherit  uint64 // the priority of the option picked, for the process it creates
}

type startKey struct {
	pt *procType
	n  int // the instance's number among the type's runs
}

type deliverKey struct {
	pt  *procType
	msg *qmsg
}

// drawnBase keeps every drawn priority above the ones switches give, which
// are at most depth-1.
const drawnBase = 1 << 40

func newPrioritized(rng *rand.Rand, depth, k, crashes, losses int) *prioritized {
	p := &prioritized{rng: rng, starts: map[startKey]uint64{}, delivers: map[deliverKey]uint64{}}
	k = max(k, 1)
	p.switches = distinctSteps(rng, min(depth-1, k), k)
	for range crashes {
		p.crashAt = append(p.crashAt, 1+rng.IntN(k))
	}
	for range losses {
		p.loseAt = append(p.loseAt, 1+rng.IntN(k))
	}
	slices.Sort(p.crashAt)
	slices.Sort(p.loseAt)
	return p
}

// distinctSteps draws n distinct steps of 1..k, ascending.
func distinctSteps(rng *rand.Rand, n, k int) []int {
	picked := map[int]bool{}
	for len(picked) < n {
		picked[1+rng.IntN(k)] = true
	}
	steps := make([]int, 0, n)
	for s := range picked {
		steps = append(steps, s)
	}
	slices.Sort(steps)
	return steps
}

func (p *prioritized) draw() uint64 { return drawnBase + p.rng.Uint64()>>24 }

// pick returns the option the step takes. A fault due takes the step when
// one is possible, and stays due until then.
func (p *prioritized) pick(r *run, opts []option) int {
	p.step++
	for len(p.switches) > 0 && p.switches[0] == p.step {
		p.switches = p.switches[1:]
		p.lowered++
		if r.current != nil {
			r.current.prio = p.lowered
		}
	}
	for len(p.crashAt) > 0 && p.crashAt[0] == p.step {
		p.crashAt, p.crashDue = p.crashAt[1:], p.crashDue+1
	}
	for len(p.loseAt) > 0 && p.loseAt[0] == p.step {
		p.loseAt, p.loseDue = p.loseAt[1:], p.loseDue+1
	}
	if p.crashDue > 0 {
		if i, ok := p.drawOf(opts, optCrash); ok {
			p.crashDue--
			return i
		}
	}
	if p.loseDue > 0 {
		if i, ok := p.drawOf(opts, optLose); ok {
			p.loseDue--
			return i
		}
	}
	best, bestPrio := -1, uint64(0)
	for i, o := range opts {
		if o.kind == optCrash || o.kind == optLose {
			continue
		}
		if pr := p.prioOf(r, o); best < 0 || pr > bestPrio {
			best, bestPrio = i, pr
		}
	}
	if best < 0 {
		// Every process waits for a lock and only faults are left, which
		// the run takes rather than end there.
		return p.rng.IntN(len(opts))
	}
	if opts[best].kind != optResume {
		p.inherit = bestPrio
	}
	return best
}

func (p *prioritized) drawOf(opts []option, kind optionKind) (int, bool) {
	var idx []int
	for i, o := range opts {
		if o.kind == kind {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return 0, false
	}
	return idx[p.rng.IntN(len(idx))], true
}

func (p *prioritized) prioOf(r *run, o option) uint64 {
	switch o.kind {
	case optStart:
		key := startKey{o.pt, r.runs[o.pt]}
		if _, ok := p.starts[key]; !ok {
			p.starts[key] = p.draw()
		}
		return p.starts[key]
	case optDeliver:
		key := deliverKey{o.pt, o.q.msgs[o.i]}
		if _, ok := p.delivers[key]; !ok {
			p.delivers[key] = p.draw()
		}
		return p.delivers[key]
	default:
		return o.p.prio
	}
}

// spawned gives a new process the priority of the option that creates it, or
// a drawn one when another process spawned it.
func (p *prioritized) spawned(proc *Proc) {
	if p.inherit != 0 {
		proc.prio, p.inherit = p.inherit, 0
		return
	}
	proc.prio = p.draw()
}
