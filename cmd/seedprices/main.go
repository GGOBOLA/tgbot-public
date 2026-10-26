// Command seedprices loads a model price list into the store.
//
//	seedprices -db data/bot.db -prices prices.json
//
// The file is a JSON array; prices are RM strings with up to 8 decimals:
//
//	[{"model": "riverai/deepseek-ai/DeepSeek-V4.1-Flash-1017",
//	  "prompt_per_million": "1.00",
//	  "completion_per_million": "2.00"}]
//
// It is idempotent: running it twice leaves the same rows, and it does not
// overwrite a model that is already priced unless -force is passed — a price
// edited from the dashboard must not be reverted by a routine re-seed. A model
// that is not seeded is refused by the gateway as an unknown model, which is
// why this is a deploy step and not an afterthought.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"tgbot/internal/ledger"
	"tgbot/internal/store"
)

type entry struct {
	Model                string `json:"model"`
	PromptPerMillion     string `json:"prompt_per_million"`
	CompletionPerMillion string `json:"completion_per_million"`
}

func main() {
	dbPath := flag.String("db", "", "path to the SQLite database (required)")
	listPath := flag.String("prices", "", "path to the JSON price list (required)")
	force := flag.Bool("force", false, "overwrite models that already have a price")
	flag.Parse()

	if err := run(context.Background(), *dbPath, *listPath, *force); err != nil {
		fmt.Fprintln(os.Stderr, "seedprices:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dbPath, listPath string, force bool) error {
	if dbPath == "" || listPath == "" {
		return errors.New("-db and -prices are both required")
	}
	prices, err := load(listPath)
	if err != nil {
		return err
	}
	db, err := store.OpenDB(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	existing, err := db.Prices(ctx)
	if err != nil {
		return err
	}
	priced := make(map[string]bool, len(existing))
	for _, p := range existing {
		priced[p.Model] = true
	}

	var written, skipped int
	for _, p := range prices {
		if priced[p.Model] && !force {
			fmt.Printf("kept    %-56s already priced (use -force to overwrite)\n", p.Model)
			skipped++
			continue
		}
		if err := db.SetPrice(ctx, p); err != nil {
			return fmt.Errorf("set %s: %w", p.Model, err)
		}
		fmt.Printf("priced  %-56s in %s  out %s\n",
			p.Model, ledger.FormatMYR(p.PromptPerMillion), ledger.FormatMYR(p.CompletionPerMillion))
		written++
	}
	fmt.Printf("%d priced, %d kept in %s\n", written, skipped, dbPath)
	return nil
}

// load reads and validates the price list before anything is written, so a
// typo in one entry cannot leave a half-seeded table.
func load(path string) ([]store.ModelPrice, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied path on the deploy host
	if err != nil {
		return nil, err
	}
	var entries []entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s lists no models", path)
	}

	out := make([]store.ModelPrice, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for i, e := range entries {
		if e.Model == "" {
			return nil, fmt.Errorf("entry %d: model is empty", i+1)
		}
		if seen[e.Model] {
			return nil, fmt.Errorf("entry %d: %s listed twice", i+1, e.Model)
		}
		seen[e.Model] = true

		prompt, err := ledger.ParseAmount(e.PromptPerMillion)
		if err != nil {
			return nil, fmt.Errorf("entry %d (%s): prompt price: %w", i+1, e.Model, err)
		}
		completion, err := ledger.ParseAmount(e.CompletionPerMillion)
		if err != nil {
			return nil, fmt.Errorf("entry %d (%s): completion price: %w", i+1, e.Model, err)
		}
		if prompt < 0 || completion < 0 {
			return nil, fmt.Errorf("entry %d (%s): price is negative", i+1, e.Model)
		}
		out = append(out, store.ModelPrice{
			Model:                e.Model,
			PromptPerMillion:     prompt,
			CompletionPerMillion: completion,
		})
	}
	return out, nil
}
