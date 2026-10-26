package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRedactingLogOutput covers the M2 wiring: the process-wide stdlib logger
// must scrub credentials, because the gateway logs through log.Printf and only
// the obs.Logger used by internal/bot redacts on its own.
func TestRedactingLogOutput(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(redactingLogOutput(&buf), "", 0)
	logger.Printf("authorized with bot123456789:AAHfoo_bar-baz1234567890 (sk-abcdef1234567890abcd)")

	got := buf.String()
	if strings.Contains(got, "AAHfoo_bar") || strings.Contains(got, "abcdef1234567890abcd") {
		t.Fatalf("stdlib log output leaked a credential: %q", got)
	}
	if !strings.Contains(got, "bot<redacted>") || !strings.Contains(got, "sk-<redacted>") {
		t.Fatalf("placeholders missing: %q", got)
	}
}

// TestSetupLoggingPinsTheWiring is the test the audit's finding asked for: the
// seam in run() -- not just the writer -- must be pinned. TestRedactingLogOutput
// passes even if the SetOutput call is deleted, because it builds its own
// logger; this one logs through the global logger and checks what that logger
// actually writes to, so removing the wiring fails here.
func TestSetupLoggingPinsTheWiring(t *testing.T) {
	prev := log.Writer()
	t.Cleanup(func() { log.SetOutput(prev) })

	var buf bytes.Buffer
	out := setupLogging(&buf)

	log.Printf("upstream rejected key sk-abcdef1234567890abcd")

	// (a) the line reached the writer run() installs.
	got := buf.String()
	if !strings.Contains(got, "upstream rejected key") {
		t.Fatalf("log.Printf did not reach the installed writer: %q", got)
	}
	// (b) the credential in it was scrubbed.
	if strings.Contains(got, "abcdef1234567890abcd") {
		t.Fatalf("global logger leaked an api key: %q", got)
	}
	if !strings.Contains(got, "sk-<redacted>") {
		t.Fatalf("redaction placeholder missing: %q", got)
	}
	// (c) the global logger's output is exactly the writer setupLogging
	// returned. Without this, a test could green on a writer some other code
	// installed while the SetOutput call in setupLogging was gone.
	if log.Writer() != out {
		t.Fatalf("global logger output is %T, not the writer setupLogging returned", log.Writer())
	}
}

// TestRunComponentsStopsOnFirstError pins the fix for the audit's H2: when the
// gateway listener cannot bind, gw.Run returns at once and the process used to
// keep running with the error parked in a buffered channel, so systemd saw a
// healthy unit with no gateway behind it. The first non-nil result must cancel
// the shared context and the call must return that error once every component
// has stopped.
func TestRunComponentsStopsOnFirstError(t *testing.T) {
	bindErr := errors.New("bind failed")
	secondStarted := make(chan struct{})

	fns := []func(context.Context) error{
		func(context.Context) error { return bindErr },
		func(ctx context.Context) error {
			close(secondStarted)
			<-ctx.Done()
			return ctx.Err()
		},
	}

	done := make(chan error, 1)
	go func() { done <- runComponents(context.Background(), fns...) }()

	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("the second component never started")
	}

	select {
	case err := <-done:
		if !errors.Is(err, bindErr) {
			t.Fatalf("runComponents returned %v, want %v", err, bindErr)
		}
	case <-time.After(time.Second):
		t.Fatal("runComponents did not return within 1s of a component failing: " +
			"the process would stay up with no gateway")
	}
}

// TestRunComponentsStopsOnContextCancel covers the shutdown path the fix must
// keep: a signal cancels the context, every component stops, and only then does
// run() return (and close the store).
func TestRunComponentsStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	var stopped atomic.Int32

	fns := []func(context.Context) error{
		func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			stopped.Add(1)
			return nil
		},
		func(ctx context.Context) error {
			<-ctx.Done()
			stopped.Add(1)
			return ctx.Err()
		},
	}

	done := make(chan error, 1)
	go func() { done <- runComponents(ctx, fns...) }()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("components never started")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runComponents returned %v, want the cancelled component's error", err)
		}
		if stopped.Load() != 2 {
			t.Fatalf("runComponents returned before both components stopped (stopped=%d)", stopped.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("runComponents did not return after the context was cancelled")
	}
}
