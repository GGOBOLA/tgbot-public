package gateway

import (
	"context"
	"errors"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Upstream failures worth retrying: a transient 5xx (9router surfaces the provider's
// "No ready deployment" as 503) or a connection that never opened.
const (
	maxUpstreamAttempts = 3
	retryBaseDelay      = 400 * time.Millisecond
)

// replayable reports whether a transport error proves the request never reached
// the provider. Only a dial failure qualifies. ResponseHeaderTimeout does not:
// the body was fully sent and the provider may already be generating, so a
// replay would pay for a second generation and bill one.
func replayable(err error) bool {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return false
	}
	var oe *net.OpError
	if errors.As(ue.Err, &oe) && oe.Op == "dial" {
		return true
	}
	return false
}

// retryableStatus reports whether a 5xx is safe to replay. Only 503 qualifies:
// it is 9router's "no ready deployment", so no provider work has started. A 500
// or 502 may have produced (and billed) tokens already, and replaying it would
// pay for a second generation the customer is charged for once.
func retryableStatus(code int) bool {
	return code == http.StatusServiceUnavailable
}

// forwardWithRetry sends body to path and retries transient upstream failures with
// a short backoff. Every attempt replays the same body, so attempts only happen
// before the response is handed to the client: the caller never sees a half-stream.
func (s *Server) forwardWithRetry(ctx context.Context, path string, body []byte, headers map[string]string) (final *http.Response, ferr error) {
	var (
		lastErr    error
		failures   int
		lastStatus int // 0 means the request never reached the provider
	)

	// One row per affected customer request, not per attempt: /today reads as
	// "how many calls did the provider hurt", which is the number an admin acts on.
	defer func() {
		if failures == 0 {
			return
		}
		recovered := ferr == nil && final != nil && final.StatusCode < 500
		// The request context may already be cancelled; the record still matters.
		// WithoutCancel strips the deadline, so give the write a budget of its own:
		// otherwise a blocked SQLite write holds the handler and its slot forever.
		rctx, rc := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer rc()
		if err := s.db.RecordUpstreamFailure(rctx, path, lastStatus, failures, recovered); err != nil {
			log.Printf("gateway: %v", err)
		}
	}()

	for attempt := 1; attempt <= maxUpstreamAttempts; attempt++ {
		if attempt > 1 {
			// Jitter, because up to maxConcurrent callers hit the same outage and
			// used to retry in lockstep, re-loading the provider at the same
			// instant. +/-20% spreads them out.
			delay := retryBaseDelay * time.Duration(1<<(attempt-2)) // 400ms, 800ms
			spread := int64(delay) / 5
			if spread > 0 {
				// #nosec G404 -- retry jitter only; nothing here is a secret or
				// a token, and a crypto RNG per retry buys nothing.
				delay += time.Duration(rand.Int64N(spread+1)) - time.Duration(spread/2)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		resp, err := s.forwardTo(ctx, path, body, headers)
		if err != nil {
			lastErr = err
			failures++
			lastStatus = 0
			if ctx.Err() != nil {
				return nil, err
			}
			// A dial failure never reached the provider; anything else may have
			// been sent, so the call is not safe to replay.
			if !replayable(err) {
				log.Printf("gateway: upstream attempt %d/%d not replayable: %v", attempt, maxUpstreamAttempts, err)
				return nil, err
			}
			log.Printf("gateway: upstream attempt %d/%d failed: %v", attempt, maxUpstreamAttempts, err)
			continue
		}

		if !retryableStatus(resp.StatusCode) {
			return resp, nil
		}
		failures++
		lastStatus = resp.StatusCode
		if attempt == maxUpstreamAttempts {
			return resp, nil
		}

		// Drain and close before replaying, otherwise the connection leaks.
		// Log the status only: an upstream error body can echo request context,
		// including the internal bearer key.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		log.Printf("gateway: upstream %d on attempt %d/%d, retrying",
			resp.StatusCode, attempt, maxUpstreamAttempts)
	}

	// Every terminal 5xx returned above; anything reaching here is a transport
	// failure. Returning the drained response instead would hide it.
	return nil, lastErr
}
