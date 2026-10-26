package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// alertQueueSize bounds how many admin alerts may wait for the dispatcher.
	// Alerts are rare (missing usage, unbooked charges, provider trouble), so
	// eight rides out a stalled Telegram send; a billing storm beyond that goes
	// to the spool rather than being parked in front of the request path.
	alertQueueSize = 8
	// alertSendTimeout is one sink call's budget. The Telegram client's own
	// timeout is 70 s, which is far longer than a queue slot should be held.
	alertSendTimeout = 5 * time.Second
	// alertSpoolName is the file beside DB_PATH that holds alerts the queue
	// could not take, one JSON-encoded message per line.
	alertSpoolName = "alerts.spool"
	// alertSpoolMax caps the spool at 1 MiB. Past it the append is refused and
	// the alert goes to the journal instead: an unbounded spool would fill the
	// disk that holds the customer ledger, and a spool that large already means
	// nobody is reading it.
	alertSpoolMax = 1 << 20
	// alertSpoolAttempts caps how often one spooled alert is sent before it
	// leaves the spool with the journal as its record. Without a cap, a sink
	// that accepts nothing blocks every alert behind it forever.
	alertSpoolAttempts = 5
	// alertRetryBase and alertRetryMax bound the backoff between attempts at a
	// head that failed: 2 s, 4 s, 8 s … capped at 60 s. Retrying at once is
	// worse than useless: a sink that overran its budget has usually delivered
	// the message anyway, so the immediate re-kick sent it again.
	alertRetryBase = 2 * time.Second
	alertRetryMax  = 60 * time.Second
	// alertDrainTimeout bounds how long Run waits on the way out for the
	// dispatcher to spool what the queue still holds.
	alertDrainTimeout = 10 * time.Second
)

// alertState is the queue's lifecycle. The zero value is idle, which is what a
// Server that never ran looks like.
type alertState int

const (
	// alertIdle: no dispatcher has been started. Callers deliver inline, which
	// is the pre-run behaviour (and what the maintenance tests exercise).
	alertIdle alertState = iota
	alertRunning
	// alertStopped: the server's context ended. Late alerts from billing
	// goroutines that outlived shutdown go to the spool (the dispatcher has
	// already drained its share) or the journal: ch is never closed, so a send
	// cannot panic.
	alertStopped
)

// alertQueue is the hand-off between the request path and the admin sink. One
// goroutine drains it, so a slow Telegram send delays only later alerts and
// never a customer's bill; and however many requests alert, there is still one
// sender goroutine.
type alertQueue struct {
	mu    sync.Mutex
	state alertState
	ch    chan string
	done  chan struct{} // closed when the dispatcher goroutine returns
	// inflight holds a single token while a sink call is outstanding. It is
	// what bounds abandoned sends to one: a sink that ignores its budget does
	// not get a new goroutine for every alert behind it.
	inflight chan struct{}
	// kick wakes the dispatcher when an abandoned send finally returns, so a
	// spooled alert is retried as soon as the sink recovers instead of waiting
	// for the next alert to arrive. One pending kick is enough: it only means
	// "look at the queues again".
	kick chan struct{}
	// spoolPath is the on-disk queue for alerts the channel could not take. An
	// empty path (no DB_PATH) turns spooling off, which leaves the old drop.
	spoolPath string
	// spoolMu guards every read and write of the spool file. offer holds q.mu
	// while appending; the dispatcher drains under spoolMu alone, so spoolMu is
	// a leaf lock and the two orders cannot interleave into a deadlock.
	spoolMu sync.Mutex
	// budget overrides alertSendTimeout. Tests set it before start so a stalled
	// sink costs milliseconds instead of five seconds; production leaves it at
	// the default.
	budget time.Duration
	// retryBase and retryMax override the backoff bounds for the same reason:
	// a test that watches five attempts cannot spend two minutes doing it.
	retryBase time.Duration
	retryMax  time.Duration
}

// sendBudget is one sink call's allowance. A queue that never had newAlertQueue
// called on it (a hand-built zero value) still gets the production default.
func (q *alertQueue) sendBudget() time.Duration {
	if q.budget > 0 {
		return q.budget
	}
	return alertSendTimeout
}

// backoff is the wait before retrying a head that has failed attempts times:
// base, 2×base, 4×base … capped at max.
func (q *alertQueue) backoff(attempts int) time.Duration {
	base, max := q.retryBase, q.retryMax
	if base <= 0 {
		base = alertRetryBase
	}
	if max <= 0 {
		max = alertRetryMax
	}
	d := base
	for i := 1; i < attempts && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

func newAlertQueue(size int, spoolPath string) alertQueue {
	return alertQueue{
		ch:        make(chan string, size),
		done:      make(chan struct{}),
		inflight:  make(chan struct{}, 1),
		kick:      make(chan struct{}, 1),
		spoolPath: spoolPath,
	}
}

// start launches the single dispatcher. It is safe to call more than once and
// safe to call after shutdown: only the first call while idle wins.
func (q *alertQueue) start(ctx context.Context, send func(string)) {
	q.mu.Lock()
	if q.state != alertIdle {
		q.mu.Unlock()
		return
	}
	if q.ch == nil {
		q.ch = make(chan string, alertQueueSize)
	}
	if q.done == nil {
		q.done = make(chan struct{})
	}
	if q.inflight == nil {
		q.inflight = make(chan struct{}, 1)
	}
	if q.kick == nil {
		q.kick = make(chan struct{}, 1)
	}
	q.state = alertRunning
	q.mu.Unlock()
	go q.dispatch(ctx, send)
}

// offer hands msg to the dispatcher. It reports whether the queue took
// responsibility for it. The answer is false in exactly one state: alertIdle,
// before Run starts the dispatcher, where the caller delivers inline as it did
// before the queue existed.
//
// A full queue means the sink is stalled, and a stall is exactly when a money
// alert matters most. Waiting for a slot would put Telegram back in front of the
// customer's bill, so the alert goes to disk instead: the append costs a few
// microseconds, and the dispatcher delivers it once the sink recovers. When the
// append fails (disk full, permissions, a spool past its cap) the alert is
// written to the journal as a flattened one-line record. That is the durable
// record and it still costs the request path nothing — the request path must
// never send inline, because doing so held the customer's inflight slot and
// their 15 s billing context for as long as Telegram took.
func (q *alertQueue) offer(msg string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch q.state {
	case alertRunning:
		select {
		case q.ch <- msg:
			return true
		default:
			// spool runs with q.mu held. It never waits on the sink, and its
			// own lock is a leaf — nothing takes q.mu while holding spoolMu —
			// so the nesting cannot deadlock.
			if q.spool(msg, len(q.ch), "queue full") {
				return true
			}
			q.journal("queue full and the spool refused it", msg)
			return true
		}
	case alertStopped:
		// A billing goroutine that outlived shutdown still raised a money
		// alert. The dispatcher has already spooled the queue and gone, so the
		// spool is the only queue left; the journal covers a spool that
		// refuses. Either way the request path does not wait on the sink.
		if q.spool(msg, len(q.ch), "gateway stopped") {
			return true
		}
		q.journal("gateway stopped and the spool refused it", msg)
		return true
	default:
		return false
	}
}

// spool appends one alert to the on-disk queue and reports whether it landed.
// Every failure here is loud, and the false return carries a promise: the alert
// is not on disk, so responsibility for it is still the caller's. Every caller
// journals the alert on false — the log is the record of last resort, never a
// blocking send.
func (q *alertQueue) spool(msg string, pending int, reason string) bool {
	if q.spoolPath == "" {
		log.Printf("gateway: admin alert not spooled (%s, %d pending, no spool path)", reason, pending)
		return false
	}
	q.spoolMu.Lock()
	defer q.spoolMu.Unlock()
	line, err := json.Marshal(msg)
	if err != nil {
		log.Printf("gateway: admin alert not spooled (%s, spool encode: %v)", reason, err)
		return false
	}
	line = append(line, '\n')
	var size int64
	if fi, err := os.Stat(q.spoolPath); err == nil {
		size = fi.Size()
	}
	if size+int64(len(line)) > alertSpoolMax {
		log.Printf("gateway: admin alert not spooled (%s and spool full: %d bytes, limit %d) — the spool needs attention", reason, size, alertSpoolMax)
		return false
	}
	// #nosec G304 -- the path is built from the operator's DB_PATH, never from
	// a request.
	f, err := os.OpenFile(q.spoolPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("gateway: admin alert not spooled (%s, spool open: %v)", reason, err)
		return false
	}
	_, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		log.Printf("gateway: admin alert not spooled (%s, spool write: %v)", reason, werr)
		return false
	}
	if cerr != nil {
		log.Printf("gateway: admin alert not spooled (%s, spool close: %v)", reason, cerr)
		return false
	}
	// The alert text is request-derived and stays out of this line (gosec
	// G706): the counts are all an operator needs to see alerts backing up.
	log.Printf("gateway: admin alert spooled to disk (%s, %d pending)", reason, pending)
	return true
}

// spoolPeek returns the oldest spooled alert and the exact line holding it,
// without touching the file. Sending comes first and removal afterwards
// (deliverSpooled), so an alert whose send never completes stays exactly where
// it was — the spool is at-least-once, not at-most-once.
func (q *alertQueue) spoolPeek() (msg, line string, ok bool) {
	if q.spoolPath == "" {
		return "", "", false
	}
	q.spoolMu.Lock()
	defer q.spoolMu.Unlock()
	// #nosec G304 -- spoolPath derives from the operator's DB_PATH.
	data, err := os.ReadFile(q.spoolPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("gateway: alert spool unreadable: %v", err)
		}
		return "", "", false
	}
	for _, entry := range splitSpool(data) {
		if msg, ok := spoolLine(entry); ok {
			return msg, entry, true
		}
		// A line we cannot decode is a damaged entry: skip it and keep the
		// alerts queued behind it rather than wedging the spool on it.
		log.Printf("gateway: alert spool line skipped (not a JSON string)")
	}
	return "", "", false
}

// removeSpooled drops the first spool line equal to line. It re-reads the file
// under spoolMu instead of trusting an earlier read, so an offer() that appended
// while the send was in flight is preserved: appends only ever add at the end,
// and the first line equal to what was just sent is the one that was peeked.
// The rewrite is atomic (temp file, sync, rename), so a crash leaves the old
// queue or the new one.
func (q *alertQueue) removeSpooled(line string) error {
	q.spoolMu.Lock()
	defer q.spoolMu.Unlock()
	// #nosec G304 -- spoolPath derives from the operator's DB_PATH.
	data, err := os.ReadFile(q.spoolPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Nothing left to remove: an empty spool is the goal.
			return nil
		}
		return err
	}
	lines := splitSpool(data)
	rest := make([]string, 0, len(lines))
	removed := false
	for _, entry := range lines {
		if !removed && entry == line {
			removed = true
			continue
		}
		if !removed {
			if _, ok := spoolLine(entry); !ok {
				// A damaged line ahead of the alert is dropped by the rewrite
				// we are doing anyway, so it cannot be scanned again.
				log.Printf("gateway: alert spool line dropped (not a JSON string)")
				continue
			}
		}
		rest = append(rest, entry)
	}
	if !removed {
		// Only the dispatcher removes, so this is a spool someone else edited
		// under us. Report it: the caller stops draining instead of sending the
		// same head again in a loop.
		return errors.New("spool head vanished before it could be removed")
	}
	return q.rewriteSpool(rest)
}

// rewriteSpool replaces the spool with lines. Readers see the old file or the
// new one, never a partial write: a temp file in the same directory, synced, then
// renamed over the spool.
func (q *alertQueue) rewriteSpool(lines []string) error {
	tmp, err := os.CreateTemp(filepath.Dir(q.spoolPath), ".alerts.spool-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if name != "" {
			_ = os.Remove(name) // still there only when the rename did not happen
		}
	}()
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, q.spoolPath); err != nil {
		return err
	}
	name = "" // renamed: nothing left to clean up
	return nil
}

// splitSpool returns the spool's non-empty lines. JSON encoding escapes the
// newlines inside an alert, so one line is exactly one alert.
func splitSpool(data []byte) []string {
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// spoolLine decodes one spool line into an alert. A line that will not decode is
// damaged: the one thing the spool must never do is wedge on it, or take a
// single corrupt byte as an excuse to stop delivering the alerts behind it.
func spoolLine(entry string) (string, bool) {
	var msg string
	if err := json.Unmarshal([]byte(entry), &msg); err != nil {
		return "", false
	}
	return msg, true
}

// spoolAttempt is what one pass over the spool head did. queued and attempted
// separate "there was nothing to do" from "the sink was tried", because only a
// real try counts against the alert's attempt budget.
type spoolAttempt struct {
	msg       string
	line      string
	queued    bool // the spool had a head to try
	attempted bool // a send was actually started for it
	delivered bool // that send returned inside its budget
	removed   bool // the head left the spool, so the alert is done
}

// deliverSpooled sends the spool's head and removes it only once that send has
// completed. The order is the whole point: popping first destroyed the alert
// whenever the sink overran its budget, which is exactly the state that put it on
// disk.
func (q *alertQueue) deliverSpooled(ctx context.Context, send func(string)) spoolAttempt {
	// An abandoned send still holds the only inflight token, so a second send
	// would be refused anyway; waiting for the next pass is the cheap answer.
	if ctx.Err() != nil || len(q.inflight) > 0 {
		return spoolAttempt{}
	}
	msg, line, ok := q.spoolPeek()
	if !ok {
		return spoolAttempt{}
	}
	attempted, delivered := q.sendOne(msg, send)
	if !attempted {
		// Another send is still outstanding: the head was not tried, so this
		// pass must not count against it.
		return spoolAttempt{msg: msg, line: line, queued: true}
	}
	if !delivered {
		// Neither delivered nor lost: the head stays on disk for a later
		// attempt, after its backoff.
		return spoolAttempt{msg: msg, line: line, queued: true, attempted: true}
	}
	if err := q.removeSpooled(line); err != nil {
		// Delivered but still on disk: the next pass repeats it rather than
		// dropping it. A duplicate money alert is the safe failure.
		log.Printf("gateway: alert spool removal failed (alert will be retried): %v", err)
		return spoolAttempt{msg: msg, line: line, queued: true, attempted: true, delivered: true}
	}
	return spoolAttempt{msg: msg, line: line, queued: true, attempted: true, delivered: true, removed: true}
}

// deliverQueued sends one alert taken off the channel and reports whether the
// send returned inside its budget. The queue no longer holds the alert, so a
// send that overran its budget goes to the spool: without that, a stalled sink
// would erase it from both places. A spool that refuses the append leaves the
// journal as the record. It never blocks the request path.
func (q *alertQueue) deliverQueued(msg string, send func(string)) bool {
	_, delivered := q.sendOne(msg, send)
	if delivered {
		return true
	}
	if !q.spool(msg, len(q.ch), "send timed out") {
		q.journal("send timed out and the spool refused it", msg)
	}
	return false
}

// dispatch delivers alerts until ctx ends, oldest first across both queues. A
// channel item always goes before a spooled one: offer only appends to the spool
// when the channel is full, so anything spooled is newer than anything still
// queued, and draining the spool first let a newer alert overtake an older one.
// A stopped queue keeps its channel open and journals anything offered later, so
// a request goroutine that outlives shutdown cannot panic on a closed channel.
//
// The spool head is retried on a backoff held here (not a sleep in the send
// path), and only a bounded number of times: a sink that overruns its budget has
// usually delivered the message anyway, so an immediate re-kick re-sent it, and a
// head that can never be delivered must not park every alert behind it.
func (q *alertQueue) dispatch(ctx context.Context, send func(string)) {
	defer close(q.done)
	var (
		// Owned by this goroutine alone.
		attempts int       // failed attempts at the current head
		head     string    // the alert those attempts belong to
		retryAt  time.Time // zero when the head may be tried at once
	)
	for {
		// Shutdown first: with both cases ready Go picks at random, and a stop
		// that raced with a queued alert must not keep sending.
		if ctx.Err() != nil {
			q.stop()
			return
		}
		select {
		case <-ctx.Done():
			q.stop()
			return
		case msg := <-q.ch:
			if !q.deliverQueued(msg, send) {
				// The sink just overran: back off before trying the spool
				// head, or the next alert is the same failure again.
				retryAt = time.Now().Add(q.backoff(1))
			}
			continue
		default:
		}

		// The spool head, but only once its backoff has elapsed. A kick is not
		// permission to skip the wait: the sink returning does not mean the
		// alert was not delivered.
		if !time.Now().Before(retryAt) {
			a := q.deliverSpooled(ctx, send)
			if a.queued && !a.attempted {
				// The previous send is still outstanding. Wait for it, but do
				// not depend on its kick alone.
				retryAt = time.Now().Add(q.backoff(1))
				continue
			}
			if a.attempted {
				if a.msg != head {
					// A different alert became the head: its own count starts
					// at zero.
					attempts, head = 0, a.msg
				}
				switch {
				case a.removed:
					attempts, head, retryAt = 0, "", time.Time{}
				case a.delivered:
					// Delivered but still on disk: repeating it is the safe
					// failure, but do it slowly so a broken spool cannot turn
					// into a duplicate generator.
					retryAt = time.Now().Add(q.backoff(alertSpoolAttempts))
				default:
					attempts++
					if attempts >= alertSpoolAttempts {
						// The journal is the durable record, and the spool must
						// move on: an alert nobody can deliver must not block
						// the ones behind it forever.
						q.journal(fmt.Sprintf("undeliverable after %d attempts", attempts), a.msg)
						if err := q.removeSpooled(a.line); err != nil {
							log.Printf("gateway: alert spool removal failed after %d attempts (alert will be retried): %v", attempts, err)
							attempts, retryAt = 0, time.Now().Add(q.backoff(alertSpoolAttempts))
						} else {
							attempts, head, retryAt = 0, "", time.Time{}
						}
					} else {
						retryAt = time.Now().Add(q.backoff(attempts))
					}
				}
				continue
			}
		}

		// Nothing was deliverable: the spool is empty, or its head is waiting
		// out its backoff. Wait for work, a kick, or the retry deadline instead
		// of spinning.
		var (
			timer *time.Timer
			tick  <-chan time.Time
		)
		if !retryAt.IsZero() {
			wait := time.Until(retryAt)
			if wait < 0 {
				wait = 0
			}
			timer = time.NewTimer(wait)
			tick = timer.C
		}
		select {
		case <-ctx.Done():
		case msg := <-q.ch:
			if !q.deliverQueued(msg, send) {
				retryAt = time.Now().Add(q.backoff(1))
			}
		case <-q.kick:
			// A send returned. Look again: the channel may hold an older
			// alert, and the head's backoff may have elapsed.
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// stop marks the queue closed and moves whatever is still waiting onto the
// spool, so the next start() delivers it instead of a restart erasing it. Alerts
// are notifications, not work that must complete: shutdown does not wait for a
// send it has already started. Only a spool that refuses an append is short of
// the goal, and it says so — and that alert goes to the journal, not on the
// floor.
func (q *alertQueue) stop() {
	q.mu.Lock()
	q.state = alertStopped
	q.mu.Unlock()
	q.spoolPending()
}

// spoolPending moves the channel's contents onto the spool, oldest first. The
// loop is bounded by what is in the channel: once stop() has marked the queue
// stopped, offer() cannot add to it, so it cannot chase late arrivals.
func (q *alertQueue) spoolPending() {
	spooled, journaled := 0, 0
	for {
		select {
		case msg := <-q.ch:
			if q.spool(msg, len(q.ch), "shutdown") {
				spooled++
			} else {
				q.journal("shutdown and the spool refused it", msg)
				journaled++
			}
		default:
			if spooled > 0 {
				log.Printf("gateway: spooled %d queued admin alert(s) at shutdown", spooled)
			}
			if journaled > 0 {
				log.Printf("gateway: journaled %d queued admin alert(s) at shutdown (the spool refused them)", journaled)
			}
			return
		}
	}
}

// sendOne gives one alert its own budget. A sink that overruns is abandoned,
// not cancelled: the send has no context of its own and its HTTP client gives up
// on its own timeout, while the dispatcher must stay free for the next alert.
// Only one send may be outstanding, so a sink that never returns costs one
// goroutine, not one per alert.
//
// It reports whether a send was started at all, and whether it returned inside
// the budget. A returned sink is not proof Telegram accepted the message —
// nothing here can prove that — but it is the best signal available, and it is
// what decides whether the alert has left our hands. A panicking sink is treated
// as not delivered and logged; it must not take the process down. "Not started"
// is separate from "not delivered" so a caller cannot burn an alert's retry
// budget on a pass that never reached the sink.
func (q *alertQueue) sendOne(msg string, send func(string)) (attempted, delivered bool) {
	select {
	case q.inflight <- struct{}{}:
	default:
		log.Printf("gateway: admin alert send skipped (previous send still stuck): %s", alertSummary(msg))
		return false, false
	}
	// Buffered: the goroutine must be able to publish its result after the
	// caller has given up and moved on.
	done := make(chan bool, 1)
	go func() {
		sent := false
		defer func() {
			if r := recover(); r != nil {
				log.Printf("gateway: admin alert sink panicked: %v", r)
			}
			// Explicit order, because each step is load-bearing: the token is
			// free before the dispatcher is woken, and the result is published
			// last so a caller that reads it never mistakes its own finished
			// send for a stuck one.
			<-q.inflight
			q.kickDispatch()
			done <- sent
		}()
		send(msg)
		sent = true
	}()
	timer := time.NewTimer(q.sendBudget())
	defer timer.Stop()
	select {
	case ok := <-done:
		return true, ok
	case <-timer.C:
		log.Printf("gateway: admin alert send timed out after %s: %s", q.sendBudget(), alertSummary(msg))
		return true, false
	}
}

// kickDispatch nudges the dispatcher without ever blocking on it. One pending
// nudge is enough: it means "look at the queues again". It is a wake-up, not
// permission to skip a head's backoff.
func (q *alertQueue) kickDispatch() {
	select {
	case q.kick <- struct{}{}:
	default:
	}
}

// journal records an alert in the process log as the durable record of last
// resort: the spool refused it (or there is no spool), and the request path must
// not fall back to a blocking send. The text is flattened and capped by
// alertJournalLine, so request-derived content cannot forge a log line or carry
// a terminal escape.
func (q *alertQueue) journal(reason, msg string) {
	log.Printf("gateway: admin alert journaled (%s): %s", reason, alertJournalLine(msg)) // #nosec G706 -- alertJournalLine replaces line breaks with " | " and drops every other control character, so the tainted text cannot inject a line
}

// alertJournalLine renders an alert as one log-safe line: line breaks become
// " | ", every other control character is dropped, and the result is capped. The
// journal is all an operator has when the spool is gone, so the alert has to stay
// recognisable — and it must never be able to forge a log entry.
func alertJournalLine(msg string) string {
	line := strings.NewReplacer("\r\n", " | ", "\n", " | ", "\r", " | ").Replace(msg)
	line = strings.Map(func(r rune) rune {
		// C0 and DEL, plus the C1 block (U+0080–U+009F, which contains NEL)
		// and the Unicode line separators: some terminals treat those as
		// line breaks, so they are dropped rather than trusted.
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, line)
	return truncateLine(line, 200)
}

// truncateLine caps a log line at max bytes, cutting on a rune boundary so an
// emoji in an alert cannot leave half a rune behind.
func truncateLine(line string, max int) string {
	if len(line) <= max {
		return line
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + "…"
}

// wait blocks until the dispatcher has returned, ctx ends, or limit elapses, and
// reports whether the dispatcher finished. Run uses it so a restart cannot exit
// while stop() is still spooling the queue: that window is exactly how queued
// money alerts used to disappear.
func (q *alertQueue) wait(ctx context.Context, limit time.Duration) bool {
	q.mu.Lock()
	done, state := q.done, q.state
	q.mu.Unlock()
	if state == alertIdle || done == nil {
		// No dispatcher was ever started: there is nothing to join.
		return true
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// alertSummary is the first line of an alert, trimmed for a log line. The body
// carries customer ids and amounts; the headline is enough to recognise it.
func alertSummary(msg string) string {
	line := msg
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return truncateLine(line, 120)
}

// startAlerts starts the queue's dispatcher. Run calls it once; the sink is
// resolved per send so SetAdminAlert keeps working while the gateway is up.
func (s *Server) startAlerts(ctx context.Context) {
	s.alerts.start(ctx, func(msg string) {
		if fn := s.adminAlertSink(); fn != nil {
			fn(msg)
		}
	})
}
