package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"tgbot/internal/ledger"
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
		offline, err := s.availability.isOffline(r.Context(), p.Model)
		if err != nil {
			log.Printf("gateway: check model availability: %v", err)
			writeErr(w, http.StatusInternalServerError, "model list unavailable")
			return
		}
		if offline {
			continue
		}
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
	s.serveRequest(w, r, func(body []byte) (requestPlan, *requestFailure) {
		var payload struct {
			Model     string `json:"model"`
			Stream    bool   `json:"stream"`
			MaxTokens int64  `json:"max_tokens"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return requestPlan{}, &requestFailure{status: http.StatusBadRequest, message: "invalid JSON body"}
		}
		if payload.Model == "" {
			return requestPlan{}, &requestFailure{status: http.StatusBadRequest, message: "model is required"}
		}
		headers := make(map[string]string, 2)
		for _, h := range []string{"anthropic-version", "anthropic-beta"} {
			if v := r.Header.Get(h); v != "" {
				headers[h] = v
			}
		}
		return requestPlan{
			path: messagesPath, model: payload.Model, body: body, headers: headers,
			preflightPromptTokens: int64(len(body) / 4), maxTokens: payload.MaxTokens,
			promptEstimate: estimatePromptTokens(body),
			respond: func(w http.ResponseWriter, resp *http.Response, start time.Time) requestResult {
				if resp.StatusCode >= 300 {
					defer func() { _ = resp.Body.Close() }()
					logUpstreamFailure(resp)
					writeErr(w, resp.StatusCode, upstreamFailureMessage(resp.StatusCode))
					return requestResult{}
				}
				isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
				var result requestResult
				switch {
				case isSSE && payload.Stream:
					result.usage, result.firstMS, result.delivered = relayClaudeStream(w, resp, start)
				case isSSE:
					result.usage, result.delivered = s.writeClaudeJSON(w, resp)
				default:
					result.usage, result.delivered = readClaudeJSON(w, resp)
				}
				result.upstreamPads = true
				return result
			},
		}, nil
	})
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
