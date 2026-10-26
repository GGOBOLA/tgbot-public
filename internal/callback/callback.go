// Package callback signs button payloads so a client cannot forge or replay one
// with different values. The payload is the visible action string; the token is
// that payload plus a truncated HMAC-SHA256 over it.
package callback

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

const (
	// MaxToken bounds what Verify will parse.
	MaxToken = 512
	// MaxPayload bounds the unsigned payload.
	MaxPayload = 256
	// MinSecret is the shortest key accepted.
	MinSecret = 16
	// tagBytes is the truncated HMAC width. 12 bytes is 96 bits: infeasible to
	// forge, and it keeps every Telegram button under the 64-byte data limit.
	// The payload travels raw (its alphabet excludes '.'), because base64 of
	// "admin_view_customer:<10-digit id>:0" alone overflowed 64 bytes.
	tagBytes = 12
)

var (
	// ErrMalformed means the token is not payload.signature.
	ErrMalformed = errors.New("malformed callback token")
	// ErrTooLong means the token exceeds MaxToken.
	ErrTooLong = errors.New("callback token too long")
	// ErrSignature means the HMAC did not match.
	ErrSignature = errors.New("callback signature mismatch")
	// ErrShortSecret means the codec key is too small to be meaningful.
	ErrShortSecret = errors.New("callback secret must be at least 16 bytes")
)

// Codec signs and verifies callback payloads.
type Codec struct {
	secret []byte
}

// New builds a codec. The secret is copied, so the caller may reuse its buffer.
func New(secret []byte) (*Codec, error) {
	if len(secret) < MinSecret {
		return nil, ErrShortSecret
	}
	cp := make([]byte, len(secret))
	copy(cp, secret)
	return &Codec{secret: cp}, nil
}

// Sign returns payload "." base64url(hmac[:tagBytes]).
func (c *Codec) Sign(payload string) (string, error) {
	if payload == "" || len(payload) > MaxPayload || !safePayload(payload) {
		return "", ErrMalformed
	}
	sig := c.mac([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(sig[:tagBytes]), nil
}

// safePayload keeps the payload inside Telegram's callback alphabet and free of
// the separator, so Verify can split on the last '.' unambiguously.
func safePayload(p string) bool {
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == ':' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// Verify returns the payload only when the signature matches.
func (c *Codec) Verify(token string) (string, error) {
	if len(token) > MaxToken {
		return "", ErrTooLong
	}
	sep := strings.LastIndex(token, ".")
	if sep <= 0 || sep == len(token)-1 {
		return "", ErrMalformed
	}
	payload, sigPart := token[:sep], token[sep+1:]
	if len(payload) > MaxPayload || !safePayload(payload) {
		return "", ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil || len(sig) != tagBytes {
		return "", ErrMalformed
	}
	// Reject non-canonical encodings. Base64 leaves slack in the final
	// character, so without this a token could be edited and still decode to
	// the same bytes — signature malleability.
	if base64.RawURLEncoding.EncodeToString(sig) != sigPart {
		return "", ErrMalformed
	}

	want := c.mac([]byte(payload))
	// Constant-time compare; a byte-by-byte check leaks the prefix.
	if !hmac.Equal(sig, want[:tagBytes]) {
		return "", ErrSignature
	}
	return payload, nil
}

func (c *Codec) mac(p []byte) []byte {
	h := hmac.New(sha256.New, c.secret)
	h.Write(p)
	return h.Sum(nil)
}
