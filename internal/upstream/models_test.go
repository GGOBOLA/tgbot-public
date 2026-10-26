package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestModelsReturnsSortedUniqueIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("auth header = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[
			{"id":"b/model"},
			{"id":"a/model"},
			{"id":"b/model"},
			{"id":"  "}
		]}`))
	}))
	defer srv.Close()

	l := NewModelLister(srv.URL, "sk-test", time.Second)
	got, err := l.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "a/model" || got[1] != "b/model" {
		t.Fatalf("models = %v", got)
	}
}

func TestModelsReportsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := NewModelLister(srv.URL, "k", time.Second).Models(context.Background()); err == nil {
		t.Fatal("a 401 was accepted as a catalogue")
	}
}

func TestModelsReportsBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data": "not-a-list"}`))
	}))
	defer srv.Close()

	if _, err := NewModelLister(srv.URL, "k", time.Second).Models(context.Background()); err == nil {
		t.Fatal("malformed JSON was accepted")
	}
}

func TestModelsWithoutBaseURL(t *testing.T) {
	if _, err := NewModelLister("", "k", time.Second).Models(context.Background()); err == nil {
		t.Fatal("an empty base URL was accepted")
	}
	var nilLister *ModelLister
	if _, err := nilLister.Models(context.Background()); err == nil {
		t.Fatal("a nil lister was accepted")
	}
}

func TestModelsTimesOut(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
	}))
	defer func() { close(block); srv.Close() }()

	_, err := NewModelLister(srv.URL, "k", 50*time.Millisecond).Models(context.Background())
	if err == nil {
		t.Fatal("a hanging upstream returned a catalogue")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}
