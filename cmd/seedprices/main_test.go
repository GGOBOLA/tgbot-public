package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"tgbot/internal/ledger"
	"tgbot/internal/store"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadParsesRMPrices(t *testing.T) {
	p, err := load(write(t, `[
		{"model":"riverai/deepseek-ai/DeepSeek-V4.1-Flash-1017",
		 "prompt_per_million":"1.00","completion_per_million":"2.00"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 1 {
		t.Fatalf("got %d entries", len(p))
	}
	if want := ledger.Amount(1) * ledger.UsecPerMYR; p[0].PromptPerMillion != want {
		t.Fatalf("prompt = %d, want %d", p[0].PromptPerMillion, want)
	}
	if want := ledger.Amount(2) * ledger.UsecPerMYR; p[0].CompletionPerMillion != want {
		t.Fatalf("completion = %d, want %d", p[0].CompletionPerMillion, want)
	}
}

func TestLoadRefusesBadLists(t *testing.T) {
	cases := map[string]string{
		"empty list":    `[]`,
		"no model":      `[{"prompt_per_million":"1","completion_per_million":"2"}]`,
		"bad price":     `[{"model":"m","prompt_per_million":"1.2.3","completion_per_million":"2"}]`,
		"negative":      `[{"model":"m","prompt_per_million":"-1","completion_per_million":"2"}]`,
		"duplicate row": `[{"model":"m","prompt_per_million":"1","completion_per_million":"2"},{"model":"m","prompt_per_million":"3","completion_per_million":"4"}]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := load(write(t, body)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// A routine re-seed must not revert a price an admin set from the dashboard:
// that is the whole reason -force exists.
func TestRunKeepsAnExistingPriceUnlessForced(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bot.db")
	list := write(t, `[{"model":"m","prompt_per_million":"1.00","completion_per_million":"2.00"}]`)

	if err := run(ctx, dbPath, list, false); err != nil {
		t.Fatalf("first seed: %v", err)
	}

	// The admin raises the input price from the dashboard.
	db, err := store.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetPriceAudited(ctx, store.ModelPrice{
		Model: "m", PromptPerMillion: 5 * ledger.UsecPerMYR, CompletionPerMillion: 2 * ledger.UsecPerMYR,
	}, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// A plain re-seed leaves it alone.
	if err := run(ctx, dbPath, list, false); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	db, err = store.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	p, err := db.Price(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if p.PromptPerMillion != 5*ledger.UsecPerMYR {
		t.Fatalf("re-seed reverted the dashboard price: %+v", p)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// -force restores the file's value, which is how an operator takes back
	// control of a price list.
	if err := run(ctx, dbPath, list, true); err != nil {
		t.Fatalf("forced seed: %v", err)
	}
	db, err = store.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	p, err = db.Price(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if p.PromptPerMillion != ledger.UsecPerMYR {
		t.Fatalf("forced seed did not overwrite: %+v", p)
	}
}

// A model the file adds is seeded even without -force: the guard is about
// overwriting, not about refusing new rows.
func TestRunAddsNewModelsWithoutForce(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bot.db")

	first := write(t, `[{"model":"a","prompt_per_million":"1.00","completion_per_million":"2.00"}]`)
	if err := run(ctx, dbPath, first, false); err != nil {
		t.Fatal(err)
	}
	second := write(t, `[{"model":"a","prompt_per_million":"9.00","completion_per_million":"9.00"},
	                       {"model":"b","prompt_per_million":"3.00","completion_per_million":"4.00"}]`)
	if err := run(ctx, dbPath, second, false); err != nil {
		t.Fatal(err)
	}

	db, err := store.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	a, err := db.Price(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if a.PromptPerMillion != ledger.UsecPerMYR {
		t.Fatalf("existing model was overwritten without -force: %+v", a)
	}
	b, err := db.Price(ctx, "b")
	if err != nil {
		t.Fatalf("new model was not seeded: %v", err)
	}
	if b.PromptPerMillion != 3*ledger.UsecPerMYR || b.CompletionPerMillion != 4*ledger.UsecPerMYR {
		t.Fatalf("new model price = %+v", b)
	}
}
