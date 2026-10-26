package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestUpstreamDisasterAlert fires once when failures reach the threshold,
// stays quiet inside the window, and speaks again after it elapses.
func TestUpstreamDisasterAlert(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	var mu sync.Mutex
	var alerts []string
	h.srv.SetAdminAlert(func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		alerts = append(alerts, msg)
	})

	for i := 0; i < upstreamAlertThreshold; i++ {
		if err := h.store.RecordUpstreamFailure(ctx, chatPath, 503, 2, true); err != nil {
			t.Fatal(err)
		}
	}

	h.srv.maybeAlertUpstream(ctx, time.Now())
	mu.Lock()
	n := len(alerts)
	first := ""
	if n > 0 {
		first = alerts[0]
	}
	mu.Unlock()
	if n != 1 {
		t.Fatalf("want exactly 1 alert, got %d", n)
	}
	if !strings.Contains(first, "Provider trouble") ||
		!strings.Contains(first, fmt.Sprintf("%d request", upstreamAlertThreshold)) ||
		!strings.Contains(first, "503") {
		t.Fatalf("alert copy lacks the facts: %q", first)
	}

	// Inside the same window nothing may re-fire.
	h.srv.maybeAlertUpstream(ctx, time.Now().Add(upstreamAlertWindow-time.Minute))
	mu.Lock()
	n = len(alerts)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("alert re-fired inside the window: %d", n)
	}

	// After the window, failures are still fresh in it, so it must fire again.
	h.srv.maybeAlertUpstream(ctx, time.Now().Add(upstreamAlertWindow+time.Minute))
	mu.Lock()
	n = len(alerts)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("alert not re-sent after the window: %d", n)
	}
}

// TestUpstreamAlertUnderThreshold: a handful of blips is not a disaster.
func TestUpstreamAlertUnderThreshold(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	var fired int
	h.srv.SetAdminAlert(func(string) { fired++ })

	for i := 0; i < upstreamAlertThreshold-1; i++ {
		if err := h.store.RecordUpstreamFailure(ctx, chatPath, 503, 1, true); err != nil {
			t.Fatal(err)
		}
	}
	h.srv.maybeAlertUpstream(ctx, time.Now())
	if fired != 0 {
		t.Fatalf("alert fired below threshold (%d calls)", fired)
	}
}
