package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"tgbot/internal/store"
)

const modelProbeTimeout = 15 * time.Second
const modelAvailabilityTick = time.Second

var modelProbeBackoff = [...]time.Duration{
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	30 * time.Minute,
}

type modelAvailability struct {
	db      *store.DB
	probe   func(context.Context, string) error
	now     func() time.Time
	backoff []time.Duration
}

func (s *Server) markModelOffline(ctx context.Context, model string) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.availability.takeOffline(writeCtx, model); err != nil {
		return err
	}
	select {
	case s.availabilityWake <- struct{}{}:
	default:
	}
	return nil
}

func (s *Server) probeOfflineModels(ctx context.Context) {
	if err := s.availability.probeDue(ctx); err != nil && ctx.Err() == nil {
		log.Printf("gateway: model availability check: %v", err)
	}
}

func (s *Server) classifyProviderResult(ctx context.Context, model string, status int) {
	if status != http.StatusServiceUnavailable {
		return
	}
	if err := s.markModelOffline(ctx, model); err != nil {
		log.Printf("gateway: take Model offline: %v", err)
	}
}

func (a *modelAvailability) isOffline(ctx context.Context, model string) (bool, error) {
	return a.db.IsModelOffline(ctx, model)
}

func (a *modelAvailability) takeOffline(ctx context.Context, model string) error {
	return a.db.TakeModelOffline(ctx, model, a.now())
}

func (a *modelAvailability) probeDue(ctx context.Context) error {
	now := a.now()
	models, err := a.db.DueModelProbes(ctx, now)
	if err != nil {
		return err
	}
	for _, model := range models {
		probeCtx, cancel := context.WithTimeout(ctx, modelProbeTimeout)
		err := a.probe(probeCtx, model.Model)
		cancel()
		if err == nil {
			if err := a.db.RestoreModel(ctx, model.Model); err != nil {
				return err
			}
			logModelAvailability("restored", model.Model)
			continue
		}
		failures := model.ProbeFailures + 1
		if len(a.backoff) == 0 {
			return errors.New("model probe backoff is empty")
		}
		backoffIndex := failures - 1
		if backoffIndex >= len(a.backoff) {
			backoffIndex = len(a.backoff) - 1
		}
		delay := a.backoff[backoffIndex]
		if err := a.db.RecordModelProbeFailure(ctx, model.Model, a.now().Add(delay)); err != nil {
			return err
		}
		log.Printf("gateway: Model %q probe failed: %v", model.Model, err)
	}
	return nil
}

func (s *Server) probeModel(ctx context.Context, model string) error {
	body, err := json.Marshal(struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		MaxTokens int `json:"max_tokens"`
	}{
		Model: model,
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{{Role: "user", Content: "Reply with one character."}},
		MaxTokens: 1,
	})
	if err != nil {
		return err
	}

	resp, err := s.forwardTo(ctx, chatPath, body, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("upstream rejected model health probe")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRelayBytes+1))
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > maxRelayBytes {
		return errors.New("upstream returned an empty or oversized health probe")
	}
	return nil
}

func logModelAvailability(action, model string) {
	// %q escapes any control characters in the request-derived model id.
	log.Printf("gateway: model %q %s", model, action)
}
