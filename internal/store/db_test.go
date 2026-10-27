package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"tgbot/internal/ledger"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestSchemaV1(t *testing.T) {
	d := openTestDB(t)

	var version int
	if err := d.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version = %d, want %d", version, schemaVersion)
	}

	for _, table := range []string{"customers", "api_keys", "ledger", "usage", "claims", "prices", "model_availability"} {
		var name string
		err := d.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %q missing: %v", table, err)
		}
	}
}

func TestCreditIsIdempotent(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 42); err != nil {
		t.Fatal(err)
	}

	amount := ledger.Amount(10) * ledger.UsecPerMYR
	bal, err := d.Credit(ctx, 42, amount, "topup", "ref-1")
	if err != nil {
		t.Fatalf("first credit: %v", err)
	}
	if bal != amount {
		t.Fatalf("balance = %d, want %d", bal, amount)
	}

	replayBal, err := d.Credit(ctx, 42, amount, "topup", "ref-1")
	if err != nil {
		t.Fatalf("replayed credit: %v", err)
	}
	if replayBal != amount {
		t.Fatalf("replayed balance = %d, want %d", replayBal, amount)
	}

	var rows int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM ledger WHERE ref = 'ref-1'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("ledger rows for ref-1 = %d, want 1", rows)
	}
}

func TestChargeAllowsDebt(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Credit(ctx, 7, ledger.Amount(1)*ledger.UsecPerMYR, "topup", "r1"); err != nil {
		t.Fatal(err)
	}

	bal, err := d.Charge(ctx, 7, ledger.Amount(2)*ledger.UsecPerMYR, "usage", "r2")
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if want := ledger.Amount(-1) * ledger.UsecPerMYR; bal != want {
		t.Fatalf("balance = %d, want %d", bal, want)
	}
}

func TestCreditUnknownCustomer(t *testing.T) {
	d := openTestDB(t)
	_, err := d.Credit(context.Background(), 999, ledger.Amount(1)*ledger.UsecPerMYR, "topup", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestNegativeCreditRejected(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 5); err != nil {
		t.Fatal(err)
	}
	_, err := d.Credit(ctx, 5, ledger.Amount(-1), "topup", "")
	if !errors.Is(err, ledger.ErrNegative) {
		t.Fatalf("err = %v, want ErrNegative", err)
	}
}

func TestBalanceCeilingEnforced(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 6); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Credit(ctx, 6, ledger.MaxAmount, "topup", "big-1"); err != nil {
		t.Fatalf("first credit: %v", err)
	}
	if _, err := d.Credit(ctx, 6, ledger.MaxAmount, "topup", "big-2"); !errors.Is(err, ledger.ErrOverflow) {
		t.Fatalf("second credit past the ceiling = %v, want ErrOverflow", err)
	}
}

func TestOnePendingClaimPerCustomer(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 8); err != nil {
		t.Fatal(err)
	}
	insert := func() error {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO claims (telegram_id, amount, status, created_at) VALUES (?, ?, 'pending', ?)`,
			8, 1000, nowUnix())
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := insert(); err == nil {
		t.Fatal("second pending claim was accepted")
	}
}

// A ref belongs to one (customer, amount) pair. Reusing it for another customer
// used to report success and drop the money.
func TestCreditRejectsRefReusedForAnotherCustomer(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	for _, id := range []int64{7, 8} {
		if err := d.EnsureCustomer(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	amount := ledger.Amount(10) * ledger.UsecPerMYR
	if _, err := d.Credit(ctx, 7, amount, "admin", "shared-ref"); err != nil {
		t.Fatal(err)
	}
	_, err := d.Credit(ctx, 8, amount, "admin", "shared-ref")
	if !errors.Is(err, ErrRefMismatch) {
		t.Fatalf("second customer with the same ref = %v, want ErrRefMismatch", err)
	}
	bal, err := d.Balance(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if bal != 0 {
		t.Fatalf("customer 8 balance = %d after a refused credit, want 0", bal)
	}
}

// The same ref with a different amount is also a conflict, not a replay.
func TestCreditRejectsRefReusedForAnotherAmount(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Credit(ctx, 7, ledger.Amount(10)*ledger.UsecPerMYR, "admin", "r"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Credit(ctx, 7, ledger.Amount(25)*ledger.UsecPerMYR, "admin", "r"); !errors.Is(err, ErrRefMismatch) {
		t.Fatalf("same ref, different amount = %v, want ErrRefMismatch", err)
	}
}

// A zero delta is refused with a named error rather than a raw CHECK failure.
func TestZeroDeltaIsRejected(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, _, err := applyTx(ctx, tx, 7, 0, "probe", ""); err == nil {
		t.Fatal("a zero delta was accepted")
	}
}
