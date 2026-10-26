package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// The limiter's map is shared by every request goroutine. Single-goroutine tests
// cannot observe the mutex, so deleting it used to keep the suite green.
func TestConcurrentAllowAndSweep(t *testing.T) {
	l := New(1000, 100)
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i%8))
			for j := 0; j < 500; j++ {
				_, _ = l.Allow(key, now)
				if j%50 == 0 {
					l.Sweep(now.Add(time.Hour), time.Minute)
				}
			}
		}(i)
	}
	wg.Wait()
	// Goroutines can create a bucket after the final sweep, so the map may hold a
	// few; it must not hold one per call.
	if got := l.Size(); got > 8 {
		t.Fatalf("sweep left %d buckets", got)
	}
}

// A zero-value Limiter must not panic on first use.
func TestZeroValueLimiter(t *testing.T) {
	var l Limiter
	if ok, _ := l.Allow("k", time.Now()); !ok {
		t.Fatal("first call on a fresh bucket must be allowed")
	}
}
