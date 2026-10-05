package detest

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// envOptions returns the options the environment sets. Explore applies them
// after the test's own, so they override those. They are the ones that
// depend on where the tests run rather than on what a test checks: how many
// workers, how many runs and how long, and which sample a random
// exploration draws. The bounds of the search, such as MaxPreemptions, are
// part of what a test claims, and only its code sets them.
func envOptions() ([]Option, error) {
	var opts []Option
	if v, ok := os.LookupEnv("DETEST_WORKERS"); ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("detest: bad DETEST_WORKERS %q, want an integer", v)
		}
		opts = append(opts, Workers(n))
	}
	if v, ok := os.LookupEnv("DETEST_MAX_RUNS"); ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("detest: bad DETEST_MAX_RUNS %q, want a positive integer", v)
		}
		opts = append(opts, MaxRuns(n))
	}
	if v, ok := os.LookupEnv("DETEST_MAX_DURATION"); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("detest: bad DETEST_MAX_DURATION %q, want a duration such as 10m", v)
		}
		opts = append(opts, MaxDuration(d))
	}
	if v, ok := os.LookupEnv("DETEST_SEED"); ok && v != "" {
		seed, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("detest: bad DETEST_SEED %q, want an unsigned integer", v)
		}
		// Which strategy a test explores with is part of what it checks, so
		// the seed applies to a test under Random only.
		opts = append(opts, func(s *Sim) {
			if s.strategy.random {
				s.strategy.seed = seed
			}
		})
	}
	return opts, nil
}
