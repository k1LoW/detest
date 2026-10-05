package sqlir

import "sync"

var (
	warmupsMu sync.Mutex
	warmups   []func()
)

// RegisterWarmup registers work a kind's package does once per process, such
// as compiling its parser, for Explore to do before it watches for stalls.
// Done inside a run, the work makes no scheduling progress for as long as it
// takes, which under the race detector can pass the stall timeout.
func RegisterWarmup(f func()) {
	warmupsMu.Lock()
	defer warmupsMu.Unlock()
	warmups = append(warmups, f)
}

// Warmup does the registered work. Each function is to do its work once and
// return at once after that.
func Warmup() {
	warmupsMu.Lock()
	fs := warmups
	warmupsMu.Unlock()
	for _, f := range fs {
		f()
	}
}
