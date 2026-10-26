package store

import (
	"context"
	"errors"
	"testing"

	"tgbot/internal/ledger"
)

func TestCustomerRead(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Credit(ctx, 600, ledger.Amount(5)*ledger.UsecPerMYR, "topup", "c1"); err != nil {
		t.Fatal(err)
	}

	c, err := d.Account(ctx, 600)
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	if c.Balance != ledger.Amount(5)*ledger.UsecPerMYR {
		t.Fatalf("balance = %d", c.Balance)
	}

	if _, err := d.Account(ctx, 601); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account = %v, want ErrNotFound", err)
	}
}

func TestUsageWindowAndPrune(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 601); err != nil {
		t.Fatal(err)
	}
	insert := func(created int64, cost int64) {
		t.Helper()
		if _, err := d.db.ExecContext(ctx,
			`INSERT INTO usage (telegram_id, model, prompt_tokens, completion_tokens, cost, created_at)
			 VALUES (?, 'm', 10, 5, ?, ?)`, 601, cost, created); err != nil {
			t.Fatal(err)
		}
	}
	insert(1000, 100)
	insert(2000, 250)

	requests, prompt, completion, spend, err := d.UsageWindow(ctx, 601, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || prompt != 10 || completion != 5 || spend != 250 {
		t.Fatalf("window = %d %d %d %d", requests, prompt, completion, spend)
	}

	pruned, err := d.PruneUsage(ctx, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	var left int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM usage WHERE telegram_id = 601`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatalf("rows left = %d, want 1", left)
	}
}

func TestUpstreamErrorWindowAndPrune(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if err := d.RecordUpstreamFailure(ctx, "/v1/chat/completions", 500, 3, false); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordUpstreamFailure(ctx, "/v1/chat/completions", 200, 1, true); err != nil {
		t.Fatal(err)
	}

	n, err := d.UpstreamErrorWindow(ctx, nowUnix()-60)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("window = %d, want 2", n)
	}

	pruned, err := d.PruneUpstreamErrors(ctx, nowUnix()+1)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 2 {
		t.Fatalf("pruned = %d, want 2", pruned)
	}
	n, err = d.UpstreamErrorWindow(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("window after prune = %d, want 0", n)
	}
}

// The prune is batched because the store has one write connection: a single
// unbounded DELETE blocks every charge behind it. It must still delete every
// old row, and only those.
func TestPruneDeletesEveryOldRowInBatches(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 500); err != nil {
		t.Fatal(err)
	}
	const oldRows = 2*pruneBatch + 7
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < oldRows; i++ {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO usage (telegram_id, model, cost, created_at) VALUES (500, 'm', 1, 100)`,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO usage (telegram_id, model, cost, created_at) VALUES (500, 'm', 1, 900000000)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	n, err := d.PruneUsage(ctx, 500000000)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != oldRows {
		t.Fatalf("pruned %d rows, want %d", n, oldRows)
	}
	var left int
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatalf("%d rows left, want the recent one", left)
	}
}
