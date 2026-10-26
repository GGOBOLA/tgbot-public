// Package ratelimit bounds how fast one caller can hit the gateway, keyed by an
// opaque string, so a runaway client cannot drain a balance or hammer upstream in
// a single second.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// bucket is one key's token bucket.
type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// Limiter is a token bucket per string key, safe for concurrent use. The key is
// opaque to it -- the gateway passes the customer's auth.TelegramID, not the API
// key, so several keys held by one customer share a budget.
type Limiter struct {
	ratePerSec float64 // tokens added per second (= allowed burst per second)
	burst      float64 // bucket capacity: how many calls may arrive back-to-back

	mu      sync.Mutex
	buckets map[string]*bucket
}

// New builds a limiter allowing burst requests back-to-back and then ratePerSec
// requests per second sustained.
func New(ratePerSec, burst float64) *Limiter {
	// NaN passes every comparison, so `<= 0` alone lets it through and the
	// limiter then denies every call with a negative wait.
	if math.IsNaN(ratePerSec) || math.IsInf(ratePerSec, 0) || ratePerSec <= 0 {
		ratePerSec = 1
	}
	if math.IsNaN(burst) || math.IsInf(burst, 0) || burst < 1 {
		burst = 1
	}
	return &Limiter{
		ratePerSec: ratePerSec,
		burst:      burst,
		buckets:    make(map[string]*bucket),
	}
}

// Allow reports whether a call may proceed, and how long to wait if not.
func (l *Limiter) Allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		// A zero-value Limiter is exported; New is not the only way in.
		l.buckets = make(map[string]*bucket)
	}
	if l.ratePerSec <= 0 {
		l.ratePerSec = 1
	}
	if l.burst < 1 {
		l.burst = 1
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, lastSeen: now}
		l.buckets[key] = b
	} else {
		// Refill for the elapsed time, capped at the burst size.
		elapsed := now.Sub(b.lastSeen).Seconds()
		if elapsed > 0 {
			b.tokens = min(l.burst, b.tokens+elapsed*l.ratePerSec)
			b.lastSeen = now
		}
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// Time until one token is available again.
	need := (1 - b.tokens) / l.ratePerSec
	return false, time.Duration(need * float64(time.Second))
}

// Sweep drops buckets idle longer than idleFor, keeping memory bounded when keys
// come and go. Call it periodically.
//
// A bucket is only dropped once its refill has finished: deleting one that is
// still earning tokens would hand the key a brand-new full bucket on its next
// call, so a client idling just past the sweep interval could bypass the
// sustained rate with a fresh burst. A saturated bucket is indistinguishable
// from a new one, so dropping it changes nothing, and every bucket saturates
// within burst/rate seconds of idleness — memory still stays bounded.
func (l *Limiter) Sweep(now time.Time, idleFor time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Mirror Allow's normalization; a sweeper must not mutate the limiter.
	rate, burst := l.ratePerSec, l.burst
	if rate <= 0 {
		rate = 1
	}
	if burst < 1 {
		burst = 1
	}

	removed := 0
	for key, b := range l.buckets {
		idle := now.Sub(b.lastSeen)
		if idle <= idleFor {
			continue
		}
		if b.tokens+idle.Seconds()*rate < burst {
			// Still short of a full bucket: keep it, so the next call refills
			// from lastSeen instead of resetting to a full burst.
			continue
		}
		delete(l.buckets, key)
		removed++
	}
	return removed
}

// Size reports how many keys are currently tracked. Tests use it; no production
// caller does.
func (l *Limiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
