package store

import (
	"context"
	"errors"

	"tgbot/internal/ledger"
)

// ChargeUsage records one served request and debits its cost in a single
// transaction, so a balance never moves without the usage row that justifies
// it and a usage row never exists for money that was not taken.
func (d *DB) ChargeUsage(ctx context.Context, rec UsageEntry) (ledger.Amount, error) {
	if rec.TelegramID <= 0 {
		return 0, errors.New("telegram id must be positive")
	}
	if rec.Cost < 0 {
		return 0, ledger.ErrNegative
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Refuse an unknown customer before writing anything, so the caller gets
	// ErrNotFound instead of a foreign-key violation.
	if _, err := balanceInTx(ctx, tx, rec.TelegramID); err != nil {
		return 0, err
	}

	var keyID any
	if rec.KeyID != 0 {
		keyID = rec.KeyID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO usage (telegram_id, key_id, model, prompt_tokens, completion_tokens, cost, created_at, first_ms, estimated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.TelegramID, keyID, rec.Model, rec.PromptTokens, rec.CompletionTokens,
		int64(rec.Cost), nowUnix(), rec.FirstMS, boolInt(rec.Estimated)); err != nil {
		return 0, err
	}

	var balance ledger.Amount
	crossed := false
	if rec.Cost == 0 {
		// A free or failed call still records the row; it just moves no money.
		balance, err = balanceInTx(ctx, tx, rec.TelegramID)
	} else {
		balance, crossed, err = applyTx(ctx, tx, rec.TelegramID, -rec.Cost, "usage", "")
	}
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if crossed {
		d.notifyLowBalance(rec.TelegramID, balance)
	}
	return balance, nil
}
