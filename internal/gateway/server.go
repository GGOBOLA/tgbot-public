// Package gateway is the public OpenAI-compatible front door: it authenticates
// customer sk- keys against the hashed keys table, enforces balance, forwards to
// 9router with the internal key and bills the token usage it sees.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tgbot/internal/obs"
	"tgbot/internal/ratelimit"
	"tgbot/internal/store"
)

// errRelayTooLong is a stream that hit the relay cap. Headers are already sent
// by then, so the caller can only log it — but it must not pass as a clean end
// of stream.
var errRelayTooLong = errors.New("upstream stream exceeded the relay cap")

const (
	maxBodyBytes    = 4 << 20          // request cap; also 1M prompt tokens
	maxRelayBytes   = 64 << 20         // response cap for one upstream call
	maxRelayLine    = 16 << 20         // maximum size of one SSE line
	upstreamTimeout = 90 * time.Second // time to response headers, not the stream
	// requestDeadline bounds one whole proxied call. Generous because a long
	// generation is legitimate; the old 10-minute client timeout cut streams in
	// half and billed nothing for the tokens the provider had already produced.
	requestDeadline = 45 * time.Minute
	maxConcurrent   = 128
	// maxInflightPerCustomer bounds how many calls one account can have in
	// flight. It caps both the overshoot a concurrent burst can create and the
	// slots an abandoned stream can hold. Kept above normal agent fan-out
	// (Claude Code and friends run several tool calls at once).
	maxInflightPerCustomer = 8
	// defaultMaxTokens is the completion size assumed when a request declares
	// none, so the credit hold is never zero.
	defaultMaxTokens = 4096
	chatPath         = "/v1/chat/completions"
	billingTimeout   = 15 * time.Second
	// billingDrainTimeout is how long Run waits, after Shutdown, for settlement
	// already in progress to finish before the store is closed.
	billingDrainTimeout = 30 * time.Second
)

var writeStallTimeout = 30 * time.Second

// newUpstreamClient bounds connection setup and the wait for response headers,
// but not the body: an SSE stream legitimately runs longer than any fixed total.
func newUpstreamClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: upstreamTimeout,
			ExpectContinueTimeout: time.Second,
		},
		// A redirect is upstream-controlled. Following one across hosts would
		// forward the caller's prompt (and our bearer key) somewhere else, and
		// an https -> http hop would put both on the wire in clear text.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) == 0 {
				return nil
			}
			first := via[0].URL
			if req.URL.Host != first.Host {
				return errors.New("refusing redirect to another host")
			}
			if first.Scheme == "https" && req.URL.Scheme != "https" {
				return errors.New("refusing redirect that downgrades to http")
			}
			return nil
		},
	}
}

// billingContext detaches the charge from the client connection. net/http
// cancels the request context the moment the customer disconnects, which used to
// make both RecordUsage and Credit fail for a request the upstream had already
// served. The debt for that call must still be recoverable, so the write runs on
// a context of its own.
func billingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), billingTimeout)
}

// waitBilling waits for handlers to finish relaying and billing.
func (s *Server) waitBilling(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.billing.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// Server is the proxy. One instance per process.
type Server struct {
	addr     string
	upstream string // 9router base URL, no trailing slash
	apiKey   string // internal 9router key; never logged
	db       *store.DB
	client   *http.Client
	auth     Authenticator

	limiter   *ratelimit.Limiter
	authFails *authFailures
	inflight  *inflightLimiter

	availability     modelAvailability
	availabilityWake chan struct{}

	// adminAlert, when set, receives provider-disaster pings. Wired from main
	// with the bot's sender, so the gateway itself never touches Telegram.
	// alertMu guards it because SetAdminAlert runs at boot and settlement reads
	// it from request goroutines.
	alertMu    sync.RWMutex
	adminAlert func(string)
	settlement settlement
	// alerts queues admin alerts off the request path: the wired sink does a
	// blocking Telegram send, and request settlement must not wait for it.
	alerts alertQueue
	// upstreamAlertedAt throttles the disaster alert to one per window. Touched
	// only from the maintenance goroutine.
	upstreamAlertedAt time.Time
	// anomalies turns a burst of rejected auth and a spend spike into an admin
	// alert instead of a journal line nobody is watching.
	anomalies *anomalyWatcher

	// billing counts complete chat and Messages handlers. Shutdown's deadline can expire while
	// a handler is relaying or billing; closing the store under it would lose the
	// debit, so Run waits for this before returning.
	billing   sync.WaitGroup
	drainCh   chan struct{}
	drainOnce sync.Once
}

// New builds a gateway. addr is the listen address, upstream the 9router base URL.
// ratePerSec/burst bound each caller's request rate; the limiter is keyed on the
// customer's auth.TelegramID, so every key that customer holds shares one budget.
func New(addr, upstream, apiKey string, db *store.DB, dbPath string, ratePerSec, burst float64) *Server {
	s := &Server{
		addr:             addr,
		upstream:         strings.TrimRight(upstream, "/"),
		apiKey:           apiKey,
		db:               db,
		client:           newUpstreamClient(),
		auth:             Authenticator{Keys: db},
		limiter:          ratelimit.New(ratePerSec, burst),
		authFails:        newAuthFailures(),
		anomalies:        newAnomalyWatcher(),
		inflight:         newInflightLimiter(maxInflightPerCustomer),
		availabilityWake: make(chan struct{}, 1),
		alerts:           newAlertQueue(alertQueueSize, alertSpoolPath(dbPath)),
		drainCh:          make(chan struct{}),
	}
	s.availability = modelAvailability{db: db, probe: s.probeModel, now: time.Now, backoff: modelProbeBackoff[:]}
	s.settlement = settlement{
		db:                db,
		alert:             s.alert,
		alertSpend:        s.alertSpend,
		adminAlertPresent: func() bool { return s.adminAlertSink() != nil },
		now:               time.Now,
	}
	return s
}

// upstreamContext lets a proxied call outlive its client connection while still
// ending promptly when the server begins draining.
func (s *Server) upstreamContext(ctx context.Context) (context.Context, context.CancelFunc) {
	upCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestDeadline)
	done := make(chan struct{})
	go func() {
		select {
		case <-s.drainCh:
			cancel()
		case <-upCtx.Done():
		}
		close(done)
	}()
	return upCtx, func() {
		cancel()
		<-done
	}
}

// alertSpoolPath keeps the alert spool beside the database: same directory, same
// operator-configured lifecycle, no second path to keep in sync. An unset
// DB_PATH leaves spooling off rather than guessing a location.
func alertSpoolPath(dbPath string) string {
	if dbPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(dbPath), alertSpoolName)
}

// Run serves until ctx is cancelled, then drains.
func (s *Server) Run(ctx context.Context) error {
	// The dispatcher gets its own cancel so that BOTH ways out of Run — a
	// clean shutdown and a listener error — stop it and wait for its final
	// spool write. Without that, a process exiting on a late accept error
	// could leave a queued alert in memory with no durable record.
	alertCtx, stopAlerts := context.WithCancel(ctx)
	// Alerts go out on their own goroutine, and stop with the server.
	s.startAlerts(alertCtx)
	// Housekeeping: drop idle rate-limit buckets, roll up and prune old usage rows.
	go s.maintenance(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc(chatPath, s.handleChat)
	mux.HandleFunc(messagesPath, s.handleMessages)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           s.guard(mux),
		ReadHeaderTimeout: 10 * time.Second,
		// The request body is capped at 4 MB and must arrive promptly.
		ReadTimeout: 60 * time.Second,
		IdleTimeout: 120 * time.Second,
		// No WriteTimeout: an SSE response legitimately outlives any fixed bound.
		// Each request is bounded by requestDeadline instead.
		MaxHeaderBytes: 1 << 20,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.drainOnce.Do(func() { close(s.drainCh) })
		// A stream that outlives the drain is force-closed; that is expected on
		// shutdown, not a fatal error for the process.
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("gateway: shutdown: %v", err)
		}
		// A restart must not exit mid-send. The dispatcher spools whatever the
		// queue still holds on its way out (stop), and that write has to finish
		// before the process goes away: returning right after Shutdown used to
		// leave the spool empty and the next start with nothing to deliver.
		if !s.alerts.wait(context.Background(), alertDrainTimeout) {
			log.Printf("gateway: alert dispatcher still running %s after shutdown (queued alerts may not be spooled)", alertDrainTimeout)
		}
		stopAlerts()
		// Shutdown's own deadline force-closes long streams; a handler may still
		// be inside settlement at that moment, and the store closes as soon as Run
		// returns. Wait for those charges, then report the drain as failed so an
		// operator sees it rather than a clean exit hiding a lost debit.
		if !s.waitBilling(billingDrainTimeout) {
			log.Printf("gateway: billing still in flight %s after shutdown; a charge may be lost", billingDrainTimeout)
			return errors.New("shutdown left billing in flight")
		}
		return nil
	case err := <-errCh:
		// Same duty as the shutdown path: a process leaving on a listener
		// error must still spool what the queue holds, or the alert is gone.
		stopAlerts()
		if !s.alerts.wait(context.Background(), alertDrainTimeout) {
			log.Printf("gateway: alert dispatcher still running %s after a listener error (queued alerts may not be spooled)", alertDrainTimeout)
		}
		return err
	}
}

// inflightLimiter caps concurrent calls per customer. The client is trusted to
// disconnect mid-stream, and the relay keeps draining so the call can be billed;
// without this cap a few abandoned streams would hold the gateway's slots.
type inflightLimiter struct {
	mu  sync.Mutex
	n   map[int64]int
	max int
}

func newInflightLimiter(max int) *inflightLimiter {
	return &inflightLimiter{n: make(map[int64]int), max: max}
}

func (l *inflightLimiter) acquire(id int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n[id] >= l.max {
		return false
	}
	l.n[id]++
	return true
}

func (l *inflightLimiter) release(id int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n[id] <= 1 {
		delete(l.n, id)
		return
	}
	l.n[id]--
}

// guard bounds each request in time and caps how many run at once. Without the
// cap, a customer could hold hundreds of request bodies and SSE responses open.
// /healthz is exempt: it is a liveness probe, and a busy gateway is not a dead
// one — the watchdog must not report an outage during a burst.
func (s *Server) guard(next http.Handler) http.Handler {
	sem := make(chan struct{}, maxConcurrent)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		default:
			writeErr(w, http.StatusServiceUnavailable, "gateway busy, retry shortly")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestDeadline)
		defer cancel()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(ctx))
		if sw.status == http.StatusUnauthorized || sw.status == http.StatusForbidden {
			s.alertAuthBurst()
		}
	})
}

// statusWriter remembers the status code the handler chose, so guard can count
// rejected requests without every handler reporting them.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush keeps SSE working through the wrapper: the relay type-asserts for
// http.Flusher, and a wrapper that hides it would buffer every stream.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// handleChat adapts OpenAI chat payloads and responses to the shared request
// lifecycle.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	s.serveRequest(w, r, func(body []byte) (requestPlan, *requestFailure) {
		model, stream, maxTokens, upstreamBody, err := prepareBody(body)
		if err != nil || model == "" {
			return requestPlan{}, &requestFailure{status: http.StatusBadRequest, message: "model is required"}
		}
		return requestPlan{
			path: chatPath, model: model, body: upstreamBody,
			preflightPromptTokens: int64(len(body) / 4), maxTokens: maxTokens,
			promptEstimate: estimatePromptTokens(body),
			respond: func(w http.ResponseWriter, resp *http.Response, start time.Time) requestResult {
				result := requestResult{}
				result.upstreamPads = stream && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
				if result.upstreamPads {
					result.usage, result.firstMS, result.delivered = pipeStream(w, resp, start)
				} else {
					result.usage, result.delivered = writeJSON(w, resp)
				}
				return result
			},
		}, nil
	})
}

// authenticate turns "Bearer sk-…" into an active key. Only a hash leaves this
// function.
func (s *Server) authenticate(r *http.Request) (store.Key, error) {
	return s.auth.Authenticate(r)
}

// alert hands a message to the admin queue. The request path never waits for
// the sink: a stalled Telegram send used to hold a customer's inflight slot and
// their 15 s billing context. The inline send that remains is for the one state
// with no queue at all — before Run starts the dispatcher, which is what the
// maintenance path did before the queue existed. Every later failure (a spool
// that refuses an append, a gateway already stopped) goes to the journal, not to
// the request goroutine.
func (s *Server) alert(msg string) {
	fn := s.adminAlertSink()
	if fn == nil {
		return
	}
	if !s.alerts.offer(msg) {
		fn(msg)
	}
}

// adminAlertSink reads the sink under the lock.
func (s *Server) adminAlertSink() func(string) {
	s.alertMu.RLock()
	defer s.alertMu.RUnlock()
	return s.adminAlert
}

// Usage is the subset of the OpenAI usage object the gateway bills on.
type usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`

	// Anthropic prompt caching: reads bill at 0.1x the input price, writes at
	// 1.25x (5-minute TTL) or 2x (1-hour TTL). PromptTokens excludes these.
	CacheReadTokens       int64 `json:"-"`
	CacheCreationTokens   int64 `json:"-"`
	CacheCreation1hTokens int64 `json:"-"`
}

// merge fills in fields this chunk did not carry. Some providers split prompt
// and completion usage across chunks; replacing the struct lost the first half.
func (u *usage) merge(v usage) {
	if v.PromptTokens > 0 {
		u.PromptTokens = v.PromptTokens
	}
	if v.CompletionTokens > 0 {
		u.CompletionTokens = v.CompletionTokens
	}
	if v.CacheReadTokens > 0 {
		u.CacheReadTokens = v.CacheReadTokens
	}
	if v.CacheCreationTokens > 0 {
		u.CacheCreationTokens = v.CacheCreationTokens
	}
	if v.CacheCreation1hTokens > 0 {
		u.CacheCreation1hTokens = v.CacheCreation1hTokens
	}
}

// prepareBody returns the model and stream flag, and rewrites the body so a
// streaming call always reports usage, since the gateway cannot bill otherwise.
func prepareBody(body []byte) (model string, stream bool, maxTokens int64, out []byte, err error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	// Every number used to round-trip through float64, so a large integer
	// (a seed, an id) could reach the provider as a different value.
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return "", false, 0, nil, err
	}
	model, _ = m["model"].(string)
	stream, _ = m["stream"].(bool)
	// The declared completion size lets preflight hold enough credit before the
	// upstream is called; without it the overshoot is one call's cost.
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		n, ok := m[key].(json.Number)
		if !ok {
			continue
		}
		if v, err := n.Int64(); err == nil && v > 0 {
			maxTokens = v
			break
		}
	}

	if stream {
		opts, _ := m["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		opts["include_usage"] = true
		m["stream_options"] = opts
	}
	out, err = json.Marshal(m)
	return model, stream, maxTokens, out, err
}

// clampTokens treats a negative upstream count as zero: no real response has a
// negative token count, and carrying one into the ledger would credit the caller.
func clampTokens(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// saturatingAdd adds token counts without wrapping. The sum is what the usage
// row stores and what the schema bounds, so an overflow here used to reject the
// insert after the provider had already served and charged the call.
func saturatingAdd(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

// saturatingMul returns a*b without wrapping positive token estimates.
func saturatingMul(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

// pipeStream relays SSE to the client while watching for the usage chunk. The
// second return is the wait for the first relayed byte in milliseconds: the
// customer-perceived first-token time.
// sseHeaders prepares a streaming response.
func sseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// relaySSE copies an upstream SSE body to the client. A write that fails or
// stalls past writeStallTimeout stops the writes but NOT the reads: the usage
// chunk is what the call is billed on, so an abandoned stream is still read to
// its end and priced correctly instead of being billed from a partial count.
func relaySSE(w http.ResponseWriter, body io.Reader, start time.Time, apply func([]byte)) (int64, int64, error) {
	flusher, _ := w.(http.Flusher)
	controller := http.NewResponseController(w)
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	// cap+1 so hitting the cap is detectable: LimitReader at exactly the cap ends
	// in io.EOF, which is indistinguishable from the upstream finishing cleanly.
	reader := bufio.NewReaderSize(io.LimitReader(body, maxRelayBytes+1), 64<<10)
	var (
		ttftMs     int64
		ttftTook   bool
		clientGone bool
		received   int64
	)
	for {
		line := make([]byte, 0, 64<<10)
		var readErr error
		for {
			part, err := reader.ReadSlice('\n')
			if len(line)+len(part) > maxRelayLine {
				return ttftMs, received + int64(len(line)+len(part)), errRelayTooLong
			}
			line = append(line, part...)
			if err != bufio.ErrBufferFull {
				readErr = err
				break
			}
		}
		if len(line) == 0 {
			if errors.Is(readErr, io.EOF) {
				if received > maxRelayBytes {
					return ttftMs, received, errRelayTooLong
				}
				return ttftMs, received, nil
			}
			return ttftMs, received, readErr
		}
		received += int64(len(line))
		if len(line) > 0 {
			if !ttftTook {
				ttftMs, ttftTook = time.Since(start).Milliseconds(), true
				if ttftMs == 0 {
					ttftMs = 1 // sub-millisecond still counts as a measured wait
				}
			}
			if !clientGone {
				// A client that stops reading must not pin this handler until
				// requestDeadline: the write deadline turns the stall into a write
				// error, which ends the writes while the relay keeps reading.
				_ = controller.SetWriteDeadline(time.Now().Add(writeStallTimeout))
				if _, werr := w.Write(line); werr != nil {
					clientGone = true
				} else if flusher != nil {
					flusher.Flush()
				}
			}
			apply(line)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if received > maxRelayBytes {
					return ttftMs, received, errRelayTooLong
				}
				return ttftMs, received, nil
			}
			return ttftMs, received, readErr
		}
	}
}

func pipeStream(w http.ResponseWriter, resp *http.Response, start time.Time) (usage, int64, bool) {
	defer func() { _ = resp.Body.Close() }()
	sseHeaders(w)
	var seen usage
	ttft, received, err := relaySSE(w, resp.Body, start, func(line []byte) {
		if u, ok := usageFromSSE(line); ok {
			seen.merge(u)
		}
	})
	if err != nil {
		log.Printf("gateway: stream copy: %v", err)
	}
	// received is whether the upstream produced anything at all. A stream that
	// dies before its first byte consumed nothing, so it must not be billed —
	// the old code left delivered true and charged the whole hold for silence.
	// Delivered means bytes reached the client, not that the stream ended
	// cleanly: a relay cut by the drain still produced output the customer saw,
	// and settlement must still run for it. An over-long response (one line past
	// maxRelayLine, or the whole body past maxRelayBytes) is the upstream
	// misbehaving, so it is not charged even though bytes were relayed.
	return seen, ttft, received > 0 && !errors.Is(err, errRelayTooLong)
}

// usageFromSSE extracts usage from one "data: {...}" line, ignoring everything else.
func usageFromSSE(line []byte) (usage, bool) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return usage{}, false
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return usage{}, false
	}
	var chunk struct {
		Usage *usage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil || chunk.Usage == nil {
		return usage{}, false
	}
	return *chunk.Usage, true
}

// writeJSON relays a buffered response, stripping 9router's trailing SSE tail.
// delivered reports whether the body reached the client: a read error or the
// size cap means the caller can only answer 502, and a response the customer
// never received must not be billed.
func writeJSON(w http.ResponseWriter, resp *http.Response) (usage, bool) {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		logUpstreamFailure(resp)
		writeErr(w, resp.StatusCode, upstreamFailureMessage(resp.StatusCode))
		return usage{}, false
	}

	// The 4 MB request cap is about what we accept, not what a provider may
	// return: a large completion refused here was served and charged upstream
	// and never billed here.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRelayBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "cannot read upstream response")
		return usage{}, false
	}
	// LimitReader stops at the cap; one extra byte tells us it was truncation.
	// Relaying a truncated body would hand the client invalid JSON and hide the
	// usage object, so the call would also go unbilled.
	if len(body) > maxRelayBytes {
		writeErr(w, http.StatusBadGateway, "upstream response too large")
		return usage{}, false
	}
	// 9router appends "data: [DONE]" even for non-stream calls; cut it so strict
	// OpenAI clients can parse the body. Only a trailing marker: the literal can
	// also appear inside a JSON string, and cutting there truncated the body.
	body = bytes.TrimSpace(bytes.TrimSuffix(bytes.TrimSpace(body), []byte("data: [DONE]")))

	var probe struct {
		Usage *usage `json:"usage"`
	}
	_ = json.Unmarshal(body, &probe)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if _, werr := w.Write(body); werr != nil {
		// The body never reached the client, so this call was not delivered.
		return usage{}, false
	}

	if probe.Usage != nil && resp.StatusCode < 300 {
		return *probe.Usage, true
	}
	return usage{}, true
}

const maxUpstreamErrorExcerpt = 512

func upstreamFailureMessage(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "upstream rate limit"
	case status >= 500:
		return "upstream unavailable"
	default:
		return "upstream rejected the request"
	}
}

func logUpstreamFailure(resp *http.Response) {
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorExcerpt))
	log.Printf("gateway: upstream status %d, error body: %q", resp.StatusCode, obs.Redact(string(excerpt)))
}

// writeErr emits an OpenAI-shaped error so SDKs surface the message.
func writeErr(w http.ResponseWriter, status int, msg string) {
	body, err := json.Marshal(map[string]any{
		"error": map[string]string{"message": msg, "type": "gateway_error"},
	})
	if err != nil { // only a non-string value could fail; msg is a string
		body = []byte(`{"error":{"message":"gateway error","type":"gateway_error"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
