package gateway

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tgbot/internal/ratelimit"
	"tgbot/internal/store"
)

type fakeKeys struct {
	want []byte
	key  store.Key
	err  error
}

func (f fakeKeys) KeyByHash(_ context.Context, hash []byte) (store.Key, error) {
	if f.err != nil {
		return store.Key{}, f.err
	}
	if string(hash) != string(f.want) {
		return store.Key{}, store.ErrNotFound
	}
	return f.key, nil
}

func TestRateLimitIsPerCustomerNotPerKey(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":10,"completion_tokens":0}}`)
	})
	// Burst 2, effectively no refill: the whole budget for this customer.
	h.srv.limiter = ratelimit.New(0.0001, 2)

	ctx := context.Background()
	other, _, err := h.store.CreateKey(ctx, 42, "second")
	if err != nil {
		t.Fatalf("second key for the same customer: %v", err)
	}

	body := `{"model":"test/model","messages":[{"role":"user","content":"hi"}]}`
	// Two keys, one customer: together they may spend the two-token budget and
	// no more. Bucketing on the key id would hand each key its own bucket and
	// let the customer through with four.
	for _, key := range []string{testKey, other} {
		if rec := h.do(t, body, key); rec.Code != http.StatusOK {
			t.Fatalf("first call on a key: code %d, body %s", rec.Code, rec.Body)
		}
	}
	rec := h.do(t, body, testKey)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third call across the customer's keys: code %d, want 429 (bucketed per key, not per customer)", rec.Code)
	}
	if h.upCalls != 2 {
		t.Fatalf("upstream called %d times, want 2 (throttled call must not forward)", h.upCalls)
	}
}

func TestAuthenticate(t *testing.T) {
	plain := "sk-" + strings.Repeat("a", 64)
	sum := sha256.Sum256([]byte(plain))

	a := Authenticator{Keys: fakeKeys{
		want: sum[:],
		key:  store.Key{ID: 1, TelegramID: 99, Active: true},
	}}

	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Authorization", "Bearer "+plain)
	got, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if got.TelegramID != 99 {
		t.Fatalf("key = %+v", got)
	}
}

func TestAuthenticateRejections(t *testing.T) {
	a := Authenticator{Keys: fakeKeys{}}

	cases := []struct {
		name string
		auth string
		want error
	}{
		{"no header", "", ErrMissingBearer},
		{"wrong scheme", "Basic abc", ErrMissingBearer},
		{"short", "Bearer sk-", ErrMalformedKey},
		{"no prefix", "Bearer " + strings.Repeat("a", 64), ErrMalformedKey},
		{"too long", "Bearer sk-" + strings.Repeat("a", 200), ErrMalformedKey},
		{"unknown", "Bearer sk-" + strings.Repeat("b", 64), ErrUnauthorized},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/", nil)
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		if _, err := a.Authenticate(r); !errors.Is(err, c.want) {
			t.Fatalf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}
