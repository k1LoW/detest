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

// parseCache maps a parseCacheKey to its *parsedStatement. Every statement of
// every worker looks it up, and after the first runs it is only read, which
// is what sync.Map serves without a lock to contend on.
var parseCache sync.Map

func parseWith(d sqlir.Parser, query string) (*parsedStatement, error) {
	key := parseCacheKey{d.Name(), query}
	if v, ok := parseCache.Load(key); ok {
		s, _ := v.(*parsedStatement) // only parsedStatements are stored
		return s, nil
	}
	stmt, err := d.Parse(query)
	if err != nil {
		return nil, err
	}
	v, _ := parseCache.LoadOrStore(key, &parsedStatement{query: query, stmt: stmt})
	s, _ := v.(*parsedStatement)
	return s, nil
}
