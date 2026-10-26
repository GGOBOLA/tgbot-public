package ratelimit

import (
	"testing"
	"time"
)

func TestBurstThenThrottle(t *testing.T) {
	l := New(2, 3) // 3 back-to-back, then 2/s
	now := time.Unix(1000, 0)

	for i := 0; i < 3; i++ {
		if ok, wait := l.Allow("k", now); !ok {
			t.Fatalf("burst call %d denied (wait %v)", i, wait)
		}
	}
	ok, wait := l.Allow("k", now)
	if ok {
		t.Fatal("4th call in the same instant must be denied")
	}
	if wait <= 0 || wait > 500*time.Millisecond {
		t.Fatalf("wait = %v, want ~500ms for 2/s", wait)
	}

	// 500ms later exactly one token has refilled.
	if ok, _ := l.Allow("k", now.Add(500*time.Millisecond)); !ok {
		t.Fatal("call after refill must be allowed")
	}
	if ok, _ := l.Allow("k", now.Add(500*time.Millisecond)); ok {
		t.Fatal("second call in the same instant must be denied")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l := New(1, 1)
	now := time.Unix(2000, 0)

	if ok, _ := l.Allow("a", now); !ok {
		t.Fatal("key a first call denied")
	}
	if ok, _ := l.Allow("a", now); ok {
		t.Fatal("key a second call allowed")
	}
	if ok, _ := l.Allow("b", now); !ok {
		t.Fatal("key b must not be affected by key a")
	}
}

// Sweep must not launder an exhausted bucket back into a full one: a client that
// idles past the sweep interval may only spend the tokens it actually accrued.
func TestSweepDoesNotRestoreFullBurst(t *testing.T) {
	l := New(1, 10) // 10 back-to-back, then 1/s
	now := time.Unix(4000, 0)

	for i := 0; i < 10; i++ {
		if ok, wait := l.Allow("k", now); !ok {
			t.Fatalf("burst call %d denied (wait %v)", i, wait)
		}
	}

	// Idle past the sweep window, but not long enough to earn the burst back.
	after := now.Add(3 * time.Second)
	l.Sweep(after, 2*time.Second)

	allowed := 0
	for {
		ok, wait := l.Allow("k", after)
		if ok {
			allowed++
			continue
		}
		if wait <= 0 {
			t.Fatalf("denied with wait %v, want a positive refill delay", wait)
		}
		break
	}
	if allowed != 3 {
		t.Fatalf("allowed %d calls after an idle sweep, want 3 (the 3 tokens accrued at 1/s)", allowed)
	}
}

// A bucket that has idled long enough to refill completely is indistinguishable
// from a new one, so sweeping it away must still be allowed.
func TestSweepDropsFullyRefilledKey(t *testing.T) {
	l := New(1, 10)
	now := time.Unix(5000, 0)

	for i := 0; i < 10; i++ {
		l.Allow("k", now)
	}
	if removed := l.Sweep(now.Add(30*time.Second), 10*time.Second); removed != 1 {
		t.Fatalf("removed %d refilled buckets, want 1", removed)
	}
	if ok, _ := l.Allow("k", now.Add(30*time.Second)); !ok {
		t.Fatal("a fully refilled key must get its burst back")
	}
}

func TestSweepDropsIdleKeys(t *testing.T) {
	l := New(1, 1)
	now := time.Unix(3000, 0)
	l.Allow("old", now)
	l.Allow("new", now.Add(10*time.Minute))

	if removed := l.Sweep(now.Add(10*time.Minute), 5*time.Minute); removed != 1 {
		t.Fatalf("removed %d idle buckets, want 1", removed)
	}
	if l.Size() != 1 {
		t.Fatalf("size = %d, want 1", l.Size())
	}
}
