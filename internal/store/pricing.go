package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"tgbot/internal/ledger"
)

// ModelPrice is one model's per-million-token cost.
type ModelPrice struct {
	Model                string
	PromptPerMillion     ledger.Amount
	CompletionPerMillion ledger.Amount
	UpdatedAt            int64
}

// Prices lists every configured model price.
func (d *DB) Prices(ctx context.Context) ([]ModelPrice, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT model, prompt_per_million, completion_per_million, updated_at
		 FROM prices ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var prices []ModelPrice
	for rows.Next() {
		var p ModelPrice
		if err := rows.Scan(&p.Model, &p.PromptPerMillion, &p.CompletionPerMillion, &p.UpdatedAt); err != nil {
			return nil, err
		}
		prices = append(prices, p)
	}
	return prices, rows.Err()
}

// Price reads one model's price.
func (d *DB) Price(ctx context.Context, model string) (ModelPrice, error) {
	var p ModelPrice
	err := d.db.QueryRowContext(ctx,
		`SELECT model, prompt_per_million, completion_per_million, updated_at
		 FROM prices WHERE model = ?`, model).
		Scan(&p.Model, &p.PromptPerMillion, &p.CompletionPerMillion, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelPrice{}, ErrNotFound
	}
	if err != nil {
		return ModelPrice{}, err
	}
	return p, nil
}

// Rates converts a stored price into the pure cost model.
func (p ModelPrice) Rates() ledger.Rates {
	return ledger.Rates{
		PromptPerMillion:     p.PromptPerMillion,
		CompletionPerMillion: p.CompletionPerMillion,
	}
}

// PriceChange is one recorded edit of a model's price.
type PriceChange struct {
	Model         string
	OldPrompt     ledger.Amount
	OldCompletion ledger.Amount
	NewPrompt     ledger.Amount
	NewCompletion ledger.Amount
	ChangedBy     int64
	CreatedAt     int64
}

// PriceChanges lists the audit rows for one model, newest first.
func (d *DB) PriceChanges(ctx context.Context, model string) ([]PriceChange, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT model, old_prompt, old_completion, new_prompt, new_completion, changed_by, created_at
		 FROM price_changes WHERE model = ? ORDER BY id DESC LIMIT 200`, model)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []PriceChange
	for rows.Next() {
		var c PriceChange
		if err := rows.Scan(&c.Model, &c.OldPrompt, &c.OldCompletion, &c.NewPrompt, &c.NewCompletion,
			&c.ChangedBy, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ErrPriceNotPositive refuses a zero price. A model priced at zero bills that
// half of every call at nothing, forever, and the audit row would look clean.
var ErrPriceNotPositive = errors.New("price must be greater than zero")

// validatePrice keeps a price inside the range the gateway can bill safely.
func validatePrice(p ModelPrice) error {
	if p.Model == "" {
		return errors.New("model is required")
	}
	if p.PromptPerMillion < 0 || p.CompletionPerMillion < 0 {
		return ledger.ErrNegative
	}
	if p.PromptPerMillion == 0 || p.CompletionPerMillion == 0 {
		return ErrPriceNotPositive
	}
	if p.PromptPerMillion > ledger.MaxRatePerMillion || p.CompletionPerMillion > ledger.MaxRatePerMillion {
		return fmt.Errorf("price above the %d µsen/1M cap", int64(ledger.MaxRatePerMillion))
	}
	return nil
}

// SeedActor records a price change made by the deploy (cmd/seedprices), not by
// a person at the dashboard. It is a fixed negative id so it can never collide
// with a Telegram user id, which is always positive.
const SeedActor int64 = -1

// SetPrice upserts one model price from the deploy path. It goes through the
// same audited write as a dashboard edit, with SeedActor as the actor, so no
// price can move without a price_changes row — the promise schema v3 makes.
func (d *DB) SetPrice(ctx context.Context, p ModelPrice) error {
	_, err := d.SetPriceAudited(ctx, p, SeedActor)
	return err
}

// SetPriceAudited upserts one model price and records who changed it, in one
// transaction: a price that moved without an audit row, or an audit row for a
// price that never landed, is a half-applied money change. It returns the
// price as it was before the edit (zero values when the model was new).
func (d *DB) SetPriceAudited(ctx context.Context, p ModelPrice, changedBy int64) (ModelPrice, error) {
	if err := validatePrice(p); err != nil {
		return ModelPrice{}, err
	}
	if changedBy == 0 {
		return ModelPrice{}, errors.New("a price change needs an actor")
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelPrice{}, err
	}
	defer func() { _ = tx.Rollback() }()

	old := ModelPrice{Model: p.Model}
	err = tx.QueryRowContext(ctx,
		`SELECT prompt_per_million, completion_per_million, updated_at FROM prices WHERE model = ?`,
		p.Model).Scan(&old.PromptPerMillion, &old.CompletionPerMillion, &old.UpdatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ModelPrice{}, err
	}

	// An edit that changes nothing is not a change: it must not move updated_at
	// or add an audit row claiming somebody altered the price.
	if err == nil && old.PromptPerMillion == p.PromptPerMillion &&
		old.CompletionPerMillion == p.CompletionPerMillion {
		if err := tx.Commit(); err != nil {
			return ModelPrice{}, err
		}
		return old, nil
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO prices (model, prompt_per_million, completion_per_million, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(model) DO UPDATE SET
		   prompt_per_million = excluded.prompt_per_million,
		   completion_per_million = excluded.completion_per_million,
		   updated_at = excluded.updated_at`,
		p.Model, int64(p.PromptPerMillion), int64(p.CompletionPerMillion), nowUnix()); err != nil {
		return ModelPrice{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO price_changes
		   (model, old_prompt, old_completion, new_prompt, new_completion, changed_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.Model, int64(old.PromptPerMillion), int64(old.CompletionPerMillion),
		int64(p.PromptPerMillion), int64(p.CompletionPerMillion), changedBy, nowUnix()); err != nil {
		return ModelPrice{}, err
	}

	if err := tx.Commit(); err != nil {
		return ModelPrice{}, err
	}
	return old, nil
}
