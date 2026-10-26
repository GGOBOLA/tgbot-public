package gateway

import (
	"sync"
	"time"
)

// Failed authentication is what gets throttled, not all pre-auth traffic: a
// reverse proxy or a tunnel puts every customer on one peer address, so a bucket
// on the address itself would cap paying customers together. Only a request that
// actually presents a bad key consumes budget, which leaves a legitimate client
// retrying a key unaffected while a guessing flood runs out after authThrottleLimit
// attempts per window.
const (
	authThrottleLimit  = 20
	authThrottleWindow = 5 * time.Minute
	// authThrottleMaxPeers bounds the map so a flood of distinct source addresses
	// cannot grow it without limit; the oldest state is dropped wholesale when the
	// cap is reached, which costs at most one window of accounting.
	authThrottleMaxPeers = 4096
)

type authFailEntry struct {
	count   int
	resetAt time.Time
}

// authFailures counts bad-key attempts per peer address in a fixed window.
type authFailures struct {
	mu sync.Mutex
	n  map[string]authFailEntry
}

func newAuthFailures() *authFailures {
	return &authFailures{n: make(map[string]authFailEntry)}
}

// blocked reports whether this peer has spent its failure budget.
func (a *authFailures) blocked(peer string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.n[peer]
	if !ok {
		return false
	}
	if !now.Before(e.resetAt) {
		delete(a.n, peer)
		return false
	}
	return e.count >= authThrottleLimit
}

// fail records one rejected key for this peer.
func (a *authFailures) fail(peer string, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.n) >= authThrottleMaxPeers {
		for k, e := range a.n {
			if !now.Before(e.resetAt) {
				delete(a.n, k)
			}
		}
		if len(a.n) >= authThrottleMaxPeers {
			clear(a.n)
		}
	}
	e, ok := a.n[peer]
	if !ok || !now.Before(e.resetAt) {
		a.n[peer] = authFailEntry{count: 1, resetAt: now.Add(authThrottleWindow)}
		return
	}
	e.count++
	a.n[peer] = e
}

// sweep drops windows that have expired.
func (a *authFailures) sweep(now time.Time) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	dropped := 0
	for k, e := range a.n {
		if !now.Before(e.resetAt) {
			delete(a.n, k)
			dropped++
		}
	}
	return dropped
}
