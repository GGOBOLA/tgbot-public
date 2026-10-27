package gateway

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"tgbot/internal/ledger"
	"tgbot/internal/store"
)

type requestFailure struct {
	status  int
	message string
}

type requestPlan struct {
	path                  string
	model                 string
	body                  []byte
	headers               map[string]string
	preflightPromptTokens int64
	maxTokens             int64
	promptEstimate        int64
	respond               func(http.ResponseWriter, *http.Response, time.Time) requestResult
}

type requestResult struct {
	usage        usage
	firstMS      int64
	delivered    bool
	upstreamPads bool
}

// serveRequest owns the common Customer request lifecycle. The protocol
// adapter prepares its model request and handles the provider response; all
// admission, pricing, forwarding, result classification, and settlement stay
// here.
func (s *Server) serveRequest(
	w http.ResponseWriter,
	r *http.Request,
	prepare func([]byte) (requestPlan, *requestFailure),
) {
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
	// Acquire the Customer slot before reading the body, so a burst cannot hold
	// many large request bodies in memory at once.
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
	plan, failure := prepare(body)
	if failure != nil {
		writeErr(w, failure.status, failure.message)
		return
	}
	price, hold, ok := s.preflight(w, r, plan.model, auth, plan.preflightPromptTokens, plan.maxTokens)
	if !ok {
		return
	}

	upCtx, cancelUp := s.upstreamContext(r.Context())
	defer cancelUp()
	resp, err := s.forwardWithRetry(upCtx, plan.path, plan.body, plan.headers)
	if err != nil {
		log.Printf("gateway: upstream error: %v", err)
		writeErr(w, http.StatusBadGateway, "upstream unavailable")
		return
	}
	s.classifyProviderResult(r.Context(), plan.model, resp.StatusCode)
	result := plan.respond(w, resp, start)

	bctx, cancelBilling := billingContext(r.Context())
	defer cancelBilling()
	s.settlement.settle(bctx, settlementOutcome{
		auth: auth, model: plan.model, price: price, usage: result.usage,
		firstMS: result.firstMS, status: resp.StatusCode, delivered: result.delivered,
		hold: hold, promptEstimate: plan.promptEstimate, upstreamPads: result.upstreamPads,
	})
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
		if errors.Is(err, ErrMissingBearer) || errors.Is(err, ErrMalformedKey) || errors.Is(err, ErrUnauthorized) {
			s.authFails.fail(peer, now)
			writeErr(w, http.StatusUnauthorized, "invalid or revoked API key")
			return store.Key{}, false
		}
		// A storage failure is not a bad Customer credential; returning 401 would
		// cause clients to discard a valid key.
		writeErr(w, http.StatusServiceUnavailable, "authentication unavailable")
		return store.Key{}, false
	}
	// A Customer's keys share one rate budget.
	if ok, wait := s.limiter.Allow(strconv.FormatInt(auth.TelegramID, 10), time.Now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, "rate limit exceeded, slow down and retry")
		return store.Key{}, false
	}
	return auth, true
}

// preflight checks the Customer's priced Model and available Balance before
// forwarding. It reserves no credit; completed in-flight requests may create
// debt under ADR-0001.
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
	offline, err := s.availability.isOffline(r.Context(), model)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "model availability unavailable")
		return store.ModelPrice{}, 0, false
	}
	if offline {
		writeErr(w, http.StatusServiceUnavailable, "model temporarily unavailable")
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
	// The request-size estimate deliberately overstates prompt tokens to cover
	// JSON overhead and guard large prompts before they reach the upstream.
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
	// The preflight amount is a check, not a reservation. A served in-flight
	// request can still take the Balance below zero (ADR-0001).
	return price, hold, true
}
