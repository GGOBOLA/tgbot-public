package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tgbot/internal/ledger"
	"tgbot/internal/store"
)

// messagesPath is the Anthropic-shaped endpoint Claude Code and the Claude SDK use.
const messagesPath = "/v1/messages"

// modelObject is one entry of the OpenAI /v1/models list.
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// Pricing is an extension: µsen per 1M tokens, plus a human string. Clients
	// that only read id/object ignore it.
	Pricing modelPricing `json:"pricing"`
}

type modelPricing struct {
	PromptUsenPer1M     int64  `json:"prompt_usen_per_1m"`
	CompletionUsenPer1M int64  `json:"completion_usen_per_1m"`
	PromptMYRPer1M      string `json:"prompt_myr_per_1m"`
	CompletionMYRPer1M  string `json:"completion_myr_per_1m"`
}

// handleModels lists the models a customer may call: exactly those with a price,
// because anything unpriced is refused anyway. Requires a valid key.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.authorize(w, r); !ok {
		return
	}

	prices, err := s.db.Prices(r.Context())
	if err != nil {
		log.Printf("gateway: list prices: %v", err)
		writeErr(w, http.StatusInternalServerError, "model list unavailable")
		return
	}

	now := time.Now().Unix()
	out := make([]modelObject, 0, len(prices))
	for _, p := range prices {
		out = append(out, modelObject{
			ID:      p.Model,
			Object:  "model",
			Created: now,
			OwnedBy: "9router",
			Pricing: modelPricing{
				PromptUsenPer1M:     int64(p.PromptPerMillion),
				CompletionUsenPer1M: int64(p.CompletionPerMillion),
				PromptMYRPer1M:      formatMYR(int64(p.PromptPerMillion)),
				CompletionMYRPer1M:  formatMYR(int64(p.CompletionPerMillion)),
			},
		})
	}

	writeJSONBody(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

// handleMessages proxies the Anthropic Messages API. 9router answers it with SSE
// even when the caller did not ask to stream, so the response is always assembled
// from events: relayed as SSE for a streaming caller, or rebuilt into one JSON
// message so a non-streaming client (the Claude SDK) can parse it.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.billing.Add(1)
	defer s.billing.Done()

	start := time.Now()
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	auth, ok := s.authorize(w, r)
	if !ok {
		return
	}

	// Slot before body: see handleChat.
	if !s.inflight.acquire(auth.TelegramID) {
		writeErr(w, http.StatusTooManyRequests, "too many concurrent requests for this account, retry shortly")
		return
	}
	defer s.inflight.release(auth.TelegramID)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cannot read request body")
		return
	}
	var payload struct {
		Model     string `json:"model"`
		Stream    bool   `json:"stream"`
		MaxTokens int64  `json:"max_tokens"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if payload.Model == "" {
		writeErr(w, http.StatusBadRequest, "model is required")
		return
	}

	price, hold, ok := s.preflight(w, r, payload.Model, auth, int64(len(body)/4), payload.MaxTokens)
	if !ok {
		return
	}

	// Keep the caller's headers that matter to the Anthropic API.
	headers := map[string]string{}
	for _, h := range []string{"anthropic-version", "anthropic-beta"} {
		if v := r.Header.Get(h); v != "" {
			headers[h] = v
		}
	}
	// Outlive the client so an abandoned stream can still be read to its usage.
	upCtx, cancelUp := s.upstreamContext(r.Context())
	defer cancelUp()
	resp, err := s.forwardWithRetry(upCtx, messagesPath, body, headers)
	if err != nil {
		log.Printf("gateway: upstream error: %v", err)
		writeErr(w, http.StatusBadGateway, "upstream unavailable")
		return
	}

	// Keep upstream failures gateway-shaped. The early return preserves the
	// existing no-usage-row behavior for failed calls.
	if resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		logUpstreamFailure(resp)
		writeErr(w, resp.StatusCode, upstreamFailureMessage(resp.StatusCode))
		return
	}

	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	var (
		used      usage
		ttft      int64
		delivered bool
	)
	switch {
	case isSSE && payload.Stream:
		used, ttft, delivered = relayClaudeStream(w, resp, start)
	case isSSE:
		used, delivered = s.writeClaudeJSON(w, resp)
	default:
		used, delivered = readClaudeJSON(w, resp)
	}

	bctx, cancel := billingContext(r.Context())
	defer cancel()
	s.bill(bctx, auth, payload.Model, price, used, ttft, resp.StatusCode, delivered, hold, estimatePromptTokens(body), true)
}

// authorize authenticates the key and applies the per-customer rate limit,
// writing the error response itself when either fails.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (store.Key, bool) {
	// Only rejected keys are throttled, and only per peer address: a proxy puts
	// every customer on one address, so capping all pre-auth traffic would cap
	// paying customers together. A refusal here happens before any key is hashed
	// or any query touches the single SQLite connection.
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		peer = host
	}
	now := time.Now()
	if s.authFails.blocked(peer, now) {
		w.Header().Set("Retry-After", strconv.Itoa(int(authThrottleWindow.Seconds())))
		writeErr(w, http.StatusTooManyRequests, "too many failed authentication attempts, retry later")
		return store.Key{}, false
	}
	auth, err := s.authenticate(r)
	if err != nil {
		// A missing, malformed or unknown credential is the caller's problem and
		// stays 401. Anything else came from storage: answering 401 would make
		// every SDK discard a perfectly good key, and would page the operator
		// about a leaked key while the database is the actual fault.
		if errors.Is(err, ErrMissingBearer) || errors.Is(err, ErrMalformedKey) || errors.Is(err, ErrUnauthorized) {
			s.authFails.fail(peer, now)
			writeErr(w, http.StatusUnauthorized, "invalid or revoked API key")
			return store.Key{}, false
		}
		writeErr(w, http.StatusServiceUnavailable, "authentication unavailable")
		return store.Key{}, false
	}
	// Bucket on the customer, not the key: one customer may hold several active
	// keys (store.MaxActiveKeys) and must not get a fresh budget per key.
	if ok, wait := s.limiter.Allow(strconv.FormatInt(auth.TelegramID, 10), time.Now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, "rate limit exceeded, slow down and retry")
		return store.Key{}, false
	}
	return auth, true
}

// preflight checks that the model is offered and that the customer has credit
// left. auth is the already-authenticated key: re-reading it here doubled the
// hash and the database lookup on every request.
func (s *Server) preflight(w http.ResponseWriter, r *http.Request, model string, auth store.Key, promptTokens, maxTokens int64) (store.ModelPrice, ledger.Amount, bool) {
	price, err := s.db.Price(r.Context(), model)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusBadRequest, "model not offered: "+model)
		return store.ModelPrice{}, 0, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pricing unavailable")
		return store.ModelPrice{}, 0, false
	}

	customer, err := s.db.Account(r.Context(), auth.TelegramID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, "balance unavailable")
		return store.ModelPrice{}, 0, false
	}
	if customer.Balance <= 0 {
		writeErr(w, http.StatusPaymentRequired, "insufficient balance, reload it in the Telegram bot")
		return store.ModelPrice{}, 0, false
	}

	// Hold enough credit for this call before touching the upstream: the prompt
	// estimate plus the declared completion (or a default ceiling). Prompt tokens
	// are held at 1.25x because a cache write costs that much, and under-holding
	// is how a burst overshoots. This is what bounds the overshoot.
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	hold := ledger.Cost(price.Rates(), ledger.Usage{
		Prompt:     promptTokens + promptTokens/4,
		Completion: maxTokens,
	})
	if hold > customer.Balance {
		writeErr(w, http.StatusPaymentRequired, "insufficient balance for this request, reload it in the Telegram bot")
		return store.ModelPrice{}, 0, false
	}
	// A prepaid balance is the only spend gate. Usage is charged after the call
	// (store.ChargeUsage), which may take the balance negative by one request;
	// the next request is refused until the customer tops up.
	return price, hold, true
}

// forwardTo replays a request against 9router, adding the given extra headers.
func (s *Server) forwardTo(ctx context.Context, path string, body []byte, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.upstream+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return s.client.Do(req)
}

// writeJSONBody writes any JSON-serialisable value.
func writeJSONBody(w http.ResponseWriter, status int, v any) {
	payload, err := json.Marshal(v)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot encode response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// formatMYR renders µsen as a plain RM string for API responses. Same formatter
// as the dashboard: the two used to differ on negatives.
func formatMYR(usen int64) string { return ledger.FormatMYR(ledger.Amount(usen)) }

// readClaudeJSON relays a JSON Messages response and bills its usage. delivered
// is false when the body never reached the client, which is not a billable call.
func readClaudeJSON(w http.ResponseWriter, resp *http.Response) (usage, bool) {
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRelayBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "cannot read upstream response")
		return usage{}, false
	}
	if len(body) > maxRelayBytes {
		// One extra byte distinguishes "at the cap" from "truncated"; relaying a
		// truncated body would hand the client invalid JSON and lose the usage.
		writeErr(w, http.StatusBadGateway, "upstream response too large")
		return usage{}, false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)

	// Only 2xx bodies reach here: handleMessages relayed and returned already
	// for any status >= 300.
	var probe claudeUsageEnvelope
	if err := json.Unmarshal(body, &probe); err != nil {
		return usage{}, true
	}
	return probe.Usage.toUsage(), true
}
