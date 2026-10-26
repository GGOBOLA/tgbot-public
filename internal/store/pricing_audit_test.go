package store

import (
	"context"
	"errors"
	"testing"

	"tgbot/internal/ledger"
)

func TestSetPriceAuditedRecordsBothSides(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	// A model that has no price yet reports a zero "old": there is nothing to
	// compare against, but the row must still say who set it first.
	old, err := d.SetPriceAudited(ctx, ModelPrice{Model: "m", PromptPerMillion: 100, CompletionPerMillion: 200}, 11)
	if err != nil {
		t.Fatal(err)
	}
	if old.PromptPerMillion != 0 || old.CompletionPerMillion != 0 {
		t.Fatalf("first edit reported old = %+v, want zeros", old)
	}

	old, err = d.SetPriceAudited(ctx, ModelPrice{Model: "m", PromptPerMillion: 150, CompletionPerMillion: 250}, 12)
	if err != nil {
		t.Fatal(err)
	}
	if old.PromptPerMillion != 100 || old.CompletionPerMillion != 200 {
		t.Fatalf("second edit reported old = %+v, want the first values", old)
	}

	rows, err := d.db.QueryContext(ctx,
		`SELECT old_prompt, old_completion, new_prompt, new_completion, changed_by
		 FROM price_changes ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var got [][5]int64
	for rows.Next() {
		var r [5]int64
		if err := rows.Scan(&r[0], &r[1], &r[2], &r[3], &r[4]); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("audit rows = %d, want 2 (one per edit)", len(got))
	}
	if got[0] != [5]int64{0, 0, 100, 200, 11} {
		t.Fatalf("first audit row = %v", got[0])
	}
	if got[1] != [5]int64{100, 200, 150, 250, 12} {
		t.Fatalf("second audit row = %v", got[1])
	}

	// The price itself landed.
	p, err := d.Price(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if p.PromptPerMillion != 150 || p.CompletionPerMillion != 250 {
		t.Fatalf("price = %+v", p)
	}
}

func TestSetPriceAuditedRefusesUnattributedChange(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if _, err := d.SetPriceAudited(ctx, ModelPrice{Model: "m", PromptPerMillion: 1}, 0); err == nil {
		t.Fatal("a price change with no actor was accepted")
	}
	if _, err := d.Price(ctx, "m"); err == nil {
		t.Fatal("the refused change still wrote a price")
	}
	var audit int
	if err := d.db.QueryRowContext(ctx, `SELECT count(*) FROM price_changes`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if audit != 0 {
		t.Fatalf("audit rows = %d after a refused change", audit)
	}
}

func TestSetPriceAuditedKeepsCapAndSignChecks(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if _, err := d.SetPriceAudited(ctx, ModelPrice{Model: "m", PromptPerMillion: ledger.MaxRatePerMillion + 1}, 1); err == nil {
		t.Fatal("a price above the cap was accepted")
	}
	if _, err := d.SetPriceAudited(ctx, ModelPrice{Model: "m", PromptPerMillion: -1}, 1); err == nil {
		t.Fatal("a negative price was accepted")
	}
	if _, err := d.SetPriceAudited(ctx, ModelPrice{Model: ""}, 1); err == nil {
		t.Fatal("an empty model was accepted")
	}
}

// A zero price is refused by both write paths: it would bill that half of every
// call at nothing, and the audit row would look perfectly clean.
func TestPriceMustBePositive(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	cases := []ModelPrice{
		{Model: "m", PromptPerMillion: 0, CompletionPerMillion: 100},
		{Model: "m", PromptPerMillion: 100, CompletionPerMillion: 0},
	}
	for _, c := range cases {
		if err := d.SetPrice(ctx, c); !errors.Is(err, ErrPriceNotPositive) {
			t.Fatalf("SetPrice(%+v) = %v, want ErrPriceNotPositive", c, err)
		}
		if _, err := d.SetPriceAudited(ctx, c, 1); !errors.Is(err, ErrPriceNotPositive) {
			t.Fatalf("SetPriceAudited(%+v) = %v, want ErrPriceNotPositive", c, err)
		}
	}
	if _, err := d.Price(ctx, "m"); err == nil {
		t.Fatal("a refused zero price was written")
	}
}

// SetPrice is the deploy path, but it is still a price change: it must leave
// exactly one audit row, attributed to the seed actor, never to a person.
func TestSetPriceIsAuditedAsTheSeedActor(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if err := d.SetPrice(ctx, ModelPrice{Model: "m", PromptPerMillion: 100, CompletionPerMillion: 200}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetPrice(ctx, ModelPrice{Model: "m", PromptPerMillion: 150, CompletionPerMillion: 250}); err != nil {
		t.Fatal(err)
	}

	rows, err := d.PriceChanges(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.ChangedBy != SeedActor {
			t.Fatalf("audit row attributed to %d, want SeedActor %d", r.ChangedBy, SeedActor)
		}
	}
	if rows[0].NewPrompt != 150 || rows[1].NewPrompt != 100 {
		t.Fatalf("audit rows = %+v", rows)
	}
}

// Re-saving an unchanged price must not add an audit row or move updated_at:
// the trail should say what changed, and a no-op edit changed nothing.
func TestSetPriceAuditedNoOpWritesNothing(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	price := ModelPrice{Model: "m", PromptPerMillion: 100, CompletionPerMillion: 200}
	if _, err := d.SetPriceAudited(ctx, price, 11); err != nil {
		t.Fatal(err)
	}
	var firstUpdated int64
	if err := d.db.QueryRowContext(ctx, `SELECT updated_at FROM prices WHERE model = 'm'`).Scan(&firstUpdated); err != nil {
		t.Fatal(err)
	}

	old, err := d.SetPriceAudited(ctx, price, 12)
	if err != nil {
		t.Fatalf("re-saving the same price: %v", err)
	}
	if old.PromptPerMillion != 100 || old.CompletionPerMillion != 200 {
		t.Fatalf("no-op reported old = %+v", old)
	}

	var changes, updated int
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM price_changes`).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if err := d.db.QueryRowContext(ctx, `SELECT updated_at FROM prices WHERE model = 'm'`).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	if changes != 1 {
		t.Fatalf("price_changes rows = %d, want 1 (only the real change)", changes)
	}
	if int64(updated) != firstUpdated {
		t.Fatalf("updated_at moved on a no-op: %d -> %d", firstUpdated, updated)
	}
}
