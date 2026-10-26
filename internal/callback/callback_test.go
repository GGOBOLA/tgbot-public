package callback

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func newCodec(t *testing.T) *Codec {
	t.Helper()
	c, err := New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRoundTrip(t *testing.T) {
	c := newCodec(t)
	for _, payload := range []string{"menu", "reload:1000000000", "key:revoke:7", "admin_view_customer:-1001234567890:0"} {
		token, err := c.Sign(payload)
		if err != nil {
			t.Fatalf("%q: %v", payload, err)
		}
		got, err := c.Verify(token)
		if err != nil {
			t.Fatalf("%q verify: %v", payload, err)
		}
		if got != payload {
			t.Fatalf("round trip = %q, want %q", got, payload)
		}
	}
}

func TestRejectsTamperedPayload(t *testing.T) {
	c := newCodec(t)
	token, err := c.Sign("reload:100")
	if err != nil {
		t.Fatal(err)
	}
	body, sig, _ := strings.Cut(token, ".")

	// Re-encode a different payload under the same signature.
	forged := "reload:999999"
	alt := base64.RawURLEncoding.EncodeToString([]byte(forged)) + "." + sig
	if alt == body+"."+sig {
		t.Fatal("test did not change the payload")
	}
	if _, err := c.Verify(alt); !errors.Is(err, ErrSignature) {
		t.Fatalf("err = %v, want ErrSignature", err)
	}
}

func TestRejectsWrongKey(t *testing.T) {
	a := newCodec(t)
	b, err := New([]byte("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := a.Sign("credit:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Verify(token); !errors.Is(err, ErrSignature) {
		t.Fatalf("err = %v, want ErrSignature", err)
	}
}

func TestRejectsMalformed(t *testing.T) {
	c := newCodec(t)
	for _, token := range []string{"", "nodot", ".", "a.", ".b", "a.b.c"} {
		if _, err := c.Verify(token); err == nil {
			t.Fatalf("%q was accepted", token)
		}
	}
}

func TestRejectsOversize(t *testing.T) {
	c := newCodec(t)
	if _, err := c.Sign(strings.Repeat("x", MaxPayload+1)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("sign oversize = %v, want ErrMalformed", err)
	}
	if _, err := c.Verify(strings.Repeat("x", MaxToken+1)); !errors.Is(err, ErrTooLong) {
		t.Fatalf("verify oversize = %v, want ErrTooLong", err)
	}
}

func TestRejectsShortSecret(t *testing.T) {
	if _, err := New([]byte("short")); !errors.Is(err, ErrShortSecret) {
		t.Fatalf("err = %v, want ErrShortSecret", err)
	}
}

func TestSecretIsCopied(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	c, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	for i := range key {
		key[i] = 'x'
	}
	token, err := c.Sign("menu")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Verify(token); err != nil {
		t.Fatalf("codec depended on the caller's buffer: %v", err)
	}
}

func FuzzVerify(f *testing.F) {
	f.Add("menu")
	f.Add("reload:100")
	f.Add("a.b")
	f.Fuzz(func(t *testing.T, token string) {
		c := &Codec{secret: []byte("0123456789abcdef0123456789abcdef")}
		got, err := c.Verify(token)
		if err != nil {
			return
		}
		// Anything Verify accepts must round-trip.
		again, err := c.Sign(got)
		if err != nil {
			t.Fatalf("accepted payload cannot be re-signed: %q", got)
		}
		if _, err := c.Verify(again); err != nil {
			t.Fatalf("re-signed token rejected: %v", err)
		}
	})
}

// Telegram rejects callback_data over 64 bytes, so the worst case a real button
// can produce has to fit. The old design base64'd the payload and overflowed.
func TestWorstCaseTokenFitsTelegram(t *testing.T) {
	c := newCodec(t)
	// Longest action name, a 10-digit Telegram id, an 11-digit argument.
	payload := "admin_view_customer:-1234567890:42949672950"
	token, err := c.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) > 64 {
		t.Fatalf("token is %d bytes, Telegram allows 64: %q", len(token), token)
	}
	if got, err := c.Verify(token); err != nil || got != payload {
		t.Fatalf("round trip: %q %v", got, err)
	}
}

// Non-ASCII cannot appear in a payload: the signer refuses it rather than
// producing bytes Telegram will reject at send time.
func TestSignRejectsUnsafePayload(t *testing.T) {
	c := newCodec(t)
	for _, payload := range []string{"привет", "a.b", "a b", "a/b", "a+b"} {
		if _, err := c.Sign(payload); !errors.Is(err, ErrMalformed) {
			t.Fatalf("%q accepted: %v", payload, err)
		}
	}
}
