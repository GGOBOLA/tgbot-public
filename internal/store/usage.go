package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
func (d *DB) UsageTotals(ctx context.Context, telegramID int64) (requests, prompt, completion int64, spend ledger.Amount, err error) {
	err = d.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0)
		 FROM usage WHERE telegram_id = ?`, telegramID).
		Scan(&requests, &prompt, &completion)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, 0, nil
	}
	if err != nil {
		return requests, prompt, completion, 0, err
	}
	spend, err = d.sumUsageCosts(ctx, costScopeOwner, telegramID)
	return requests, prompt, completion, spend, err
}

// usageCostScope names one of the fixed predicates below. It is an enum, not a
// string, so no caller can hand this function SQL: the query text only ever
// comes from the constants in this file.
type usageCostScope int

const (
	// costScopeOwner: every cost row for one customer.
	costScopeOwner usageCostScope = iota
	// costScopeOwnerSince: one customer's costs at or after a timestamp.
	costScopeOwnerSince
	// costScopeSince: every cost row at or after a timestamp.
	costScopeSince
)

// sumUsageCosts accumulates individual costs so SQLite never has to represent
// an overflowing SUM as REAL. Totals outside ledger.Amount's range saturate.
func (d *DB) sumUsageCosts(ctx context.Context, scope usageCostScope, args ...any) (ledger.Amount, error) {
	var query string
	switch scope {
	case costScopeOwner:
		query = `SELECT cost FROM usage WHERE telegram_id = ?`
	case costScopeOwnerSince:
		query = `SELECT cost FROM usage WHERE telegram_id = ? AND created_at >= ?`
	case costScopeSince:
		query = `SELECT cost FROM usage WHERE created_at >= ?`
	default:
		return 0, fmt.Errorf("unknown usage cost scope %d", scope)
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var total int64
	for rows.Next() {
		var cost int64
		if err := rows.Scan(&cost); err != nil {
			return 0, err
		}
		total = addUsageCosts(total, cost)
	}
	return ledger.Amount(total), rows.Err()
}

func addUsageCosts(total, cost int64) int64 {
	if cost > 0 && total > math.MaxInt64-cost {
		return math.MaxInt64
	}
	if cost < 0 && total < math.MinInt64-cost {
		return math.MinInt64
	}
	return total + cost
}
