package gateway

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tgbot/internal/ledger"
	"tgbot/internal/store"
)

// doMessages sends a /v1/messages request through the real handler.
func (h *harness) doMessages(t *testing.T, body, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, messagesPath, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	h.srv.handleMessages(rec, req)
	return rec
}

// A 2xx that reports no usage must not be free: the pre-flight hold is the only
// number the gateway has, and charging zero is how a 200k-token call slipped
// through unbilled.
func TestMissingUsageBillsTheHold(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	before, err := h.store.Account(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"test/model","messages":[{"role":"user","content":"hi"}]}`
	if rec := h.do(t, body, testKey); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	after, err := h.store.Account(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if int64(after.Balance) >= int64(before.Balance) {
		t.Fatalf("balance unchanged (%d): a served call with no usage was free", int64(after.Balance))
	}
}

// A failed /v1/messages call is not a served request: it must not add a usage
// row, and its upstream status must reach the client.
func TestFailedMessagesNotCounted(t *testing.T) {
	const sentinel = "provider-private-sentinel-4821"
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"`+sentinel+`"}}`)
	})
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	body := `{"model":"test/model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	rec := h.doMessages(t, body, testKey)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"type":"gateway_error"`) || strings.Contains(rec.Body.String(), sentinel) {
		t.Fatalf("customer response was not a sanitized gateway error: %s", rec.Body)
	}
	if !strings.Contains(logs.String(), sentinel) {
		t.Fatalf("captured server log does not contain upstream diagnostic: %s", logs.String())
	}
	reqs, _, _, spend, err := h.store.UsageTotals(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if reqs != 0 || spend != 0 {
		t.Fatalf("failed call counted: requests=%d spend=%d", reqs, spend)
	}
}

func TestLargeMessagesJSONResponseRelayedAndBilled(t *testing.T) {
	const paddingBytes = 4<<20 + 128<<10
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_large","type":"message","role":"assistant","content":[{"type":"text","text":"`)
		_, _ = io.WriteString(w, strings.Repeat("x", paddingBytes))
		_, _ = io.WriteString(w, `"}],"usage":{"input_tokens":1000000,"output_tokens":0}}`)
	})

	request := `{"model":"test/model","max_tokens":8,"messages":[]}`
	rec := h.doMessages(t, request, testKey)
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("status %d, want 2xx: %s", rec.Code, rec.Body)
	}
	if rec.Body.Len() <= maxBodyBytes || !strings.Contains(rec.Body.String(), `"msg_large"`) {
		t.Fatalf("large response was not relayed intact: %d bytes", rec.Body.Len())
	}
	rows := h.usageRows(t)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	if rows[0].cost == 0 {
		t.Fatal("usage row has zero cost")
	}
}

// A payload that merely contains the literal "data: [DONE]" must survive the
// tail strip: cutting at any occurrence truncated valid JSON.
func TestDoneMarkerInsideJSONNotStripped(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"explain data: [DONE] please"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`+"\ndata: [DONE]")
	})
	body := `{"model":"test/model","messages":[{"role":"user","content":"hi"}]}`
	rec := h.do(t, body, testKey)
	if !strings.Contains(rec.Body.String(), "explain data: [DONE] please") {
		t.Fatalf("body was truncated: %s", rec.Body)
	}
	if strings.HasSuffix(strings.TrimSpace(rec.Body.String()), "[DONE]") {
		t.Fatalf("trailing marker not stripped: %s", rec.Body)
	}
	// The call is still billed.
	reqs, _, _, _, err := h.store.UsageTotals(t.Context(), 42)
	if err != nil || reqs != 1 {
		t.Fatalf("usage requests=%d (%v)", reqs, err)
	}
}

// An upstream 200 whose body cannot be relayed — here, over the 4 MiB cap — is
// a 502 to the customer. They never received the response, so the pre-flight
// hold must not be charged for it: the hold stands in for a call that was
// served, and this one was not.
func TestRelayFailureDoesNotCharge(t *testing.T) {
	// A 200 that overflows the relay cap: the gateway can neither parse nor
	// forward it, so the customer sees a 502 with none of those tokens.
	overflow := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":1000,"completion_tokens":500},"pad":"`)
		_, _ = io.WriteString(w, strings.Repeat("x", maxRelayBytes+1))
		_, _ = io.WriteString(w, `"}`)
	}

	// check asserts the undelivered call cost the customer nothing at all.
	check := func(t *testing.T, h *harness, before int64, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status %d, want 502: %s", rec.Code, rec.Body)
		}
		after, err := h.store.Balance(t.Context(), 42)
		if err != nil {
			t.Fatal(err)
		}
		if int64(after) != before {
			t.Fatalf("balance %d -> %d: charged the hold for a response the client never received",
				before, int64(after))
		}
		entries, err := h.store.LedgerEntries(t.Context(), 42, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Reason == "usage" {
				t.Fatalf("ledger debited %d for an undelivered response", int64(e.Delta))
			}
		}
		reqs, _, _, spend, err := h.store.UsageTotals(t.Context(), 42)
		if err != nil {
			t.Fatal(err)
		}
		if reqs != 0 {
			t.Fatalf("usage requests = %d, want 0: a response the client never received is not a served request", reqs)
		}
		if spend != 0 {
			t.Fatalf("usage cost = %d, want 0 for an undelivered response", int64(spend))
		}
	}

	t.Run("chat", func(t *testing.T) {
		h := newHarness(t, overflow)
		before, err := h.store.Balance(t.Context(), 42)
		if err != nil {
			t.Fatal(err)
		}
		rec := h.do(t, `{"model":"test/model","messages":[{"role":"user","content":"hi"}]}`, testKey)
		check(t, h, int64(before), rec)
	})

	t.Run("messages", func(t *testing.T) {
		h := newHarness(t, overflow)
		before, err := h.store.Balance(t.Context(), 42)
		if err != nil {
			t.Fatal(err)
		}
		rec := h.doMessages(t, `{"model":"test/model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, testKey)
		check(t, h, int64(before), rec)
	})

	// The third relay path: 9router streams even when the caller asked for a
	// single JSON message, so writeClaudeJSON assembles the SSE events. A stream
	// that dies mid-assembly is a 502 with no body — the client got nothing.
	t.Run("messages_sse", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			// Declaring more bytes than are written makes the body end early,
			// which surfaces as scanner.Err() in writeClaudeJSON.
			w.Header().Set("Content-Length", "4096")
			_, _ = io.WriteString(w, "event: message_start\n"+
				`data: {"type":"message_start","message":{"id":"msg_1","model":"test/model","role":"assistant","usage":{"input_tokens":1000}}}`+"\n\n")
			_, _ = io.WriteString(w, `event: content_block_delta`+"\n"+`data: {"type":"content`)
		})
		before, err := h.store.Balance(t.Context(), 42)
		if err != nil {
			t.Fatal(err)
		}
		rec := h.doMessages(t, `{"model":"test/model","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`, testKey)
		check(t, h, int64(before), rec)
	})
}

// TestSlowAdminAlertDoesNotBlockBilling is the M4 regression test. The sink
// wired in production is SendAdminNotice, a Telegram HTTP call with a 70 s
// client timeout. Billing still holds the customer's inflight slot and a 15 s
// billing context, so a stalled Telegram send must never run on that path.
func TestSlowAdminAlertDoesNotBlockBilling(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// No usage reported: bill() charges the hold and raises the alert.
		_, _ = io.WriteString(w, `{}`)
	})

	entered := make(chan struct{}, 1)
	h.srv.SetAdminAlert(func(string) {
		select {
		case entered <- struct{}{}:
		default:
		}
		time.Sleep(2 * time.Second) // the stalled Telegram send
	})

	// The production lifecycle is Run, which starts the dispatcher before any
	// request is served. The harness has no listener, so start it directly.
	ctx, cancel := context.WithCancel(context.Background())
	h.srv.startAlerts(ctx)
	t.Cleanup(cancel)

	start := time.Now()
	rec := h.do(t, `{"model":"test/model","messages":[{"role":"user","content":"hi"}]}`, testKey)
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	t.Logf("billing took %s with a 2s alert sink", elapsed)
	// A quarter of the sink's 2 s: an inline send cannot fit, while a loaded
	// runner still has a wide margin over the ~5 ms a correct path needs.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("billing waited %s for a 2s alert sink: the alert is still on the billing path", elapsed)
	}

	// Fast is not enough: the alert must still have left the queue.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("alert never reached the sink")
	}

	// And the call is still billed exactly once.
	reqs, _, _, spend, err := h.store.UsageTotals(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if reqs != 1 || spend == 0 {
		t.Fatalf("usage requests=%d spend=%d, want one billed row", reqs, spend)
	}
}

// Guarantee the store refuses to apply a negative cost at all.
func TestChargeUsageRejectsNegativeCost(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	_, err := h.store.ChargeUsage(t.Context(), store.UsageEntry{
		TelegramID: 42, KeyID: 1, Model: "test/model", PromptTokens: 1, Cost: -12345,
	})
	if err == nil {
		t.Fatal("negative cost must be refused, not credited")
	}
	c, err := h.store.Account(t.Context(), 42)
	if err != nil || int64(c.Balance) != 1000*usenPerSen {
		t.Fatalf("balance moved: %d (%v)", int64(c.Balance), err)
	}
}

// alertLog is an admin-alert sink that records what it was handed.
type alertLog struct {
	mu   sync.Mutex
	msgs []string
}

func (a *alertLog) sink(msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.msgs = append(a.msgs, msg)
}

func (a *alertLog) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.msgs...)
}

// An upstream answer whose body cannot be relayed costs the customer nothing,
// but those tokens were spent against our 9router account. The operator must be
// told: before this, a single log line was the only trace of the loss.
func TestUnbillableCallAlerts(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	log := &alertLog{}
	h.srv.SetAdminAlert(log.sink)

	price, err := h.store.Price(t.Context(), "test/model")
	if err != nil {
		t.Fatal(err)
	}
	before, err := h.store.Balance(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	const hold = ledger.Amount(250_000) // µsen the call would have cost
	h.srv.bill(t.Context(), store.Key{ID: 1, TelegramID: 42}, "test/model", price,
		usage{PromptTokens: 900, CompletionTokens: 100}, 0, http.StatusBadGateway, false, hold, 0, false)

	got := log.all()
	if len(got) != 1 {
		t.Fatalf("want exactly 1 alert for an unbillable call, got %d: %q", len(got), got)
	}
	for _, want := range []string{"Unbillable call", "Customer 42", "test/model", "250000", "never reached the client"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("alert does not carry %q: %q", want, got[0])
		}
	}

	// Pinned alongside the alert: the customer still pays nothing for a
	// response they never received.
	after, err := h.store.Balance(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if int64(after) != int64(before) {
		t.Fatalf("balance %d -> %d: an undelivered response was charged", int64(before), int64(after))
	}
	reqs, _, _, spend, err := h.store.UsageTotals(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if reqs != 0 || spend != 0 {
		t.Fatalf("unbillable call recorded: requests=%d spend=%d", reqs, spend)
	}
	entries, err := h.store.LedgerEntries(t.Context(), 42, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Reason == "usage" {
			t.Fatalf("ledger debited %d for an undelivered response", int64(e.Delta))
		}
	}
}

// A broken relay path fails every request, so the alert is throttled to one per
// window; the calls it swallowed are carried in the next alert's count.
func TestUnbillableAlertThrottles(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	log := &alertLog{}
	h.srv.SetAdminAlert(log.sink)

	price, err := h.store.Price(t.Context(), "test/model")
	if err != nil {
		t.Fatal(err)
	}
	key := store.Key{ID: 1, TelegramID: 42}
	const hold = ledger.Amount(250_000)
	billUnbillable := func() {
		h.srv.bill(t.Context(), key, "test/model", price,
			usage{PromptTokens: 900, CompletionTokens: 100}, 0, http.StatusBadGateway, false, hold, 0, false)
	}

	billUnbillable()
	if n := len(log.all()); n != 1 {
		t.Fatalf("first unbillable call: %d alerts, want 1", n)
	}

	// Inside the window nothing more may be sent, however many calls fail.
	for i := 0; i < 3; i++ {
		billUnbillable()
	}
	if got := log.all(); len(got) != 1 {
		t.Fatalf("alert re-fired inside the window: %d alerts: %q", len(got), got)
	}

	// Past the window one alert fires and names the three it suppressed.
	h.srv.alertUnbillable(time.Now().Add(unbillableAlertWindow+time.Minute), key.TelegramID, "test/model", hold)
	got := log.all()
	if len(got) != 2 {
		t.Fatalf("after the window: %d alerts, want 2: %q", len(got), got)
	}
	if !strings.Contains(got[1], "3 more since the last alert") {
		t.Fatalf("second alert does not carry the suppressed count: %q", got[1])
	}

	// The new alert opens a fresh window: the suppressed count reset with it.
	billUnbillable()
	if got := log.all(); len(got) != 2 {
		t.Fatalf("alert re-fired inside the new window: %d alerts", len(got))
	}
}
