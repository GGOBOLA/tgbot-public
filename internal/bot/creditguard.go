package bot

import (
	"sync"
	"time"
)

// creditGuard remembers one credit tap for a short window so a double tap on
// the same button cannot credit twice.
//
// It is deliberately in memory: the window only has to cover the seconds
// between two taps, and the ledger's own ref already makes a *redelivered*
// callback idempotent across restarts. What it must never do is block a second
// deliberate credit — the old ref keyed on message and amount did exactly that,
// silently, for a different customer.
type creditGuard struct {
	mu   sync.Mutex
	ttl  time.Duration
	seen map[string]time.Time
}

func newCreditGuard() *creditGuard {
	return &creditGuard{ttl: 30 * time.Second, seen: make(map[string]time.Time)}
}

// claim records the key and reports whether this is the first tap inside the
// window. Expired entries are pruned on the way through, so the map cannot grow
// without bound.
func (g *creditGuard) claim(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	for k, at := range g.seen {
		if now.Sub(at) > g.ttl {
			delete(g.seen, k)
		}
	}
	if _, ok := g.seen[key]; ok {
		return false
	}
	g.seen[key] = now
	return true
}
