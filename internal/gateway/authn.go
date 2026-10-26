package gateway

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"

	"tgbot/internal/store"
)

// MaxKeyLen bounds the bearer token we will hash, so a hostile client cannot
// make the gateway chew on a megabyte-long header per request.
const MaxKeyLen = 128

var (
	// ErrMissingBearer means there was no "Authorization: Bearer …" header.
	ErrMissingBearer = errors.New("missing bearer token")
	// ErrMalformedKey means the token shape was wrong.
	ErrMalformedKey = errors.New("malformed key")
	// ErrUnauthorized means the token did not resolve to an active key.
	ErrUnauthorized = errors.New("unauthorized")
)

// KeyLookup resolves an active key by its SHA-256 hash.
type KeyLookup interface {
	KeyByHash(ctx context.Context, hash []byte) (store.Key, error)
}

// Authenticator turns an Authorization header into an active key. The
// plaintext token is hashed and discarded; only the digest reaches storage.
type Authenticator struct {
	Keys KeyLookup
}

// Authenticate validates the request's bearer token.
func (a Authenticator) Authenticate(r *http.Request) (store.Key, error) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return store.Key{}, ErrMissingBearer
	}
	key := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if len(key) < 4 || len(key) > MaxKeyLen || !strings.HasPrefix(key, "sk-") {
		return store.Key{}, ErrMalformedKey
	}

	sum := sha256.Sum256([]byte(key))
	k, err := a.Keys.KeyByHash(r.Context(), sum[:])
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Key{}, ErrUnauthorized
		}
		return store.Key{}, err
	}
	return k, nil
}
