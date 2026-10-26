package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitUntil polls cond until it holds. The dispatcher is a goroutine, so its
// effects are observed rather than assumed.
//
// The budget is generous because this runs under -race on shared CI runners,
// where draining a flood plus reading the spool file can take seconds: a 1 s
// deadline failed a green tree twice (TestDispatcherDrainsSpool and
// TestSpoolDrainsOnlyWhenChannelIsEmpty) before it was raised. A truly stuck
// dispatcher still fails, just later.
const waitBudget = 10 * time.Second

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitBudget)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not reached within %s", waitBudget)
		}
		time.Sleep(time.Millisecond)
	}
}

// Before Run there is no dispatcher, so an alert is delivered inline: that is
// how the maintenance path behaved before the queue existed, and SetAdminAlert
// with no sink must do no work at all.
func TestAlertInlineBeforeDispatcherStarts(t *testing.T) {
	s := &Server{}
	var got []string
	s.SetAdminAlert(func(msg string) { got = append(got, msg) })

	s.alert("boot")
	if len(got) != 1 || got[0] != "boot" {
		t.Fatalf("alert before Run = %q, want one inline delivery", got)
	}

	s.SetAdminAlert(nil)
	s.alert("no sink")
	if len(got) != 1 {
		t.Fatalf("a nil sink still received %q", got[1:])
	}
}

// A flood of alerts against a stalled sink must never wait for it: the request
// path hands over and returns. The queue fills, and everything past it is
// spooled to disk (TestFullQueueSpoolsInsteadOfDropping). This queue has no
// spool path, which is the degraded case: the overflow is dropped, and even
// then it is dropped without blocking.
func TestAlertQueueFloodDoesNotBlock(t *testing.T) {
	s := &Server{}
	var calls int32
	release := make(chan struct{})
	s.SetAdminAlert(func(string) {
		atomic.AddInt32(&calls, 1)
		<-release
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startAlerts(ctx)

	start := time.Now()
	for i := 0; i < alertQueueSize*8; i++ {
		s.alert("flood")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("alerting blocked for %s behind a stalled sink", elapsed)
	}
	if got := cap(s.alerts.ch); got != alertQueueSize {
		t.Fatalf("queue capacity = %d, want %d", got, alertQueueSize)
	}
	// Something is in flight on the dispatcher; the rest of the queue is full.
	// The dispatcher may dequeue one message before this read, so the bound is
	// alertQueueSize-1 rather than alertQueueSize.
	if got := len(s.alerts.ch); got < alertQueueSize-1 {
		t.Fatalf("queue holds %d, want at least %d: the flood should have filled it", got, alertQueueSize-1)
	}
	close(release)
	waitUntil(t, func() bool { return len(s.alerts.ch) == 0 })
	if got := atomic.LoadInt32(&calls); got > alertQueueSize+1 {
		t.Fatalf("sink called %d times for one flood: alerts were not bounded", got)
	}
}

// captureLog redirects the standard logger for the rest of the test and returns
// a reader for what it has written so far. The dispatcher is a goroutine, so a
// test that asserts on a log line has to observe it rather than assume it.
func captureLog(t *testing.T) func() string {
	t.Helper()
	w := &logWriter{}
	prev := log.Writer()
	log.SetOutput(w)
	t.Cleanup(func() { log.SetOutput(prev) })
	return w.String
}

// logWriter collects log output without racing the goroutines that write it.
type logWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *logWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// stopQueue cancels the dispatcher and waits for it to return. The wait is not
// politeness: t.TempDir removes the spool directory after the test function
// returns, and a dispatcher still rewriting the spool there makes that cleanup
// fail with "directory not empty".
func stopQueue(t *testing.T, q *alertQueue, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	select {
	case <-q.done:
	case <-time.After(2 * time.Second):
		t.Error("dispatcher did not stop with its context")
	}
}

// readSpool decodes the spool file as written by the gateway: one JSON string
// per line, so a multi-line alert survives the round trip.
func readSpool(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var msg string
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("spool line is not a JSON string: %q: %v", line, err)
		}
		out = append(out, msg)
	}
	return out
}

// A full queue must put the alert on disk rather than lose it. One sink call
// occupies the dispatcher for the whole flood, so everything past the in-flight
// alert plus the channel's capacity has nowhere to go but the spool.
func TestFullQueueSpoolsInsteadOfDropping(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "alerts.spool")
	q := newAlertQueue(alertQueueSize, spool)
	// The point of this test is a dispatcher that cannot get past its first
	// send. A production 5 s budget is a TIMER, and on a slow runner it fires
	// mid-test: the dispatcher then drains, a slot frees up, and the multi-line
	// alert below lands in the channel instead of the spool (a real CI failure,
	// not a hypothetical). An hour is longer than any test run, so the only way
	// the send returns is the release below.
	q.budget = time.Hour
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	// Defer order matters (LIFO): stopQueue must run AFTER the parked send is
	// released, or it cancels a dispatcher that cannot notice for an hour.
	defer stopQueue(t, &q, cancel)
	// Release even on an early Fatal, so neither stopQueue nor TempDir cleanup
	// can race a send that is still parked.
	defer close(release)
	q.start(ctx, func(string) { <-release })

	const flood = alertQueueSize * 4
	start := time.Now()
	for i := 0; i < flood; i++ {
		q.offer(fmt.Sprintf("alert %d", i))
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("offering %d alerts blocked for %s behind a stalled sink", flood, elapsed)
	}

	spooled := readSpool(t, spool)
	if want := flood - alertQueueSize - 1; len(spooled) < want {
		t.Fatalf("spool holds %d alerts, want at least %d: the overflow was dropped, not spooled", len(spooled), want)
	}
	// A spool holds customer amounts; it is created for this account only.
	if fi, err := os.Stat(spool); err != nil {
		t.Fatal(err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("spool mode = %04o, want 0600", perm)
	}
	// The count is the whole "nothing was lost" proof: one alert is in flight
	// and the channel holds at most alertQueueSize, so a spool this deep leaves
	// no room for a drop. A dropped alert would put the count below the bound.
	seen := make(map[string]bool, len(spooled))
	for _, msg := range spooled {
		if !strings.HasPrefix(msg, "alert ") {
			t.Fatalf("spool holds %q, which no request raised", msg)
		}
		if seen[msg] {
			t.Fatalf("%q is spooled twice", msg)
		}
		seen[msg] = true
	}

	// The spool is line-oriented JSON, so a multi-line alert must come back as
	// one entry, not as a broken tail.
	multi := "⚠️ Unbooked usage lost — customer 42\nfirst line\nsecond line"
	q.offer(multi)
	var found bool
	for _, msg := range readSpool(t, spool) {
		if msg == multi {
			found = true
		}
	}
	if !found {
		t.Fatalf("multi-line alert did not survive the spool round trip:\n%v", readSpool(t, spool))
	}
}

// Releasing the sink must deliver what was spooled and empty the spool: a
// spooled alert is delayed, never abandoned — the head is removed only once its
// send has returned, so a send that never completes leaves it on disk
// (TestSpoolKeepsAlertWhenSendTimesOut).
func TestDispatcherDrainsSpool(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "alerts.spool")
	q := newAlertQueue(alertQueueSize, spool)
	release := make(chan struct{})
	var mu sync.Mutex
	delivered := make(map[string]bool)
	ctx, cancel := context.WithCancel(context.Background())
	defer stopQueue(t, &q, cancel)
	q.start(ctx, func(msg string) {
		<-release
		mu.Lock()
		delivered[msg] = true
		mu.Unlock()
	})

	const flood = alertQueueSize * 4
	for i := 0; i < flood; i++ {
		q.offer(fmt.Sprintf("alert %d", i))
	}
	if len(readSpool(t, spool)) == 0 {
		t.Fatal("nothing was spooled: the flood never overflowed the queue")
	}

	close(release)
	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(delivered) == flood
	})
	waitUntil(t, func() bool { return len(readSpool(t, spool)) == 0 })
	for i := 0; i < flood; i++ {
		if msg := fmt.Sprintf("alert %d", i); !delivered[msg] {
			t.Fatalf("%q was spooled but never delivered", msg)
		}
	}
}

// A send that overruns its budget must not take the alert with it. The old
// drain popped the head first and then sent it, so a stalled sink erased the
// alert from both places: not delivered, not on disk, only a log line. The head
// now survives the send and is removed only once that send has completed.
func TestSpoolKeepsAlertWhenSendTimesOut(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "alerts.spool")
	const alert = "🔴 Unbooked usage lost — customer 42, model test/model, 900 µsen"
	line, err := json.Marshal(alert)
	if err != nil {
		t.Fatal(err)
	}
	// Seed the spool as a restart would: one alert waiting for the sink.
	if err := os.WriteFile(spool, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)

	q := newAlertQueue(alertQueueSize, spool)
	q.budget = 20 * time.Millisecond // a stalled sink, without waiting 5 s for it
	// The first attempt fails at once, so the head's backoff has to be short
	// enough to observe in a test; production waits 2 s.
	q.retryBase = 5 * time.Millisecond
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var calls int32
	var mu sync.Mutex
	var got []string
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		unblock()
		stopQueue(t, &q, cancel)
	}()
	q.start(ctx, func(msg string) {
		// The first send stalls past the budget and never delivers. Every
		// later one returns at once, which is the sink recovering.
		if atomic.AddInt32(&calls, 1) == 1 {
			<-release
			return
		}
		mu.Lock()
		got = append(got, msg)
		mu.Unlock()
	})

	waitUntil(t, func() bool { return strings.Contains(logs(), "timed out") })
	waitUntil(t, func() bool { return atomic.LoadInt32(&calls) == 1 })
	if kept := readSpool(t, spool); len(kept) != 1 || kept[0] != alert {
		t.Fatalf("spool after a timed-out send = %q, want the alert still queued", kept)
	}

	unblock()
	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})
	waitUntil(t, func() bool { return len(readSpool(t, spool)) == 0 })
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != alert {
		t.Fatalf("deliveries after the sink recovered = %q, want the alert exactly once", got)
	}
}

// Shutdown must not erase the queue. stop() used to log "dropped N queued admin
// alert(s)" and discard them, so every restart or deploy lost up to a queue's
// worth of money alerts from the record. They go to the spool instead, where the
// next start() picks them up.
func TestShutdownSpoolsQueuedAlerts(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "alerts.spool")
	logs := captureLog(t)
	q := newAlertQueue(alertQueueSize, spool)
	q.budget = 20 * time.Millisecond
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		close(release)
		stopQueue(t, &q, cancel)
	}()
	q.start(ctx, func(string) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	})

	// One alert occupies the dispatcher, so the rest land in the channel and
	// stay there until shutdown.
	q.offer("queued 0")
	<-entered
	for i := 1; i <= alertQueueSize; i++ {
		if !q.offer(fmt.Sprintf("queued %d", i)) {
			t.Fatalf("offer(%d) refused an alert while the queue was running", i)
		}
	}

	cancel()
	select {
	case <-q.done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not stop with its context")
	}

	want := make([]string, 0, alertQueueSize+1)
	for i := 0; i <= alertQueueSize; i++ {
		want = append(want, fmt.Sprintf("queued %d", i))
	}
	if got := readSpool(t, spool); !slices.Equal(got, want) {
		t.Fatalf("spool after shutdown = %q, want %q in that order", got, want)
	}
	if said := logs(); strings.Contains(said, "dropped") {
		t.Fatalf("shutdown log still claims alerts were dropped:\n%s", said)
	}
}

// FIFO across both queues. offer only appends to the spool when the channel is
// full, so anything spooled is newer than anything still queued. Draining the
// spool before the channel let alert 9 overtake alerts 1-8 once the sink
// recovered: [0 9 10 … 1 2 … 8].
func TestSpoolDrainsOnlyWhenChannelIsEmpty(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "alerts.spool")
	q := newAlertQueue(alertQueueSize, spool)
	const total = alertQueueSize * 3
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var mu sync.Mutex
	var order []string
	ctx, cancel := context.WithCancel(context.Background())
	unblock := sync.OnceFunc(func() { close(release) })
	defer func() {
		unblock()
		stopQueue(t, &q, cancel)
	}()
	q.start(ctx, func(msg string) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		mu.Lock()
		order = append(order, msg)
		mu.Unlock()
	})

	q.offer("alert 0")
	<-entered // the dispatcher is stalled with alert 0 in hand
	for i := 1; i < total; i++ {
		q.offer(fmt.Sprintf("alert %d", i))
	}
	// Both queues hold work, so a pass that empties the spool first is
	// observably out of order.
	if got := len(q.ch); got != alertQueueSize {
		t.Fatalf("channel holds %d alerts, want %d", got, alertQueueSize)
	}
	if got := len(readSpool(t, spool)); got != total-alertQueueSize-1 {
		t.Fatalf("spool holds %d alerts, want %d", got, total-alertQueueSize-1)
	}

	unblock()
	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == total
	})
	mu.Lock()
	defer mu.Unlock()
	for i, msg := range order {
		if want := fmt.Sprintf("alert %d", i); msg != want {
			t.Fatalf("delivery %d = %q, want %q: order broke between the queue and the spool\norder: %v", i, msg, want, order)
		}
	}
}

// The spool is the record of last resort only while it works. When the append
// fails, alert() used to send inline, on bill()'s goroutine, holding the
// customer's inflight slot and a global slot for as long as Telegram took (up to
// 70 s per admin). An alert is not worth a stalled customer. The journal is the
// durable record, the append is the rare failure, and offer still takes
// responsibility so the caller returns at once.
func TestSpoolFailureLogsAndDoesNotBlock(t *testing.T) {
	// A regular file where the spool's directory belongs. The append then fails
	// with ENOTDIR whatever the process runs as; a 0555 directory would still
	// be writable to root, which is how these tests run.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(blocker, alertSpoolName)
	logs := captureLog(t)

	s := &Server{}
	s.alerts = newAlertQueue(1, spool)
	release := make(chan struct{})
	var calls int32
	var mu sync.Mutex
	var inline []string
	s.SetAdminAlert(func(msg string) {
		if atomic.AddInt32(&calls, 1) == 1 {
			<-release // the queued send stalls
			return
		}
		mu.Lock()
		inline = append(inline, msg)
		mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		close(release)
		stopQueue(t, &s.alerts, cancel)
	}()
	s.startAlerts(ctx)

	s.alert("first")
	waitUntil(t, func() bool { return len(s.alerts.inflight) == 1 })
	s.alert("second") // fills the one-slot channel

	if !s.alerts.offer("third") {
		t.Fatal("offer refused an alert it journaled: the caller would send it inline")
	}
	// Newlines must not survive into the log, or a crafted alert could forge a
	// journal entry of its own.
	start := time.Now()
	s.alert("fourth line one\nfourth line two")
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("alerting blocked for %s behind a spool that refused the append", elapsed)
	}

	waitUntil(t, func() bool {
		said := logs()
		return strings.Contains(said, "admin alert journaled") &&
			strings.Contains(said, "fourth line one | fourth line two")
	})

	mu.Lock()
	defer mu.Unlock()
	if len(inline) != 0 {
		t.Fatalf("inline fallback delivered %q: the request path must never wait on the sink", inline)
	}
}

// A sink that overruns its budget but still lands must not be retried the moment
// the abandoned send returns. The old code left the head in place, took the kick
// as permission to send again, and delivered the same alert twelve times in
// 500 ms at a 20 ms budget. Retries now wait out an exponential backoff, and
// after five failed attempts the alert leaves the spool for the journal so it
// cannot starve every alert behind it.
func TestRetryBacksOffAndCaps(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "alerts.spool")
	const alert = "🔴 Unbooked usage lost — customer 42, model test/model, 900 µsen"
	line, err := json.Marshal(alert)
	if err != nil {
		t.Fatal(err)
	}
	// Seed the spool as a restart would: one alert waiting for the sink.
	if err := os.WriteFile(spool, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)

	q := newAlertQueue(alertQueueSize, spool)
	q.budget = 10 * time.Millisecond
	q.retryBase = 20 * time.Millisecond
	q.retryMax = 60 * time.Millisecond
	var calls int32
	ctx, cancel := context.WithCancel(context.Background())
	defer stopQueue(t, &q, cancel)
	started := time.Now()
	var mu sync.Mutex
	var at []time.Duration
	q.start(ctx, func(string) {
		atomic.AddInt32(&calls, 1)
		mu.Lock()
		at = append(at, time.Since(started))
		mu.Unlock()
		// Overruns the budget every time but does land: without backoff this is
		// the shape that produced a dozen duplicates.
		time.Sleep(2 * q.budget)
	})

	waitUntil(t, func() bool { return strings.Contains(logs(), "undeliverable after 5 attempts") })
	// The journal line is written first and the head removed straight after, so
	// the spool drains a moment later — still the same pass, still this test.
	waitUntil(t, func() bool { return len(readSpool(t, spool)) == 0 })

	if got := atomic.LoadInt32(&calls); got != alertSpoolAttempts {
		t.Fatalf("sink called %d times for one stubborn alert, want %d: retries must be bounded and backed off (one per %s budget would be ~%d)",
			got, alertSpoolAttempts, q.budget, 500*time.Millisecond/q.budget)
	}
	// The cap alone would still pass with the backoff set to zero (the
	// duplicates the reviewer measured), and measuring total elapsed time is
	// not enough: the sink's own sleep dominates it. The discriminator is the
	// SPACING between attempts — on a 20/40/60/60 ms schedule the first and
	// last are at least ~180 ms apart; with backoff removed they are ~80 ms
	// apart (four sink sleeps).
	mu.Lock()
	spacing := time.Duration(0)
	if len(at) >= 2 {
		spacing = at[len(at)-1] - at[0]
	}
	mu.Unlock()
	if len(at) >= 2 && spacing < 150*time.Millisecond {
		t.Fatalf("attempts spanned only %s (first %s, last %s): retries are not backing off",
			spacing, at[0], at[len(at)-1])
	}
	if got := readSpool(t, spool); len(got) != 0 {
		t.Fatalf("spool still holds %q after %d failed attempts: the head must leave so the alerts behind it move", got, alertSpoolAttempts)
	}
	if said := logs(); !strings.Contains(said, "customer 42") {
		t.Fatalf("journal line does not identify the alert it dropped:\n%s", said)
	}
}

// A restart must not strand what the previous process spooled: the dispatcher
// drains the leftover file when it starts.
func TestStartDrainsLeftoverSpool(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "alerts.spool")
	leftover := []string{
		"⚠️ Unbooked usage lost — customer 42\ncharged RM 3.00, usage never recorded",
		"⚠️ Unbillable call — customer 7, model test/model: 900 µsen not charged",
	}
	var body strings.Builder
	for _, msg := range leftover {
		line, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		body.Write(line)
		body.WriteByte('\n')
	}
	if err := os.WriteFile(spool, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	q := newAlertQueue(alertQueueSize, spool)
	var mu sync.Mutex
	var got []string
	ctx, cancel := context.WithCancel(context.Background())
	defer stopQueue(t, &q, cancel)
	q.start(ctx, func(msg string) {
		mu.Lock()
		got = append(got, msg)
		mu.Unlock()
	})

	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == len(leftover)
	})
	mu.Lock()
	defer mu.Unlock()
	for i, want := range leftover {
		if got[i] != want {
			t.Fatalf("alert %d delivered as %q, want %q (spool must be drained oldest first)", i, got[i], want)
		}
	}
	waitUntil(t, func() bool { return len(readSpool(t, spool)) == 0 })
}

// The spool is capped: an unbounded one would fill the disk that holds the
// customer ledger, so past the limit the append is refused (loudly, in the log)
// rather than growing without bound.
func TestSpoolStopsAtCap(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "alerts.spool")
	// JSON adds the two quotes and the newline, so this leaves one spare byte:
	// not enough for any alert, which is the state the cap exists to handle.
	filler, err := json.Marshal(strings.Repeat("x", alertSpoolMax-4))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spool, append(filler, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	q := newAlertQueue(alertQueueSize, spool)
	for i := 0; i < 16; i++ {
		q.spool("overflow alert that must not land", 0, "test")
	}
	fi, err := os.Stat(spool)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > alertSpoolMax {
		t.Fatalf("spool grew to %d bytes, past its %d limit", fi.Size(), alertSpoolMax)
	}
	if got := len(readSpool(t, spool)); got != 1 {
		t.Fatalf("spool holds %d entries, want 1: past the cap nothing may be appended", got)
	}
}

// The spool lives beside the database, so it inherits DB_PATH's lifecycle
// without a second env var to keep in sync.
func TestSpoolPathFollowsDBPath(t *testing.T) {
	for _, tc := range []struct{ dbPath, want string }{
		{"/var/lib/tgbot/bot.db", "/var/lib/tgbot/alerts.spool"},
		{"./data/bot.db", "data/alerts.spool"},
		{"", ""},
	} {
		s := New("127.0.0.1:0", "http://upstream", "key", nil, tc.dbPath, 1, 1)
		if got := s.alerts.spoolPath; got != tc.want {
			t.Fatalf("DB_PATH=%q: spool path %q, want %q", tc.dbPath, got, tc.want)
		}
	}
}

// Shutdown stops the dispatcher, and a billing goroutine that outlives it must
// not panic on a closed channel, block, or vanish: with the dispatcher gone, the
// alert is journaled, which is the only durable record still open.
func TestAlertAfterShutdownJournals(t *testing.T) {
	s := &Server{}
	var calls int32
	s.SetAdminAlert(func(string) { atomic.AddInt32(&calls, 1) })
	logs := captureLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	s.startAlerts(ctx)

	s.alert("live")
	waitUntil(t, func() bool { return atomic.LoadInt32(&calls) == 1 })

	cancel()
	select {
	case <-s.alerts.done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not stop with the server context")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.alert("late")
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("alert blocked after shutdown")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("sink called %d times: a post-shutdown alert must not reach it", got)
	}
	waitUntil(t, func() bool { return strings.Contains(logs(), "late") })
}

// TestRunStartsAlertDispatcher pins the load-bearing line at the top of Run.
// Delete `s.startAlerts(ctx)` and every other test stays green while production
// silently reverts to an inline send on the request goroutine, which still holds
// the customer's per-customer slot and the 15 s billing context. Only the real
// entry point — a real listener, a real HTTP client — can observe that.
//
// The bound port is not exported, so this reserves an ephemeral port by binding
// and closing it and hands that address to Run; a bounded readiness poll covers
// the window in which it could be reused, and a bind failure is reported the
// moment Run returns instead of after the poll times out.
func TestRunStartsAlertDispatcher(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// No usage reported: bill() charges the hold and raises the alert.
		_, _ = io.WriteString(w, `{}`)
	})

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	h.srv.addr = addr

	entered := make(chan struct{}, 1)
	h.srv.SetAdminAlert(func(string) {
		select {
		case entered <- struct{}{}:
		default:
		}
		time.Sleep(2 * time.Second) // the stalled Telegram send
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- h.srv.Run(ctx) }()

	// /healthz is served by the same guard(mux) chain and needs no key, so it
	// is the readiness signal.
	healthURL := "http://" + addr + "/healthz"
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for {
		resp, err := http.Get(healthURL)
		lastErr = err
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case rerr := <-runErr:
			t.Fatalf("Run returned before serving: %v", rerr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway did not serve /healthz within 2s (last error %v)", lastErr)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// One chat call over the wire. The upstream reports no usage, so bill()
	// charges the pre-flight hold and raises the missing-usage alert.
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+chatPath,
		strings.NewReader(`{"model":"test/model","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat call: status %d, want 200", resp.StatusCode)
	}
	// Half the sink's 2 s: an inline send cannot finish inside this bound, and
	// a real call needs a few milliseconds.
	if elapsed > time.Second {
		t.Fatalf("a chat call waited %s behind a 2s alert sink: Run did not start the dispatcher", elapsed)
	}
	t.Logf("chat call returned in %s behind a 2s alert sink", elapsed)

	// Fast is not enough: the alert must still have left the queue.
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the alert never reached the sink")
	}

	// Shutdown still completes without waiting for the stalled send.
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// Run must not return until the dispatcher has spooled what the queue still
// holds. It used to return the instant srv.Shutdown came back, so a restart
// exited while the dispatcher was still stuck in an abandoned send: stop() ran
// milliseconds later, into a process that was already gone, and the alerts it
// "spooled at shutdown" were never written. A restart must not lose them.
func TestRunWaitsForDispatcherShutdown(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.srv.addr = l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// One abandoned send has to outlive Shutdown for the window to exist at all.
	h.srv.alerts.budget = 100 * time.Millisecond

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	h.srv.SetAdminAlert(func(string) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release // the stalled Telegram send, abandoned at the budget
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- h.srv.Run(ctx) }()

	// Only reachable once Run has started the dispatcher, so nothing is offered
	// inline before it exists.
	waitUntil(t, func() bool {
		h.srv.alerts.mu.Lock()
		defer h.srv.alerts.mu.Unlock()
		return h.srv.alerts.state == alertRunning
	})

	h.srv.alert("in flight")
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the alert never reached the sink")
	}
	want := []string{"in flight"}
	for i := 0; i < alertQueueSize; i++ {
		msg := fmt.Sprintf("queued %d", i)
		h.srv.alert(msg)
		want = append(want, msg)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	// No sleeps: this is the state Run left behind the moment it returned.
	if got := readSpool(t, alertSpoolPath(h.dbPath)); !slices.Equal(got, want) {
		t.Fatalf("spool after Run returned = %q, want %q in that order", got, want)
	}
}
