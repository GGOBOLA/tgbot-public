package store

import (
	"context"
	"errors"
	"math"

	"tgbot/internal/ledger"
)

// UsageEntry is one billed request.
type UsageEntry struct {
	TelegramID       int64
	KeyID            int64 // 0 means the key row is gone
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	Cost             ledger.Amount
	FirstMS          int64
	// Estimated marks a charge taken from the pre-flight hold because the
	// upstream reported no usage: Cost is the only number available, not what
	// the call will really cost. Reconciliation excludes these rows, so a known
	// gap cannot be mistaken for drift.
	Estimated bool
}

// boolInt stores a flag as the 0/1 the schema's CHECK constraints expect.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RecordUsage appends one usage row. Billing itself goes through Charge so the
// balance and the ledger stay in step.
func (d *DB) RecordUsage(ctx context.Context, rec UsageEntry) error {
	if rec.TelegramID <= 0 {
		return errors.New("telegram id must be positive")
	}
	if rec.Cost < 0 {
		return ledger.ErrNegative
	}
	var keyID any
	if rec.KeyID != 0 {
		keyID = rec.KeyID
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO usage (telegram_id, key_id, model, prompt_tokens, completion_tokens, cost, created_at, first_ms, estimated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.TelegramID, keyID, rec.Model, rec.PromptTokens, rec.CompletionTokens, int64(rec.Cost), nowUnix(), rec.FirstMS,
		boolInt(rec.Estimated))
	return err
}

// UsageTotals returns lifetime requests, tokens and spend for one customer.
// All fields come from one result set: separate SQL SUM queries can overflow
// int64 and can observe different snapshots while a charge is committing.
func (d *DB) UsageTotals(ctx context.Context, telegramID int64) (requests, prompt, completion int64, spend ledger.Amount, err error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT prompt_tokens, completion_tokens, cost FROM usage WHERE telegram_id = ?`, telegramID)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer func() { _ = rows.Close() }()
	var totalCost int64
	for rows.Next() {
		var p, c, cost int64
		if err := rows.Scan(&p, &c, &cost); err != nil {
			return 0, 0, 0, 0, err
		}
		requests++
		prompt = addUsageCosts(prompt, p)
		completion = addUsageCosts(completion, c)
		totalCost = addUsageCosts(totalCost, cost)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, 0, err
	}
	return requests, prompt, completion, ledger.Amount(totalCost), nil
}

// addUsageCosts accumulates nonnegative token and money values without allowing
// SQLite's SUM to turn an int64 overflow into a failed dashboard query.
func addUsageCosts(total, cost int64) int64 {
	if cost > 0 && total > math.MaxInt64-cost {
		return math.MaxInt64
	}
	if cost < 0 && total < math.MinInt64-cost {
		return math.MinInt64
	}
	return total + cost
}
