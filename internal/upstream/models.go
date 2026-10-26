// Package upstream reads the catalogue of the OpenAI-compatible server the
// gateway resells. It is only used by the admin price screen: customers are
// shown what the operator priced, never what the upstream happens to serve.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// maxModelsBody caps the response read: a catalogue is a few kilobytes, and an
// upstream that streams megabytes should not be able to exhaust this process.
const maxModelsBody = 1 << 20

// ModelLister calls GET {base}/v1/models.
type ModelLister struct {
	base   string
	key    string
	client *http.Client
}

// NewModelLister builds a lister. baseURL is the same upstream base the gateway
// forwards to (no trailing slash needed); key may be empty for a keyless
// upstream, which is unusual but not an error here.
func NewModelLister(baseURL, key string, timeout time.Duration) *ModelLister {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &ModelLister{
		base: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		key:  strings.TrimSpace(key),
		client: &http.Client{
			Timeout: timeout,
			// The catalogue is same-host metadata; a redirect must not move the
			// request (with its bearer key) to a host the operator did not set.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) == 0 {
					return nil
				}
				if req.URL.Host != via[0].URL.Host {
					return errors.New("refusing redirect to another host")
				}
				if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
					return errors.New("refusing redirect that downgrades to http")
				}
				return nil
			},
		},
	}
}

// Models returns the upstream model ids, sorted and de-duplicated.
func (l *ModelLister) Models(ctx context.Context) ([]string, error) {
	if l == nil || l.base == "" {
		return nil, errors.New("upstream base URL is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.base+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build /v1/models request: %w", err)
	}
	if l.key != "" {
		req.Header.Set("Authorization", "Bearer "+l.key)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call /v1/models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream /v1/models returned %s", resp.Status)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxModelsBody)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode /v1/models: %w", err)
	}

	seen := make(map[string]bool, len(payload.Data))
	out := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}
