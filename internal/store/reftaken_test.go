package store

import (
	"context"
	"errors"
	"testing"

	"tgbot/internal/ledger"
)

// A lost unique-ref race must only be reported as success when the winning row
// is this customer's own identical movement. Anything else is a collision, and
// answering "applied" would drop a credit silently.
func TestResolveRefTakenOnlyAcceptsTheSameMovement(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	const (
		owner = int64(42)
		other = int64(43)
	)
	if err := d.EnsureCustomer(ctx, owner); err != nil {
		t.Fatalf("ensure owner: %v", err)
	}
	if err := d.EnsureCustomer(ctx, other); err != nil {
		t.Fatalf("ensure other: %v", err)
	}
	const amount = ledger.Amount(250_000)
	if _, err := d.Credit(ctx, owner, amount, "topup", "ref-shared"); err != nil {
		t.Fatalf("seed credit: %v", err)
	}

	if err := resolveRefTaken(ctx, d.db, owner, amount, "ref-shared"); err != nil {
		t.Fatalf("same customer and delta must be a replay, got %v", err)
	}
	if err := resolveRefTaken(ctx, d.db, other, amount, "ref-shared"); !errors.Is(err, ErrRefMismatch) {
		t.Fatalf("different customer: got %v, want ErrRefMismatch", err)
	}
	if err := resolveRefTaken(ctx, d.db, owner, amount+1, "ref-shared"); !errors.Is(err, ErrRefMismatch) {
		t.Fatalf("different delta: got %v, want ErrRefMismatch", err)
	}
	if err := resolveRefTaken(ctx, d.db, owner, amount, "ref-absent"); !errors.Is(err, ErrRefMismatch) && err == nil {
		t.Fatal("missing ref must not be reported as a replay")
	}
}
