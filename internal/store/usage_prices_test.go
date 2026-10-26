package store

import (
	"context"
	"sync"
	"testing"

	"tgbot/internal/ledger"
)

func TestUsageTotals(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 300); err != nil {
		t.Fatal(err)
	}
	for _, cost := range []ledger.Amount{100, 250} {
		if err := d.RecordUsage(ctx, UsageEntry{
			TelegramID: 300, Model: "m", PromptTokens: 10, CompletionTokens: 5, Cost: cost, FirstMS: 12,
		}); err != nil {
			t.Fatal(err)
		}
	}
	requests, prompt, completion, spend, err := d.UsageTotals(ctx, 300)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || prompt != 20 || completion != 10 || spend != 350 {
		t.Fatalf("totals = %d %d %d %d", requests, prompt, completion, spend)
	}
}

func TestPricesUpsert(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if err := d.SetPrice(ctx, ModelPrice{Model: "m", PromptPerMillion: 100, CompletionPerMillion: 200}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetPrice(ctx, ModelPrice{Model: "m", PromptPerMillion: 111, CompletionPerMillion: 222}); err != nil {
		t.Fatal(err)
	}
	p, err := d.Price(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if p.PromptPerMillion != 111 || p.CompletionPerMillion != 222 {
		t.Fatalf("price = %+v", p)
	}
	if all, err := d.Prices(ctx); err != nil || len(all) != 1 {
		t.Fatalf("prices = %v, %v", all, err)
	}
}

func TestCreditConcurrentReplay(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 400); err != nil {
		t.Fatal(err)
	}
	amount := ledger.Amount(3) * ledger.UsecPerMYR

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = d.Credit(ctx, 400, amount, "topup", "same-ref")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	bal, err := d.Balance(ctx, 400)
	if err != nil {
		t.Fatal(err)
	}
	if bal != amount {
		t.Fatalf("balance = %d, want exactly one credit of %d", bal, amount)
	}
}
