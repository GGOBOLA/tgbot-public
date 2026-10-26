package bot

import (
	"context"
	"sort"
	"sync"
	"time"

	"tgbot/internal/store"
)

// modelLister is the slice of *upstream.ModelLister the bot needs, so tests can
// drive the catalogue without a network.
type modelLister interface {
	Models(ctx context.Context) ([]string, error)
}

// catalogue caches the upstream model list. Only the admin price screen reads
// it: customers are shown what the operator priced, never what the upstream
// happens to serve.
type catalogue struct {
	mu        sync.Mutex
	lister    modelLister
	ttl       time.Duration
	attempted time.Time
	models    []string
	err       error
}

func newCatalogue(l modelLister) *catalogue {
	return &catalogue{lister: l, ttl: time.Minute}
}

// list returns the catalogue, refetching when the cache is stale or force is
// set. A failed fetch is cached too, so a broken upstream is asked once a
// minute instead of on every tap; the bot then renders the priced list alone.
func (c *catalogue) list(ctx context.Context, force bool) ([]string, error) {
	if c == nil || c.lister == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && !c.attempted.IsZero() && time.Since(c.attempted) < c.ttl {
		return c.models, c.err
	}
	models, err := c.lister.Models(ctx)
	c.attempted = time.Now()
	if err != nil {
		c.err = err
		return nil, err
	}
	c.models, c.err = models, nil
	return models, nil
}

// unpricedUpstream returns the upstream models that have no price yet, sorted.
// A model the operator already priced is never offered twice.
func unpricedUpstream(models []string, priced []store.ModelPrice) []string {
	if len(models) == 0 {
		return nil
	}
	have := make(map[string]bool, len(priced))
	for _, p := range priced {
		have[p.Model] = true
	}
	out := make([]string, 0, len(models))
	for _, m := range models {
		if !have[m] {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}
