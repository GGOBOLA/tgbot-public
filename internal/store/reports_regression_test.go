package store

import (
	"context"
	"math"
	"testing"

	"tgbot/internal/ledger"
)

// Maximum legal usage values must not turn a dashboard query into SQL integer
// overflow. This covers both the per-key aggregate and token sums, which used
// separate SQL SUM calls and therefore missed the existing cost boundary test.
func TestUsageReportsSaturateLegalMaximumValues(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	const id int64 = 841
	if err := d.EnsureCustomer(ctx, id); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := d.RecordUsage(ctx, UsageEntry{
			TelegramID: id, Model: "m",
			PromptTokens: math.MaxInt64, CompletionTokens: math.MaxInt64,
			Cost: ledger.Amount(math.MaxInt64 / 4),
		}); err != nil {
			t.Fatal(err)
		}
	}

	usageByKey, err := d.UsageByKey(ctx, id, 0)
	if err != nil {
		t.Fatalf("UsageByKey: %v", err)
	}
	if len(usageByKey) != 1 {
		t.Fatalf("UsageByKey rows = %+v", usageByKey)
	}
	row := usageByKey[0]
	if row.Requests != 5 || row.PromptTokens != math.MaxInt64 || row.CompletionTokens != math.MaxInt64 || int64(row.Spend) != math.MaxInt64 {
		t.Fatalf("UsageByKey = %+v", row)
	}

	requests, prompt, completion, spend, err := d.UsageTotals(ctx, id)
	if err != nil || requests != 5 || prompt != math.MaxInt64 || completion != math.MaxInt64 || int64(spend) != math.MaxInt64 {
		t.Fatalf("UsageTotals = %d/%d/%d/%d (%v)", requests, prompt, completion, spend, err)
	}
	requests, prompt, completion, spend, err = d.UsageWindow(ctx, id, 0)
	if err != nil || requests != 5 || prompt != math.MaxInt64 || completion != math.MaxInt64 || int64(spend) != math.MaxInt64 {
		t.Fatalf("UsageWindow = %d/%d/%d/%d (%v)", requests, prompt, completion, spend, err)
	}
	summary, err := d.SummaryFor(ctx, 0)
	if err != nil || summary.Requests != 5 || summary.ActiveCustomers != 1 || summary.PromptTokens != math.MaxInt64 || summary.CompletionTokens != math.MaxInt64 || int64(summary.Spend) != math.MaxInt64 {
		t.Fatalf("SummaryFor = %+v (%v)", summary, err)
	}
}
