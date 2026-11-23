package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type failingClientWriter struct{ header http.Header }

func (w *failingClientWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingClientWriter) WriteHeader(int) {}
func (w *failingClientWriter) Write([]byte) (int, error) {
	return 0, errors.New("client connection closed")
}

func TestClaudeJSONWriteFailureIsNotDelivered(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":3,"output_tokens":4}}`)),
	}
	_, delivered := readClaudeJSON(&failingClientWriter{}, resp)
	if delivered {
		t.Fatal("a failed Claude JSON write was reported as delivered")
	}

	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m","model":"m","role":"assistant","usage":{"input_tokens":3}}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	resp = &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(stream)),
	}
	_, delivered = (&Server{}).writeClaudeJSON(&failingClientWriter{}, resp)
	if delivered {
		t.Fatal("a failed assembled Claude JSON write was reported as delivered")
	}
}

func TestClaudeStreamOverlongResponseIsNotDelivered(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader("data: {\"type\":\"message_start\"}\n\n" +
			strings.Repeat("x", maxRelayLine+1))),
	}
	_, _, delivered := relayClaudeStream(httptest.NewRecorder(), resp, time.Now())
	if delivered {
		t.Fatal("an overlong Claude response was reported as delivered")
	}
}

func TestChatBodyRequiresOneJSONValue(t *testing.T) {
	for _, body := range []string{
		`{"model":"m"} trailing`,
		`{"model":"m"} {"model":"other"}`,
	} {
		if _, _, _, _, err := prepareBody([]byte(body)); err == nil {
			t.Errorf("prepareBody accepted trailing data: %q", body)
		}
	}
	if _, _, _, _, err := prepareBody([]byte(`{"model":"m"}`)); err != nil {
		t.Fatalf("valid JSON rejected: %v", err)
	}
}

func TestUpstreamFourHundredAfterRetryIsNotRecovered(t *testing.T) {
	var h *harness
	h = newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if h.upCalls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	})
	rec := h.do(t, `{"model":"test/model","messages":[]}`, testKey)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	summary, err := h.store.UpstreamSummary(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 1 || summary.Recovered != 0 || summary.GaveUp != 1 {
		t.Fatalf("summary = %+v, want one failure, not a recovery", summary)
	}
}
