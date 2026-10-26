package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// One newline-free line larger than maxRelayLine must end the relay instead of
// materialising in memory. Before the cap, relaySSE's ReadBytes would grow a
// single slice up to the 64 MB relay limit per connection.
func TestOverlongSSELineEndsRelayWithoutBilling(t *testing.T) {
	oversized := bytes.Repeat([]byte("a"), maxRelayLine+1024)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(oversized) // no newline: one unterminated line
		w.(http.Flusher).Flush()
	})

	before, err := h.store.Balance(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}

	body := `{"model":"test/model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, chatPath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.srv.handleChat(rec, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("handler did not return on an overlong SSE line")
	}

	// No usage chunk arrived, so the call carries no tokens; the balance must not
	// move and nothing may be recorded as served.
	after, err := h.store.Balance(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("balance moved by %d µsen for a stream that produced no usage", before-after)
	}
	requests, _, _, _, err := h.store.UsageTotals(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("usage rows = %d, want 0 for an over-long line", requests)
	}
}

// A normal multi-line stream still relays and bills, so the cap did not change
// the healthy path.
func TestNormalSSEStreamStillBills(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
			"data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n",
		} {
			_, _ = io.WriteString(w, line)
		}
		w.(http.Flusher).Flush()
	})

	rec := h.do(t, `{"model":"test/model","stream":true,"messages":[{"role":"user","content":"hi"}]}`, testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d, want 200", rec.Code)
	}
	requests, prompt, completion, _, err := h.store.UsageTotals(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || prompt != 3 || completion != 2 {
		t.Fatalf("usage = %d/%d/%d, want 1/3/2", requests, prompt, completion)
	}
}
