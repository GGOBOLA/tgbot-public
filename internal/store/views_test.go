package store

import (
	"context"
	"testing"

	"tgbot/internal/ledger"
)

func TestLedgerEntriesNewestFirst(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 700); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"a", "b"} {
		if _, err := d.Credit(ctx, 700, ledger.Amount(1)*ledger.UsecPerMYR, "topup", ref); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := d.LedgerEntries(ctx, 700, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Ref != "b" || entries[1].Ref != "a" {
		t.Fatalf("order = %q, %q; want newest first", entries[0].Ref, entries[1].Ref)
	}
	if entries[0].BalanceAfter != ledger.Amount(2)*ledger.UsecPerMYR {
		t.Fatalf("balance_after = %d", entries[0].BalanceAfter)
	}
}

func TestLatencyByModel(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 701); err != nil {
		t.Fatal(err)
	}
	ins := func(model string, firstMs int64) {
		t.Helper()
		if _, err := d.db.ExecContext(ctx,
			`INSERT INTO usage (telegram_id, model, cost, created_at, first_ms)
			 VALUES (?, ?, 0, ?, ?)`, 701, model, nowUnix(), firstMs); err != nil {
			t.Fatal(err)
		}
	}
	ins("fast", 100)
	ins("fast", 300)
	ins("slow", 900)
	ins("no-sample", 0) // excluded

	got, err := d.LatencyByModel(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("models = %d, want 2 (%+v)", len(got), got)
	}
	if got[0].Model != "slow" || got[0].AvgFirstMS != 900 {
		t.Fatalf("first = %+v, want slow/900", got[0])
	}
	if got[1].Model != "fast" || got[1].Requests != 2 || got[1].AvgFirstMS != 200 {
		t.Fatalf("second = %+v, want fast/2/200", got[1])
	}
}

func TestUpstreamSummary(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if err := d.RecordUpstreamFailure(ctx, "/v1/chat", 502, 2, true); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordUpstreamFailure(ctx, "/v1/chat", 500, 3, false); err != nil {
		t.Fatal(err)
	}

	got, err := d.UpstreamSummary(ctx, nowUnix()-60)
	if err != nil {
		t.Fatal(err)
	}
	if got.Failed != 2 || got.Recovered != 1 || got.GaveUp != 1 {
		t.Fatalf("summary = %+v, want 2/1/1", got)
	}
	if got.LastStatus != 500 {
		t.Fatalf("last status = %d, want 500", got.LastStatus)
	}
	if got.LastAt == "" {
		t.Fatal("last at is empty")
	}

	empty, err := d.UpstreamSummary(ctx, nowUnix()+1)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Failed != 0 {
		t.Fatalf("future window = %+v, want zero", empty)
	}
}
