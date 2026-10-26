package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"tgbot/internal/ledger"
)

// lowBalanceCall is one hook invocation: which customer was told, and with what
// balance. The id is recorded, not discarded — a warning delivered to the wrong
// customer is exactly the regression this test has to catch.
type lowBalanceCall struct {
	id      int64
	balance ledger.Amount
}

// lowBalanceRecorder records every hook call. The hook runs on its own goroutine
// (see notifyLowBalance), so the log is mutex-guarded and read after the
// transaction returns — no sleep is needed to observe it.
type lowBalanceRecorder struct {
	mu    sync.Mutex
	calls []lowBalanceCall
	fired chan lowBalanceCall
}

func newLowBalanceRecorder() *lowBalanceRecorder {
	return &lowBalanceRecorder{fired: make(chan lowBalanceCall, 4)}
}

func (r *lowBalanceRecorder) record(_ context.Context, id int64, balance ledger.Amount) {
	call := lowBalanceCall{id: id, balance: balance}
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
	select {
	case r.fired <- call:
	default: // the log still has every call; the channel is only for waiting
	}
}

func (r *lowBalanceRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func TestLowBalanceHookFiresOnceOnCrossing(t *testing.T) {
	// The customer whose balance crosses: the warning must name this id.
	const customerID int64 = 900
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, customerID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Credit(ctx, customerID, ledger.Amount(5)*ledger.UsecPerMYR, "topup", "l1"); err != nil {
		t.Fatal(err)
	}

	rec := newLowBalanceRecorder()
	d.SetLowBalanceHook(rec.record)

	// 5 -> 2: still on the threshold, no warning.
	if _, err := d.Charge(ctx, customerID, ledger.Amount(3)*ledger.UsecPerMYR, "usage", "l2"); err != nil {
		t.Fatal(err)
	}
	// The hook runs on its own goroutine, so a wrongly fired warning that has
	// not been scheduled yet would pass an immediate check.
	time.Sleep(100 * time.Millisecond)
	if n := rec.count(); n != 0 {
		t.Fatalf("hook fired %d times at the threshold, want 0", n)
	}

	// 2 -> 1: crosses below, exactly one warning, addressed to customerID.
	if _, err := d.Charge(ctx, customerID, ledger.Amount(1)*ledger.UsecPerMYR, "usage", "l3"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-rec.fired:
		if got.id != customerID {
			t.Fatalf("warning sent to customer %d, want %d", got.id, customerID)
		}
		if want := ledger.Amount(1) * ledger.UsecPerMYR; got.balance != want {
			t.Fatalf("hook balance = %d, want %d", got.balance, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hook did not fire on the crossing")
	}
	if n := rec.count(); n != 1 {
		t.Fatalf("hook fired %d times on the crossing, want exactly 1", n)
	}

	// 1 -> 0: already below the line, no second warning.
	if _, err := d.Charge(ctx, customerID, ledger.Amount(1)*ledger.UsecPerMYR, "usage", "l4"); err != nil {
		t.Fatal(err)
	}
	if n := rec.count(); n != 1 {
		t.Fatalf("hook fired again: %d calls, want 1", n)
	}
}

func TestLowBalanceHookIgnoresCredits(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 901); err != nil {
		t.Fatal(err)
	}

	rec := newLowBalanceRecorder()
	d.SetLowBalanceHook(rec.record)

	if _, err := d.Credit(ctx, 901, ledger.Amount(1)*ledger.UsecPerMYR, "topup", "c1"); err != nil {
		t.Fatal(err)
	}
	// The hook runs on its own goroutine, so a wrongly fired warning that has
	// not been scheduled yet would pass an immediate check.
	time.Sleep(100 * time.Millisecond)
	if n := rec.count(); n != 0 {
		t.Fatalf("hook fired %d times on a credit, want 0", n)
	}
}

// TestLowBalanceCrossingDecision pins the rule behind the warning without the
// notification goroutine: applyTx reports whether a debit crossed the threshold,
// and notifyLowBalance fires iff that flag is true. The flag is decided inside
// the transaction, so it is exact — unlike a counter polled before a spawned
// goroutine has run, which can still miss a wrongly-fired hook.
func TestLowBalanceCrossingDecision(t *testing.T) {
	rm := ledger.Amount(ledger.UsecPerMYR)
	for _, tc := range []struct {
		name   string
		before ledger.Amount
		delta  ledger.Amount
		want   bool
	}{
		{"debit lands exactly on the threshold", 5 * rm, -3 * rm, false},
		{"debit crosses below the threshold", 2 * rm, -1 * rm, true},
		{"debit already below the threshold", 1 * rm, -1 * rm, false},
		{"credit below the threshold", 1 * rm, 1 * rm, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			ctx := context.Background()
			const id = 950
			if err := d.EnsureCustomer(ctx, id); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Credit(ctx, id, tc.before, "topup", "seed"); err != nil {
				t.Fatal(err)
			}
			if got := crossedInRolledBackTx(t, d, id, tc.delta); got != tc.want {
				t.Fatalf("crossed = %v for %d -> %d, want %v", got, tc.before, tc.before+tc.delta, tc.want)
			}
		})
	}
}

// crossedInRolledBackTx asks applyTx the crossing question inside a transaction
// that is rolled back, so the balance stays where the case seeded it.
func crossedInRolledBackTx(t *testing.T, d *DB, telegramID int64, delta ledger.Amount) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	_, crossed, err := applyTx(ctx, tx, telegramID, delta, "probe", "")
	if err != nil {
		t.Fatal(err)
	}
	return crossed
}
