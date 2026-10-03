package detest

import (
	"sync"

	"github.com/k1LoW/detest/internal/sqlir"
)

// parsedStatement caches a dialect's parse result per query string.
type parsedStatement struct {
	query string
	stmt  sqlir.Statement
}

type parseCacheKey struct {
	dialect string
	query   string
}

var (
	parseMu    sync.Mutex
	parseCache = map[parseCacheKey]*parsedStatement{}
)

func parseWith(d sqlir.Parser, query string) (*parsedStatement, error) {
	key := parseCacheKey{d.Name(), query}
	parseMu.Lock()
	defer parseMu.Unlock()
	if s, ok := parseCache[key]; ok {
		return s, nil
	}
	stmt, err := d.Parse(query)
	if err != nil {
		return nil, err
	}
	s := &parsedStatement{query: query, stmt: stmt}
	parseCache[key] = s
	return s, nil
}
