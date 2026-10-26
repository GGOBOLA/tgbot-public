package store

import (
	"context"
	"errors"
	"testing"

	"tgbot/internal/ledger"
)

func TestChargeUsageMovesMoneyAndRecords(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Credit(ctx, 500, ledger.Amount(10)*ledger.UsecPerMYR, "topup", "t1"); err != nil {
		t.Fatal(err)
	}

	bal, err := d.ChargeUsage(ctx, UsageEntry{
		TelegramID: 500, Model: "m", PromptTokens: 100, CompletionTokens: 50,
		Cost: ledger.Amount(3) * ledger.UsecPerMYR, FirstMS: 40,
	})
	if err != nil {
		t.Fatalf("charge usage: %v", err)
	}
	if want := ledger.Amount(7) * ledger.UsecPerMYR; bal != want {
		t.Fatalf("balance = %d, want %d", bal, want)
	}

	_, _, _, spend, err := d.UsageTotals(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	if want := ledger.Amount(3) * ledger.UsecPerMYR; spend != want {
		t.Fatalf("usage spend = %d, want %d", spend, want)
	}

	var ledgerRows int
	if err := d.db.QueryRow(
		`SELECT COUNT(*) FROM ledger WHERE telegram_id = 500 AND reason = 'usage'`).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if ledgerRows != 1 {
		t.Fatalf("usage ledger rows = %d, want 1", ledgerRows)
	}
}

func TestChargeUsageUnknownCustomerLeavesNoRow(t *testing.T) {
	d := openTestDB(t)
	_, err := d.ChargeUsage(context.Background(), UsageEntry{
		TelegramID: 501, Model: "m", Cost: ledger.Amount(1),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	var rows int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM usage WHERE telegram_id = 501`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("rolled-back usage left %d rows", rows)
	}
}

func TestChargeUsageZeroCostStillRecords(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 502); err != nil {
		t.Fatal(err)
	}
	bal, err := d.ChargeUsage(ctx, UsageEntry{TelegramID: 502, Model: "m", Cost: 0})
	if err != nil {
		t.Fatalf("zero-cost charge: %v", err)
	}
	if bal != 0 {
		t.Fatalf("balance = %d, want 0", bal)
	}
	requests, _, _, _, err := d.UsageTotals(ctx, 502)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestChargeUsageNegativeRejected(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 503); err != nil {
		t.Fatal(err)
	}
	_, err := d.ChargeUsage(ctx, UsageEntry{TelegramID: 503, Model: "m", Cost: ledger.Amount(-1)})
	if !errors.Is(err, ledger.ErrNegative) {
		t.Fatalf("err = %v, want ErrNegative", err)
	}
}
