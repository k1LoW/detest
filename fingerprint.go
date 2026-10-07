package detest

const (
	fnvOffset = 14695981039346656037
	fnvPrime  = 1099511628211
)

func (r *run) mixString(s string) { r.fp = hashString(r.fp, s) }

func (r *run) mixInt(v int64) { r.fp = hashInt(r.fp, v) }

// mixOptions folds the options of a step into the fingerprint, by the names
// they act on: pointers would differ between workers.
func (r *run) mixOptions(opts []option) {
	for _, o := range opts {
		r.mixInt(int64(o.kind))
		switch o.kind {
		case optResume, optCrash, optStall:
			r.mixString(o.p.name)
		case optLose:
			r.mixString(o.q.name)
			r.mixInt(int64(o.i))
		case optDeliver:
			r.mixString(o.q.name)
			r.mixInt(int64(o.i))
			r.mixString(o.pt.name)
		case optStart:
			r.mixString(o.pt.name)
		}
	}
}

func hashString(h uint64, s string) uint64 {
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime
	}
	h ^= 0xff // ends the string, so that "ab","c" and "a","bc" differ
	h *= fnvPrime
	return h
}

func hashInt(h uint64, v int64) uint64 {
	for range 8 {
		h ^= uint64(v & 0xff) //nolint:gosec // one byte at a time
		h *= fnvPrime
		v >>= 8
	}
	return h
}

// opHash hashes the shape of an operation a process yields at, its format,
// leaving the arguments out. Real code puts generated ids and wall-clock
// times in its statements, which differ from run to run of the same schedule
// (the bubble's clock keeps advancing across runs), while what a replay must
// reproduce is which operations happen in which order.
func opHash(format string) uint64 { return hashString(fnvOffset, format) }
