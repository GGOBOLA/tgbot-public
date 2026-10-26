package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// The library builds its request URL with the token in it, so the error it
// returns can contain that token. This client reproduces that shape on purpose:
// the check must not print it.
type failingClient struct{ token string }

func (f failingClient) Do(req *http.Request) (*http.Response, error) {
	return nil, errors.New("Get \"https://api.telegram.org/bot" + f.token + "/getMe\": dial tcp: connection refused")
}

func TestContractTokenMissing(t *testing.T) {
	t.Setenv("TELEGRAM_CONTRACT_TOKEN", "")
	if _, err := contractToken(); err == nil || !strings.Contains(err.Error(), "TELEGRAM_CONTRACT_TOKEN") {
		t.Fatalf("contractToken() error = %v, want missing-token error", err)
	}
}

func TestAuthFailureDoesNotExposeToken(t *testing.T) {
	const token = "fake-secret-token"
	err := runWithClient(token, 1, false, failingClient{token: token})
	if err == nil {
		t.Fatal("runWithClient() error = nil, want auth failure")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("auth error leaked token: %v", err)
	}
	if !strings.Contains(err.Error(), "telegram auth failed: check the token and network access") {
		t.Fatalf("auth error = %q, want generic auth failure", err)
	}
}
