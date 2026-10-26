package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tgbot/internal/ledger"
	"tgbot/internal/ratelimit"
	"tgbot/internal/store"
)

// usenPerSen / usenPerRM keep the fixture arithmetic readable.
const (
	usenPerSen int64 = int64(ledger.UsecPerSen)
	usenPerRM  int64 = int64(ledger.UsecPerMYR)
)

// testKey is the plaintext key the harness mints; newHarness sets it.
var testKey string

// harness wires a gateway to a temp DB and a fake 9router.
type harness struct {
	srv     *Server
	store   *store.DB
	dbPath  string // the file the store holds, for raw row assertions
	upMsg   string // last upstream request body
	upAuth  string
	upCalls int
}

func newHarness(t *testing.T, upstream http.HandlerFunc) *harness {
	t.Helper()
	h := &harness{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.upMsg = string(body)
		h.upAuth = r.Header.Get("Authorization")
		h.upCalls++
		upstream(w, r)
	}))
	t.Cleanup(fake.Close)

	dbPath := filepath.Join(t.TempDir(), "bot.db")
	st, err := store.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h.store = st
	h.dbPath = dbPath

	ctx := context.Background()
	if err := st.EnsureCustomer(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Credit(ctx, 42, 10*ledger.UsecPerMYR, "topup", "seed"); err != nil { // RM 10.00
		t.Fatal(err)
	}

	plain, _, err := st.CreateKey(ctx, 42, "test")
	if err != nil {
		t.Fatal(err)
	}
	testKey = plain

	if err := st.SetPrice(ctx, store.ModelPrice{
		Model:                "test/model",
		PromptPerMillion:     2 * ledger.UsecPerMYR, // RM 2.00 / 1M
		CompletionPerMillion: 6 * ledger.UsecPerMYR, // RM 6.00 / 1M
	}); err != nil {
		t.Fatal(err)
	}
	h.srv = New("127.0.0.1:0", fake.URL, "internal-key", st, dbPath, 1000, 1000)
	return h
}

// do sends a chat request through the gateway handler.
func (h *harness) do(t *testing.T, body, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, chatPath, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	rec := httptest.NewRecorder()
	h.srv.handleChat(rec, req)
	return rec
}

func TestAuthAndModelGuards(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})

	body := `{"model":"test/model","messages":[{"role":"user","content":"hi"}]}`

	if rec := h.do(t, body, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: code %d, want 401", rec.Code)
	}
	if rec := h.do(t, body, "sk-wrongkey"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad key: code %d, want 401", rec.Code)
	}
	if rec := h.do(t, `{"model":"unknown/model","messages":[]}`, testKey); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown model: code %d, want 400", rec.Code)
	}
	if h.upCalls != 0 {
		t.Fatalf("upstream called %d times before auth/price passed", h.upCalls)
	}

	// Balance at zero → 402, and the upstream must not be touched.
	ctx := context.Background()
	if _, err := h.store.Charge(ctx, 42, 10*ledger.UsecPerMYR, "adjust", "drain"); err != nil {
		t.Fatal(err)
	}
	if rec := h.do(t, body, testKey); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("zero balance: code %d, want 402", rec.Code)
	}
	if h.upCalls != 0 {
		t.Fatal("upstream called with an empty balance")
	}
}

func TestNonStreamBillsAndStripsTail(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 9router appends an SSE tail even for non-stream calls.
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1000,"completion_tokens":500}}data: [DONE]`)
	})

	rec := h.do(t, `{"model":"test/model","messages":[{"role":"user","content":"hi"}]}`, testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d, body %s", rec.Code, rec.Body)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body is not clean JSON: %v (%q)", err, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Fatal("SSE tail leaked to the client")
	}
	if h.upAuth != "Bearer internal-key" {
		t.Fatalf("upstream auth = %q", h.upAuth)
	}

	// 1000 prompt @ RM2.00/1M = 200,000 µsen; 500 completion @ RM6.00/1M = 300,000 µsen.
	const wantCost = 500_000 // 0.5 sen, stored without rounding
	cust, err := h.store.Account(context.Background(), 42)
	if err != nil || int64(cust.Balance) != 1000*usenPerSen-wantCost {
		t.Fatalf("balance %d (%v), want %d", int64(cust.Balance), err, 1000*usenPerSen-wantCost)
	}
	reqs, prompt, completion, spend, err := h.store.UsageTotals(context.Background(), 42)
	if err != nil || reqs != 1 || prompt+completion != 1500 || int64(spend) != wantCost {
		t.Fatalf("totals req=%d tok=%d spend=%d (%v)", reqs, prompt+completion, int64(spend), err)
	}
	entries, err := h.store.LedgerEntries(context.Background(), 42, 5)
	if err != nil || entries[0].Reason != "usage" || int64(entries[0].Delta) != -wantCost {
		t.Fatalf("ledger %+v (%v)", entries, err)
	}
	// Invariant the balance pre-check relies on: money actually debited for usage
	// equals the usage aggregate, so spend can never exceed the credit on file.
	charged := int64(0)
	for _, e := range entries {
		if e.Reason == "usage" {
			charged += -int64(e.Delta)
		}
	}
	if charged != int64(spend) {
		t.Fatalf("ledger usage charge %d != usage aggregate %d", charged, int64(spend))
	}
}

// An upstream that answers 200 with an SSE content type and closes before its
// first byte consumed nothing, so nothing may be billed. The old code left
// delivered true and charged the full pre-flight hold for that silence.
func TestStreamDiesBeforeFirstByteIsNotBilled(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// Return without writing a single event: the body dies empty.
	})

	ctx := context.Background()
	before, err := h.store.Balance(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	rec := h.do(t, `{"model":"test/model","stream":true,"messages":[{"role":"user","content":"hi"}]}`, testKey)
	after, err := h.store.Balance(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("balance moved by %d µsen for a stream that produced nothing (code %d)", before-after, rec.Code)
	}
	requests, promptTokens, completionTokens, spend, err := h.store.UsageTotals(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 0 || promptTokens != 0 || completionTokens != 0 || spend != 0 {
		t.Fatalf("a usage row was written for an empty stream: %d requests, %d/%d tokens, %d µsen",
			requests, promptTokens, completionTokens, spend)
	}
}

// The Anthropic path had the same hole: a stream that dies before its first
// event was billed at the hold. Nothing reached the client, nothing is charged.
type deadlineWriter struct {
	mu       sync.Mutex
	deadline time.Time
	writes   int
	got      bytes.Buffer
}

func (w *deadlineWriter) Header() http.Header { return make(http.Header) }
func (w *deadlineWriter) WriteHeader(int)     {}
func (w *deadlineWriter) Flush()              {}
func (w *deadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadline = deadline
	w.mu.Unlock()
	return nil
}
func (w *deadlineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.writes++
	if w.writes == 1 {
		n, _ := w.got.Write(p)
		w.mu.Unlock()
		return n, nil
	}
	deadline := w.deadline
	w.mu.Unlock()
	if deadline.IsZero() {
		// No write deadline was armed: a real stalled socket blocks until the
		// peer window opens again, which is what pinned the handler for the whole
		// request deadline. Block past the test's patience.
		time.Sleep(2 * time.Second)
		return 0, context.DeadlineExceeded
	}
	if wait := time.Until(deadline); wait > 0 {
		time.Sleep(wait)
	}
	return 0, context.DeadlineExceeded
}

func TestStreamWriteStallBillsDeliveredUsageAndReleasesInflight(t *testing.T) {
	previousTimeout := writeStallTimeout
	writeStallTimeout = 20 * time.Millisecond
	defer func() { writeStallTimeout = previousTimeout }()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"later\"}}]}\n\n")
	})
	writer := &deadlineWriter{}
	request := httptest.NewRequest(http.MethodPost, chatPath,
		strings.NewReader(`{"model":"test/model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer "+testKey)
	done := make(chan struct{})
	go func() {
		h.srv.handleChat(writer, request)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after the response write deadline")
	}

	requests, prompt, completion, _, err := h.store.UsageTotals(context.Background(), 42)
	if err != nil || requests != 1 || prompt != 3 || completion != 2 {
		t.Fatalf("usage totals = %d/%d/%d (%v), want 1/3/2", requests, prompt, completion, err)
	}
	h.srv.inflight.mu.Lock()
	inflight := h.srv.inflight.n[42]
	h.srv.inflight.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("inflight slots = %d, want 0", inflight)
	}
	writer.mu.Lock()
	deadline := writer.deadline
	writer.mu.Unlock()
	if !deadline.IsZero() {
		t.Fatalf("write deadline was not cleared: %v", deadline)
	}
}

func TestClaudeStreamDiesBeforeFirstEventIsNotBilled(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
	})

	ctx := context.Background()
	before, err := h.store.Balance(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"test/model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	h.srv.handleMessages(rec, req)

	after, err := h.store.Balance(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("balance moved by %d µsen for an empty Claude stream (code %d)", before-after, rec.Code)
	}
}

func TestStreamBills(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		// A small real delay so the first-token wait is measurable (> 0 ms)
		// even on loopback.
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		chunks := []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3000,\"completion_tokens\":1000}}\n\n",
			"data: [DONE]\n\n",
		}
		for _, c := range chunks {
			_, _ = io.WriteString(w, c)
			flusher.Flush()
		}
	})

	rec := h.do(t, `{"model":"test/model","stream":true,"messages":[{"role":"user","content":"`+strings.Repeat("x", 1000)+`"}]}`, testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatal("stream not relayed to the client")
	}
	// stream_options.include_usage must be injected, else usage never arrives.
	if !strings.Contains(h.upMsg, `"include_usage":true`) {
		t.Fatalf("include_usage not injected: %s", h.upMsg)
	}

	// 3000 @ RM2/1M = 600,000 µsen + 1000 @ RM6/1M = 600,000 µsen → 1,200,000 µsen.
	const wantCost = 1_200_000 // RM 0.012
	cust, err := h.store.Account(context.Background(), 42)
	if err != nil || int64(cust.Balance) != 1000*usenPerSen-wantCost {
		t.Fatalf("balance %d (%v), want %d", int64(cust.Balance), err, 1000*usenPerSen-wantCost)
	}
	_, prompt, completion, spend, err := h.store.UsageTotals(context.Background(), 42)
	if err != nil || prompt+completion != 4000 || int64(spend) != wantCost {
		t.Fatalf("totals tok=%d spend=%d (%v)", prompt+completion, int64(spend), err)
	}

	// A streamed call must carry its first-token wait into the usage row.
	lat, err := h.store.LatencyByModel(context.Background(), 0)
	if err != nil || len(lat) != 1 {
		t.Fatalf("latency rows: %v %v", err, lat)
	}
	if lat[0].Model != "test/model" || lat[0].Requests != 1 || lat[0].AvgFirstMS <= 0 {
		t.Fatalf("latency row: %+v", lat[0])
	}
}

func TestPromptTokenCountIsCappedToRequestSize(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":1000000,"completion_tokens":0}}`)
	})
	body := `{"model":"test/model","messages":[{"role":"user","content":"hi"}]}`
	if rec := h.do(t, body, testKey); rec.Code != http.StatusOK {
		t.Fatalf("code %d, body %s", rec.Code, rec.Body)
	}
	ceiling := saturatingAdd(saturatingMul(estimatePromptTokens([]byte(h.upMsg)), 4), proxyPromptPad)
	price, err := h.store.Price(context.Background(), "test/model")
	if err != nil {
		t.Fatal(err)
	}
	wantCost := ledger.Cost(price.Rates(), ledger.Usage{Prompt: ceiling})
	cust, err := h.store.Account(context.Background(), 42)
	if err != nil || int64(cust.Balance) != 1000*usenPerSen-int64(wantCost) {
		t.Fatalf("balance %d (%v), want %d (ceiling %d)", int64(cust.Balance), err,
			1000*usenPerSen-int64(wantCost), ceiling)
	}
	_, prompt, _, spend, err := h.store.UsageTotals(context.Background(), 42)
	if err != nil || prompt != ceiling || spend != wantCost {
		t.Fatalf("prompt %d spend %d (%v), want prompt %d spend %d", prompt, spend, err, ceiling, wantCost)
	}
}

// syncWriter is a response writer usable from the handler goroutine and the test
// goroutine at once; httptest.ResponseRecorder's buffer is not safe for that.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
	hdr http.Header
}

func newSyncWriter() *syncWriter { return &syncWriter{hdr: make(http.Header)} }

func (w *syncWriter) Header() http.Header { return w.hdr }
func (w *syncWriter) WriteHeader(int)     {}
func (w *syncWriter) Flush()              {}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestDrainCancelsStreamAndBillsDeliveredUsage(t *testing.T) {
	sent := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n")
		w.(http.Flusher).Flush()
		close(sent)
		<-r.Context().Done()
	})

	req := httptest.NewRequest(http.MethodPost, chatPath,
		strings.NewReader(`{"model":"test/model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := newSyncWriter()
	handlerDone := make(chan struct{})
	go func() {
		h.srv.handleChat(rec, req)
		close(handlerDone)
	}()

	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never sent the usage chunk")
	}
	// Drain only once the usage line has actually reached the client: that is the
	// state a real shutdown lands in for a stream that already produced output,
	// and it is what makes the call delivered rather than silently unbilled.
	waitUntil(t, func() bool { return strings.Contains(rec.String(), "completion_tokens") })
	h.srv.drainOnce.Do(func() { close(h.srv.drainCh) })
	if !h.srv.waitBilling(time.Second) {
		t.Fatal("billing drain did not finish")
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("handler did not exit after drain")
	}

	requests, prompt, completion, _, err := h.store.UsageTotals(context.Background(), 42)
	if err != nil || requests != 1 || prompt != 3 || completion != 2 {
		t.Fatalf("usage totals requests=%d prompt=%d completion=%d (%v), want 1/3/2", requests, prompt, completion, err)
	}
}

// TestUpstreamRetryOn5xx proves a transient provider 5xx is retried instead of being
// handed to the customer, while a 4xx is not retried (it would fail identically).
func TestUpstreamRetryOn5xx(t *testing.T) {
	attempts := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"No ready deployment"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1000,"completion_tokens":0}}`)
	})

	body := `{"model":"test/model","messages":[{"role":"user","content":"hi"}]}`
	rec := h.do(t, body, testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d, body %s", rec.Code, rec.Body)
	}
	if attempts != 3 {
		t.Fatalf("upstream attempts = %d, want 3", attempts)
	}
	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("body %s", rec.Body)
	}
}

func TestUpstreamNoRetryOn4xx(t *testing.T) {
	attempts := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad model"}}`)
	})

	rec := h.do(t, `{"model":"test/model","messages":[]}`, testKey)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code %d, want 400", rec.Code)
	}
	if attempts != 1 {
		t.Fatalf("a 400 must not be retried, attempts = %d", attempts)
	}
}

// TestRateLimitThrottlesOneKey proves the limiter bounds one customer's calls
// and that the rejection is a clean 429 rather than an upstream call. Sharing
// the bucket across one customer's several keys is covered by
// TestRateLimitIsPerCustomerNotPerKey.
func TestRateLimitThrottlesOneKey(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":10,"completion_tokens":0}}`)
	})
	// Two calls allowed back-to-back, nothing more in the same instant.
	h.srv.limiter = ratelimit.New(0.0001, 2)

	body := `{"model":"test/model","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		if rec := h.do(t, body, testKey); rec.Code != http.StatusOK {
			t.Fatalf("call %d: code %d, want 200", i, rec.Code)
		}
	}
	rec := h.do(t, body, testKey)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third call: code %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
	if h.upCalls != 2 {
		t.Fatalf("upstream called %d times, want 2 (throttled call must not forward)", h.upCalls)
	}
}

type countingKeyLookup struct {
	KeyLookup
	queries atomic.Int32
}

func (c *countingKeyLookup) KeyByHash(ctx context.Context, hash []byte) (store.Key, error) {
	c.queries.Add(1)
	return c.KeyLookup.KeyByHash(ctx, hash)
}

type errorKeyLookup struct{ err error }

func (k errorKeyLookup) KeyByHash(context.Context, []byte) (store.Key, error) {
	return store.Key{}, k.err
}

func TestAuthenticationStoreFailureIsUnavailableNotAuthBurst(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	chain := h.srv.guard(testMux(h.srv))

	request := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
		return rec
	}
	getAuthFailures := func() int {
		h.srv.anomalies.mu.Lock()
		defer h.srv.anomalies.mu.Unlock()
		return h.srv.anomalies.authFails
	}

	h.srv.auth.Keys = errorKeyLookup{err: errors.New("database locked")}
	if rec := request("sk-store-error"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("store error: code %d, want 503", rec.Code)
	} else if strings.Contains(rec.Body.String(), "database locked") {
		t.Fatalf("store error leaked to client: %s", rec.Body.String())
	}
	if got := getAuthFailures(); got != 0 {
		t.Fatalf("auth-burst failures after store error = %d, want 0", got)
	}

	h.srv.auth.Keys = h.store
	if rec := request("sk-genuinely-unknown"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key: code %d, want 401", rec.Code)
	}
	if got := getAuthFailures(); got != 1 {
		t.Fatalf("auth-burst failures after unknown key = %d, want 1", got)
	}
}

func TestAuthFailureThrottleStopsBadKeysBeforeLookup(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	lookup := &countingKeyLookup{KeyLookup: h.store}
	h.srv.auth.Keys = lookup

	// Only rejected keys spend budget, so exactly authThrottleLimit bad attempts
	// reach the store and the next one is refused before any lookup happens.
	for i := 0; i < authThrottleLimit; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.RemoteAddr = "192.0.2.10:4321"
		req.Header.Set("Authorization", "Bearer sk-invalid-key")
		rec := httptest.NewRecorder()
		h.srv.handleModels(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("request %d: code %d, want 401", i+1, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "192.0.2.10:4321"
	req.Header.Set("Authorization", "Bearer sk-invalid-key")
	rec := httptest.NewRecorder()
	h.srv.handleModels(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request %d: code %d, want 429", authThrottleLimit+1, rec.Code)
	}
	if got := lookup.queries.Load(); got != int32(authThrottleLimit) {
		t.Fatalf("key lookups = %d, want %d: the throttled request must not reach the store", got, authThrottleLimit)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
}

// TestModelsListRequiresKeyAndShowsPricing proves /v1/models is gated and only
// advertises priced models, since unpriced ones are refused at call time anyway.
func TestModelsListRequiresKeyAndShowsPricing(t *testing.T) {
	// The upstream handler runs on the server's goroutine, where t.Fatal would
	// Goexit the wrong goroutine and surface as an EOF on the client. Record it
	// and report from the test goroutine instead.
	var upstreamCalled atomic.Bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled.Store(true)
		t.Errorf("upstream must not be called")
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.srv.handleModels(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: code %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec = httptest.NewRecorder()
	h.srv.handleModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d", rec.Code)
	}
	if upstreamCalled.Load() {
		t.Fatal("/v1/models must be served from the price table, not forwarded upstream")
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Pricing struct {
				PromptMYRPer1M     string `json:"prompt_myr_per_1m"`
				CompletionMYRPer1M string `json:"completion_myr_per_1m"`
				PromptUsenPer1M    int64  `json:"prompt_usen_per_1m"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
	}
	if body.Object != "list" || len(body.Data) != 1 || body.Data[0].ID != "test/model" {
		t.Fatalf("list: %+v", body)
	}
	if body.Data[0].Pricing.PromptUsenPer1M != 200*usenPerSen || body.Data[0].Pricing.PromptMYRPer1M != "RM 2.00" {
		t.Fatalf("pricing: %+v", body.Data[0].Pricing)
	}
}

// TestMessagesConvertsSSEToJSON covers the real 9router quirk: a non-streaming
// /v1/messages request still gets SSE back, which a Claude SDK cannot parse, so the
// gateway must rebuild a single JSON message and bill from the events.
func TestMessagesConvertsSSEToJSON(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version not forwarded: %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"test/model\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":1000000,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"he\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"llo\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			// Anthropic sends input tokens in message_start and only the output
			// count in message_delta.
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":500000}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
		for _, e := range events {
			_, _ = io.WriteString(w, e)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"test/model","max_tokens":16,"messages":[{"role":"user","content":"`+strings.Repeat("x", 1_000_000)+`"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	h.srv.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "event:") {
		t.Fatalf("SSE leaked to a non-streaming caller: %s", rec.Body)
	}
	var msg struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, rec.Body)
	}
	if msg.ID != "msg_1" || msg.Type != "message" || msg.Role != "assistant" {
		t.Fatalf("envelope: %+v", msg)
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "hello" || msg.StopReason != "end_turn" {
		t.Fatalf("content: %+v", msg)
	}
	if msg.Usage.InputTokens != 1_000_000 || msg.Usage.OutputTokens != 500_000 {
		t.Fatalf("usage: %+v", msg.Usage)
	}

	// Billing uses Anthropic token names: 1M @ RM2.00/1M + 0.5M @ RM6.00/1M = RM 5.00.
	const wantCost = 500 * usenPerSen
	cust, err := h.store.Account(context.Background(), 42)
	if err != nil || int64(cust.Balance) != 1000*usenPerSen-wantCost {
		t.Fatalf("balance %d (%v), want %d", int64(cust.Balance), err, 1000*usenPerSen-wantCost)
	}
}

// TestMessagesStreamsToStreamingCaller keeps SSE for callers that asked for it,
// and still bills the usage reported in message_delta.
func TestMessagesStreamsToStreamingCaller(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1000000,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":0}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"test/model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"`+strings.Repeat("x", 1_000_000)+`"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	h.srv.handleMessages(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: message_stop") {
		t.Fatalf("stream not relayed: %d %s", rec.Code, rec.Body)
	}
	const wantCost = 200 * usenPerSen // 1M prompt @ RM2.00/1M
	cust, err := h.store.Account(context.Background(), 42)
	if err != nil || int64(cust.Balance) != 1000*usenPerSen-wantCost {
		t.Fatalf("balance %d (%v), want %d", int64(cust.Balance), err, 1000*usenPerSen-wantCost)
	}
}

func TestRevokedKeyRejected(t *testing.T) {
	// The upstream handler runs on the server's goroutine; t.Fatal there would
	// Goexit that goroutine and the client would see an EOF, not the cause.
	var upstreamCalled atomic.Bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled.Store(true)
		t.Errorf("upstream must not be called")
	})
	ctx := context.Background()
	keys, err := h.store.ListKeys(ctx, 42)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys %v (%v)", keys, err)
	}
	if err := h.store.RevokeKey(ctx, 42, keys[0].ID); err != nil {
		t.Fatal(err)
	}
	rec := h.do(t, `{"model":"test/model","messages":[]}`, testKey)
	if upstreamCalled.Load() {
		t.Fatalf("a revoked key reached the upstream; the response was %d", rec.Code)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: code %d, want 401", rec.Code)
	}
}

// TestUpstreamFailureRecorded proves a 5xx that a retry rescued is one recovered
// request in the /today counter, not one row per attempt.
func TestUpstreamFailureRecorded(t *testing.T) {
	attempts := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"No ready deployment"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":10,"completion_tokens":0}}`)
	})

	rec := h.do(t, `{"model":"test/model","messages":[]}`, testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d, body %s", rec.Code, rec.Body)
	}
	sum, err := h.store.UpstreamSummary(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failed != 1 || sum.Recovered != 1 || sum.GaveUp != 0 || sum.LastStatus != http.StatusServiceUnavailable {
		t.Fatalf("summary: %+v (attempts %d)", sum, attempts)
	}
}

// TestUpstreamFailureGaveUpRecorded is the other half: a provider that never
// recovers is counted as one request the customer saw fail.
func TestNegativeCompletionTokensAreClampedAndCharged(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1000,"completion_tokens":-7}}`)
	})

	rec := h.do(t, `{"model":"test/model","messages":[{"role":"user","content":"hi"}]}`, testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d, body %s", rec.Code, rec.Body)
	}

	_, prompt, completion, _, err := h.store.UsageTotals(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != 1000 || completion != 0 {
		t.Fatalf("usage tokens = (%d, %d), want (1000, 0)", prompt, completion)
	}

	cust, err := h.store.Account(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	const wantCost = 200_000 // 1000 prompt @ RM2.00/1M = 1000 × 200 µsen
	if got := 1000*usenPerSen - int64(cust.Balance); got != wantCost {
		t.Fatalf("ledger charge = %d, want %d", got, wantCost)
	}
}

func TestUpstreamFailureGaveUpRecorded(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		// 503 is the only status safe to replay: no provider work has started.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"No ready deployment"}}`)
	})

	rec := h.do(t, `{"model":"test/model","messages":[]}`, testKey)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code %d, want 503", rec.Code)
	}
	sum, err := h.store.UpstreamSummary(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failed != 1 || sum.Recovered != 0 || sum.GaveUp != 1 || sum.LastStatus != http.StatusServiceUnavailable {
		t.Fatalf("summary: %+v", sum)
	}
}

// TestUpstream500NotReplayed pins the retry policy: a 500/502 may already have
// produced tokens the provider will charge for, so it must reach the caller
// after one attempt instead of being replayed three times.
func TestUpstream500NotReplayed(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	})

	if rec := h.do(t, `{"model":"test/model","messages":[]}`, testKey); rec.Code != http.StatusInternalServerError {
		t.Fatalf("code %d, want 500", rec.Code)
	}
	if h.upCalls != 1 {
		t.Fatalf("upstream called %d times, want exactly 1", h.upCalls)
	}
}

// TestBillSurvivesCanceledClientContext pins the disconnect fix: a request whose
// client is already gone must still record usage and take the money.
func TestBillSurvivesCanceledClientContext(t *testing.T) {
	// Same rule: the handler runs off the test goroutine, so record the call
	// instead of calling t.Fatal from it.
	var upstreamCalled atomic.Bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled.Store(true)
		t.Errorf("upstream must not be called")
	})
	price, err := h.store.Price(context.Background(), "test/model")
	if err != nil {
		t.Fatal(err)
	}
	const prompt, completion = 1000, 500
	wantCost := ledger.Cost(ledger.Rates{
		PromptPerMillion:     price.PromptPerMillion,
		CompletionPerMillion: price.CompletionPerMillion,
	}, ledger.Usage{Prompt: prompt, Completion: completion})

	client, cancel := context.WithCancel(context.Background())
	cancel() // the customer hung up before billing

	bctx, done := billingContext(client)
	defer done()
	h.srv.bill(bctx, store.Key{ID: 1, TelegramID: 42}, "test/model", price,
		usage{PromptTokens: prompt, CompletionTokens: completion}, 5, http.StatusOK, true, 0, 0, false)
	if upstreamCalled.Load() {
		t.Fatal("billing after a client disconnect must not call the upstream")
	}

	cust, err := h.store.Account(context.Background(), 42)
	if err != nil || int64(cust.Balance) != 1000*usenPerSen-int64(wantCost) {
		t.Fatalf("balance %d (%v), want %d: the charge was lost",
			int64(cust.Balance), err, 1000*usenPerSen-int64(wantCost))
	}
}

// TestSpendHistoryDoesNotBlockCredit pins the balance gate: lifetime spend must
// not be compared with the remaining balance, or a customer is refused while
// still holding credit.
func TestSpendHistoryDoesNotBlockCredit(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 3M prompt tokens at RM2.00/1M = RM6.00 per call.
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":3000000,"completion_tokens":0}}`)
	})
	body := `{"model":"test/model","messages":[{"role":"user","content":"` + strings.Repeat("x", 3_000_000) + `"}]}`

	if rec := h.do(t, body, testKey); rec.Code != http.StatusOK {
		t.Fatalf("first call: %d %s", rec.Code, rec.Body)
	}
	// Spent RM6 of RM10, RM4 left. Under the old gate lifetime spend (RM6) was
	// compared with the remaining balance (RM4) and this was refused with 402.
	if rec := h.do(t, body, testKey); rec.Code != http.StatusOK {
		t.Fatalf("second call refused with %d though the customer still had credit", rec.Code)
	}

	cust, err := h.store.Account(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	// Two RM6 calls against a RM10 balance leave a visible RM2 debt, not a free call.
	if want := int64(-200 * usenPerSen); int64(cust.Balance) != want {
		t.Fatalf("balance %d, want %d (debt must be recorded)", int64(cust.Balance), want)
	}
	entries, err := h.store.LedgerEntries(context.Background(), 42, 10)
	if err != nil {
		t.Fatal(err)
	}
	usageRows := 0
	for _, e := range entries {
		if e.Reason == "usage" {
			usageRows++
		}
	}
	if usageRows != 2 {
		t.Fatalf("usage ledger rows = %d, want 2 (one per call)", usageRows)
	}
}

// TestMessagesAssemblesToolUse pins tool-call round-tripping: a non-streaming
// Claude client must get the block id, name and accumulated JSON input, not an
// empty text block.
func TestMessagesAssemblesToolUse(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_t","model":"test/model","role":"assistant","usage":{"input_tokens":1000,"output_tokens":0}}}` + "\n\n",
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}` + "\n\n",
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}` + "\n\n",
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"Kuala Lumpur\"}"}}` + "\n\n",
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n",
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":25}}` + "\n\n",
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
		}
		for _, e := range events {
			_, _ = io.WriteString(w, e)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"test/model","max_tokens":64,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	h.srv.handleMessages(rec, req)

	var msg struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, rec.Body)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("blocks: %+v", msg.Content)
	}
	b := msg.Content[0]
	if b.Type != "tool_use" || b.ID != "toolu_1" || b.Name != "get_weather" {
		t.Fatalf("tool block lost: %+v", b)
	}
	var args struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal(b.Input, &args); err != nil || args.City != "Kuala Lumpur" {
		t.Fatalf("tool input %s (%v)", b.Input, err)
	}
	if msg.StopReason != "tool_use" {
		t.Fatalf("stop_reason %q", msg.StopReason)
	}
}

// TestMessagesJSONUpstreamBills covers the one branch the SSE tests never reach:
// an upstream that answers /v1/messages with plain JSON.
func TestMessagesJSONUpstreamBills(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_j","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1000000,"output_tokens":0}}`)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"test/model","max_tokens":8,"messages":[{"role":"user","content":"`+strings.Repeat("x", 1_000_000)+`"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	h.srv.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"msg_j"`) {
		t.Fatalf("body not relayed: %s", rec.Body)
	}
	const wantCost = 200 * usenPerSen // 1M prompt @ RM2.00/1M
	cust, err := h.store.Account(context.Background(), 42)
	if err != nil || int64(cust.Balance) != 1000*usenPerSen-wantCost {
		t.Fatalf("balance %d (%v), want %d: JSON branch did not bill",
			int64(cust.Balance), err, 1000*usenPerSen-wantCost)
	}
}

// TestMaxTokensHoldRefusesUnaffordableCall pins the reservation: a request whose
// declared completion cannot be paid for is refused before the upstream is
// called, so one small balance cannot start a large generation.
func TestMaxTokensHoldRefusesUnaffordableCall(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for an unaffordable max_tokens")
	})
	// 10M completion tokens at RM6.00/1M = RM60 against a RM10 balance.
	body := `{"model":"test/model","max_tokens":10000000,"messages":[]}`
	if rec := h.do(t, body, testKey); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("code %d, want 402: %s", rec.Code, rec.Body)
	}
	if h.upCalls != 0 {
		t.Fatalf("upstream called %d times", h.upCalls)
	}
}

// TestFailedCallNotCountedAsRequest keeps error responses out of the dashboard's
// request totals; only a served call counts.
func TestFailedCallNotCountedAsRequest(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	})
	h.do(t, `{"model":"test/model","messages":[]}`, testKey)
	reqs, _, _, _, err := h.store.UsageTotals(context.Background(), 42)
	if err != nil || reqs != 0 {
		t.Fatalf("requests %d (%v), want 0: a failed call is not a served request", reqs, err)
	}
}

// failAfterWriter accepts one write, then behaves like a client that hung up.
type failAfterWriter struct {
	hdr    http.Header
	writes int
	status int
}

func (w *failAfterWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}
func (w *failAfterWriter) WriteHeader(status int) { w.status = status }
func (w *failAfterWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errors.New("client gone")
	}
	return len(p), nil
}

// TestAbandonedStreamStillBills pins the disconnect-without-usage case: the
// customer reads one chunk and hangs up, and the relay must keep reading to the
// usage chunk so the call is charged.
func TestAbandonedStreamStillBills(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, c := range []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":500}}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, c)
			flusher.Flush()
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test/model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	fw := &failAfterWriter{}
	h.srv.handleChat(fw, req)

	// 1000 prompt @ RM2/1M = 200,000 µsen; 500 completion @ RM6/1M = 300,000.
	const wantCost = 500_000
	cust, err := h.store.Account(context.Background(), 42)
	if err != nil || int64(cust.Balance) != 1000*usenPerSen-wantCost {
		t.Fatalf("balance %d (%v), want %d: an abandoned stream went unbilled",
			int64(cust.Balance), err, 1000*usenPerSen-wantCost)
	}
}

// TestHoldRejectsHugePromptOnEmptyBalance pins the pre-flight hold: a nearly
// drained balance cannot start a large generation.
func TestHoldRejectsHugePromptOnEmptyBalance(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called when the hold exceeds the balance")
	})
	ctx := context.Background()
	// Drain to RM 0.01.
	if _, err := h.store.Charge(ctx, 42, ledger.Amount(999*usenPerSen), "adjust", "drain"); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"test/model","max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`
	if rec := h.do(t, body, testKey); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("code %d, want 402: %s", rec.Code, rec.Body)
	}
	if h.upCalls != 0 {
		t.Fatalf("upstream called %d times", h.upCalls)
	}
}

// TestInflightLimiterCapsPerCustomer pins the bound on concurrent calls.
func TestInflightLimiterCapsPerCustomer(t *testing.T) {
	l := newInflightLimiter(2)
	if !l.acquire(7) {
		t.Fatal("first acquisition must succeed")
	}
	if !l.acquire(7) {
		t.Fatal("second acquisition must succeed")
	}
	if l.acquire(7) {
		t.Fatal("third concurrent call must be refused")
	}
	if !l.acquire(8) {
		t.Fatal("another customer must be unaffected")
	}
	l.release(7)
	if !l.acquire(7) {
		t.Fatal("released slot must be reusable")
	}
}

// TestMessagesBillsCacheTokens pins prompt-caching billing: Anthropic reports
// cache reads and writes separately from input_tokens, and ignoring them billed
// most of a Claude Code prompt at zero.
func TestMessagesBillsCacheTokens(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_c","model":"test/model","role":"assistant","usage":{"input_tokens":200000,"output_tokens":0,"cache_creation_input_tokens":200000,"cache_read_input_tokens":400000}}}` + "\n\n",
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n",
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":500000}}` + "\n\n",
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
		}
		for _, e := range events {
			_, _ = io.WriteString(w, e)
		}
	})

	// The upstream reports 1M input tokens, so the request has to be big enough
	// for that count to be plausible: bill() caps the billed prompt at roughly
	// four times the request's own estimate.
	pad := strings.Repeat("a", 900_000)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"test/model","max_tokens":16,"messages":[{"role":"user","content":"`+pad+`"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	h.srv.handleMessages(rec, req)

	// Counts stay inside what a sub-4MB request can plausibly represent, since
	// bill() caps the billed prompt at roughly four times the request estimate.
	// prompt 200k @ RM2/1M = 40 sen
	// completion 500k @ RM6/1M = 300 sen
	// cache read 400k @ RM0.2/1M = 8 sen
	// cache write 200k @ RM2.5/1M = 50 sen
	const wantCost = 398 * usenPerSen
	cust, err := h.store.Account(context.Background(), 42)
	if err != nil || int64(cust.Balance) != 1000*usenPerSen-wantCost {
		t.Fatalf("balance %d (%v), want %d: cache tokens were not billed",
			int64(cust.Balance), err, 1000*usenPerSen-wantCost)
	}

	var msg struct {
		Usage struct {
			InputTokens         int64 `json:"input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if msg.Usage.InputTokens != 200_000 || msg.Usage.CacheReadTokens != 400_000 ||
		msg.Usage.CacheCreationTokens != 200_000 {
		t.Fatalf("usage not mirrored to the client: %+v", msg.Usage)
	}
}

// testMux registers the routes Run registers. It is rebuilt here rather than
// factored out of Run so this task adds no production surface; the real
// registration and the real listener are exercised by
// TestRunStartsAlertDispatcher.
func testMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc(chatPath, s.handleChat)
	mux.HandleFunc(messagesPath, s.handleMessages)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	return mux
}

// errReader makes io.ReadAll fail, which is the only way the handler sees an
// unreadable request body without a real socket.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

// blockedReader parks a handler inside io.ReadAll until release is closed. It
// is how the table test occupies a semaphore slot or a per-customer slot with
// no sleeps: started is signalled from the handler's own goroutine, so a
// receive on it proves the slot was already taken (guard takes it before the
// handler runs, and handleChat takes the per-customer one before reading).
type blockedReader struct {
	started   chan<- struct{}
	release   <-chan struct{}
	signalled bool
}

func (r *blockedReader) Read([]byte) (int, error) {
	if !r.signalled {
		r.signalled = true
		r.started <- struct{}{}
	}
	<-r.release
	return 0, io.EOF
}

// errSurfaceCase is one row of the client-facing error table.
type errSurfaceCase struct {
	name string
	// upstream answers the proxied call; nil means a small 200 with usage.
	upstream func(w http.ResponseWriter, r *http.Request)
	method   string
	path     string
	body     string
	reader   io.Reader // overrides body when set
	// inflight replaces the per-customer cap; 0 keeps the production default.
	inflight int
	// hold parks this many requests inside the body read before the request
	// under test is sent, so a slot or the whole semaphore is really occupied.
	hold int
	// wantCode is the status the client must see; wantMsg is the exact message
	// in the error envelope (or a substring of the body for a 2xx).
	wantCode int
	wantMsg  string
}

// TestErrorSurface pins the client-facing refusals the audit found untested
// (M13). Every row drives the real guard(mux) chain, so the semaphore, the
// routing and the /healthz exemption are part of what is asserted, not just the
// handler's own error strings.
func TestErrorSurface(t *testing.T) {
	ok200 := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":10,"completion_tokens":1}}`)
	}
	// One byte past the relay cap: writeJSON can only answer 502.
	overflow := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":1000,"completion_tokens":500},"pad":"`)
		// One byte past the relay cap: the response cap moved from the request
		// cap (4 MB) to the relay cap (64 MB) so a large completion is not
		// refused after the provider already served and charged it.
		_, _ = io.WriteString(w, strings.Repeat("x", maxRelayBytes+1))
		_, _ = io.WriteString(w, `"}`)
	}
	const chatBody = `{"model":"test/model","messages":[{"role":"user","content":"hi"}]}`

	cases := []errSurfaceCase{
		{
			name: "method not allowed", method: http.MethodGet, path: chatPath,
			wantCode: http.StatusMethodNotAllowed, wantMsg: "method not allowed",
		},
		{
			name: "unreadable request body", method: http.MethodPost, path: chatPath,
			reader: errReader{}, wantCode: http.StatusBadRequest, wantMsg: "cannot read request body",
		},
		{
			name: "model is required", method: http.MethodPost, path: chatPath,
			body:     `{"messages":[{"role":"user","content":"hi"}]}`,
			wantCode: http.StatusBadRequest, wantMsg: "model is required",
		},
		{
			name: "upstream response too large", upstream: overflow,
			method: http.MethodPost, path: chatPath, body: chatBody,
			wantCode: http.StatusBadGateway, wantMsg: "upstream response too large",
		},
		{
			// maxInflightPerCustomer is an unexported const (8), so the cap is
			// driven through the constructor the production code uses rather
			// than by refactoring it. One held call fills the account's slot.
			name: "account fan-out cap", method: http.MethodPost, path: chatPath,
			body: chatBody, inflight: 1, hold: 1,
			wantCode: http.StatusTooManyRequests,
			wantMsg:  "too many concurrent requests for this account, retry shortly",
		},
		{
			// The held requests must themselves get past the per-customer cap,
			// so it is raised to maxConcurrent for this row; otherwise they
			// would be refused before they could hold a semaphore slot.
			name: "gateway busy", method: http.MethodPost, path: chatPath,
			body: chatBody, inflight: maxConcurrent, hold: maxConcurrent,
			wantCode: http.StatusServiceUnavailable,
			wantMsg:  "gateway busy, retry shortly",
		},
		{
			// Same saturation as the row above, but the liveness probe must
			// still be answered: a busy gateway is not a dead one.
			name: "healthz exempt while busy", method: http.MethodGet, path: "/healthz",
			inflight: maxConcurrent, hold: maxConcurrent,
			wantCode: http.StatusOK, wantMsg: "ok",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			up := c.upstream
			if up == nil {
				up = ok200
			}
			h := newHarness(t, up)
			if c.inflight > 0 {
				h.srv.inflight = newInflightLimiter(c.inflight)
			}
			chain := h.srv.guard(testMux(h.srv))

			// Park c.hold requests in the body read, then wait for each to
			// signal from inside the handler.
			release := make(chan struct{})
			started := make(chan struct{}, c.hold)
			var held sync.WaitGroup
			for i := 0; i < c.hold; i++ {
				held.Add(1)
				go func() {
					defer held.Done()
					req := httptest.NewRequest(http.MethodPost, chatPath,
						&blockedReader{started: started, release: release})
					req.Header.Set("Authorization", "Bearer "+testKey)
					chain.ServeHTTP(httptest.NewRecorder(), req)
				}()
			}
			defer func() {
				close(release)
				held.Wait()
			}()
			for i := 0; i < c.hold; i++ {
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					t.Fatalf("only %d of %d held requests reached the handler: no slot was occupied",
						i, c.hold)
				}
			}

			var body io.Reader = strings.NewReader(c.body)
			if c.reader != nil {
				body = c.reader
			}
			req := httptest.NewRequest(c.method, c.path, body)
			req.Header.Set("Authorization", "Bearer "+testKey)
			rec := httptest.NewRecorder()
			chain.ServeHTTP(rec, req)

			if rec.Code != c.wantCode {
				t.Fatalf("%s %s: code %d, want %d (body %s)",
					c.method, c.path, rec.Code, c.wantCode, rec.Body)
			}
			if c.wantCode < 400 {
				if !strings.Contains(rec.Body.String(), c.wantMsg) {
					t.Fatalf("body %q, want %q", rec.Body, c.wantMsg)
				}
				return
			}
			// OpenAI-shaped error: a client SDK surfaces this message, so the
			// exact text and the type are part of the contract.
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type %q, want application/json", ct)
			}
			var env struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("error body is not JSON: %v (%q)", err, rec.Body)
			}
			if env.Error.Message != c.wantMsg || env.Error.Type != "gateway_error" {
				t.Fatalf("error envelope %+v, want message %q", env.Error, c.wantMsg)
			}
			if c.upstream == nil && h.upCalls != 0 {
				t.Fatalf("upstream called %d times for a request refused before the proxy", h.upCalls)
			}
		})
	}
}
