package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tgbot/internal/ledger"
)

func TestAuthBurstFiresOncePerCooldown(t *testing.T) {
	a := &anomalyWatcher{authLimit: 3}
	start := time.Unix(1_700_000_000, 0)

	if _, fire := a.authFailure(start); fire {
		t.Fatal("fired below the threshold")
	}
	a.authFailure(start)
	if n, fire := a.authFailure(start); !fire || n != 3 {
		t.Fatalf("third failure = (%d, %v), want (3, true)", n, fire)
	}
	// Still over the line, but the cooldown has not elapsed.
	if _, fire := a.authFailure(start.Add(time.Minute)); fire {
		t.Fatal("fired again inside the cooldown")
	}
	// A quiet window resets the count.
	if _, fire := a.authFailure(start.Add(2 * authFailWindow)); fire {
		t.Fatal("fired on the first failure of a new window")
	}
	// Three fresh failures after the cooldown are a new burst again.
	later := start.Add(2*authFailWindow + time.Hour)
	a.authFailure(later)
	a.authFailure(later)
	if _, fire := a.authFailure(later); !fire {
		t.Fatal("a fresh burst after the cooldown did not fire")
	}
}

func TestSpendSpikeFiresAtTheLineAndOncePerHour(t *testing.T) {
	a := &anomalyWatcher{spendLimit: int64(10 * ledger.UsecPerMYR)}
	start := time.Unix(1_700_000_000, 0)

	if _, fire := a.addSpend(start, ledger.Amount(9*ledger.UsecPerMYR)); fire {
		t.Fatal("fired below the line")
	}
	total, fire := a.addSpend(start, ledger.Amount(2*ledger.UsecPerMYR))
	if !fire || int64(total) != int64(11*ledger.UsecPerMYR) {
		t.Fatalf("crossing = (%d, %v), want 11 RM and true", int64(total), fire)
	}
	if _, fire := a.addSpend(start.Add(time.Minute), ledger.Amount(ledger.UsecPerMYR)); fire {
		t.Fatal("paged twice inside the cooldown")
	}
	// A new hour starts a new total; the cooldown is an hour, so the first
	// crossing after it fires again.
	if _, fire := a.addSpend(start.Add(time.Hour), ledger.Amount(10*ledger.UsecPerMYR)); !fire {
		t.Fatal("a new hour's crossing did not fire after the cooldown")
	}
}

// A negative or zero cost is not spend; it must not move the counter or fire.
func TestSpendIgnoresNonPositiveCosts(t *testing.T) {
	a := &anomalyWatcher{spendLimit: int64(ledger.UsecPerMYR)}
	start := time.Unix(1_700_000_000, 0)
	for _, c := range []ledger.Amount{0, -5, -100} {
		if _, fire := a.addSpend(start, c); fire {
			t.Fatalf("cost %d fired", int64(c))
		}
	}
	if a.spend != 0 {
		t.Fatalf("spend = %d, want 0", a.spend)
	}
}

// The wrapper must record the status and keep SSE streaming through guard.
func TestStatusWriterRecordsAndFlushes(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}
	if _, ok := any(sw).(http.Flusher); !ok {
		t.Fatal("statusWriter hides http.Flusher: every stream would buffer")
	}
	sw.WriteHeader(http.StatusUnauthorized)
	_, _ = sw.Write([]byte("no"))
	if sw.status != http.StatusUnauthorized {
		t.Fatalf("status = %d", sw.status)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("underlying recorder = %d", rec.Code)
	}
}
