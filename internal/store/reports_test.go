package store

import (
	"context"
	"errors"
	"math"
	"testing"

	"tgbot/internal/ledger"
)

func TestCustomersPageAndCount(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	for _, id := range []int64{801, 802, 803} {
		if err := d.EnsureCustomer(ctx, id); err != nil {
			t.Fatal(err)
		}
	}

	n, err := d.CustomerCount(ctx)
	if err != nil || n != 3 {
		t.Fatalf("count = %d, %v", n, err)
	}

	page, err := d.Customers(ctx, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].TelegramID != 802 {
		t.Fatalf("page = %+v", page)
	}
}

func TestTotalFloatCountsOnlyPositive(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 810); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureCustomer(ctx, 811); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Credit(ctx, 810, ledger.Amount(5)*ledger.UsecPerMYR, "topup", "f1"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Credit(ctx, 811, ledger.Amount(2)*ledger.UsecPerMYR, "topup", "f2"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Charge(ctx, 811, ledger.Amount(3)*ledger.UsecPerMYR, "usage", "f3"); err != nil {
		t.Fatal(err)
	}

	total, err := d.TotalFloat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := ledger.Amount(5) * ledger.UsecPerMYR; total != want {
		t.Fatalf("float = %d, want %d (debt must not offset)", total, want)
	}
}

func TestBalanceAlerts(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 820); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Credit(ctx, 820, ledger.Amount(1)*ledger.UsecPerMYR, "topup", "a1"); err != nil {
		t.Fatal(err)
	}

	alerts, err := d.BalanceAlerts(ctx, ledger.Amount(2)*ledger.UsecPerMYR)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].TelegramID != 820 {
		t.Fatalf("alerts = %+v", alerts)
	}
}

func TestSummaryAndTopSpenders(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	for _, id := range []int64{830, 831} {
		if err := d.EnsureCustomer(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.RecordUsage(ctx, UsageEntry{TelegramID: 830, Model: "m", PromptTokens: 10, CompletionTokens: 5, Cost: 400}); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordUsage(ctx, UsageEntry{TelegramID: 831, Model: "m", PromptTokens: 20, CompletionTokens: 8, Cost: 100}); err != nil {
		t.Fatal(err)
	}

	sum, err := d.SummaryFor(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 2 || sum.PromptTokens != 30 || sum.CompletionTokens != 13 || sum.Spend != 500 || sum.ActiveCustomers != 2 {
		t.Fatalf("summary = %+v", sum)
	}

	top, err := d.TopSpenders(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0].TelegramID != 830 || top[0].Spend != 400 {
		t.Fatalf("top = %+v", top)
	}
}

func TestUsageAggregatesSaturateCostOverflow(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	const id int64 = 839
	if err := d.EnsureCustomer(ctx, id); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := d.RecordUsage(ctx, UsageEntry{TelegramID: id, Model: "m", Cost: ledger.Amount(math.MaxInt64 / 4)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, spend, err := d.UsageTotals(ctx, id); err != nil || int64(spend) != math.MaxInt64 {
		t.Fatalf("UsageTotals spend = %d, err = %v", spend, err)
	}
	if _, _, _, spend, err := d.UsageWindow(ctx, id, 0); err != nil || int64(spend) != math.MaxInt64 {
		t.Fatalf("UsageWindow spend = %d, err = %v", spend, err)
	}
	summary, err := d.SummaryFor(ctx, 0)
	if err != nil || int64(summary.Spend) != math.MaxInt64 {
		t.Fatalf("SummaryFor spend = %d, err = %v", summary.Spend, err)
	}
	top, err := d.TopSpenders(ctx, 0, 10)
	if err != nil || len(top) != 1 || int64(top[0].Spend) != math.MaxInt64 {
		t.Fatalf("TopSpenders = %+v, err = %v", top, err)
	}
}

func TestUsageByKeyKeepsDeletedKeySpend(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 840); err != nil {
		t.Fatal(err)
	}
	_, key, err := d.CreateKey(ctx, 840, "Claude Code")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RecordUsage(ctx, UsageEntry{TelegramID: 840, KeyID: key.ID, Model: "m", Cost: 100}); err != nil {
		t.Fatal(err)
	}
	// Deleting the key must not lose its spend.
	if _, err := d.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, key.ID); err != nil {
		t.Fatal(err)
	}

	rows, err := d.UsageByKey(ctx, 840, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Name != "(deleted key)" || rows[0].Spend != 100 {
		t.Fatalf("deleted-key row = %+v", rows[0])
	}
}

func TestRenameKeyAndCount(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 850); err != nil {
		t.Fatal(err)
	}
	_, key, err := d.CreateKey(ctx, 850, "old")
	if err != nil {
		t.Fatal(err)
	}

	if n, err := d.ActiveKeyCount(ctx, 850); err != nil || n != 1 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if err := d.RenameKey(ctx, 850, key.ID, "new"); err != nil {
		t.Fatal(err)
	}
	keys, err := d.ListKeys(ctx, 850)
	if err != nil {
		t.Fatal(err)
	}
	if keys[0].Name != "new" {
		t.Fatalf("name = %q", keys[0].Name)
	}
	if err := d.RenameKey(ctx, 999, key.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign rename = %v, want ErrNotFound", err)
	}
}

func TestClaimRead(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 860); err != nil {
		t.Fatal(err)
	}
	claim, err := d.OpenClaim(ctx, 860, ledger.Amount(7)*ledger.UsecPerMYR)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Claim(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TelegramID != 860 || got.Amount != claim.Amount || got.Status != "pending" {
		t.Fatalf("claim = %+v", got)
	}
	if _, err := d.Claim(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing claim = %v", err)
	}
}

// SQLite returns REAL when an int64 SUM overflows, and scanning that into an
// int64 fails the query. The float must saturate instead of breaking the
// dashboard.
func TestTotalFloatSaturatesInsteadOfFailing(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	// Four full balances still fit in int64; five do not, which is the case
	// SQLite would answer as REAL.
	for _, id := range []int64{600, 601, 602, 603, 604} {
		if err := d.EnsureCustomer(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.db.ExecContext(ctx, `UPDATE customers SET balance = ? WHERE telegram_id IN (600, 601, 602, 603, 604)`, int64(maxMoney)); err != nil {
		t.Fatal(err)
	}
	total, err := d.TotalFloat(ctx)
	if err != nil {
		t.Fatalf("TotalFloat: %v", err)
	}
	if int64(total) != math.MaxInt64 {
		t.Fatalf("total = %d, want saturating %d", int64(total), int64(math.MaxInt64))
	}
}
