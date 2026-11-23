package store

import (
	"context"
	"database/sql"
	"errors"

	"tgbot/internal/ledger"
)

// Account is one dashboard row.
type Account struct {
	TelegramID int64
	Balance    ledger.Amount
	CreatedAt  int64
}

// Account reads one customer, returning ErrNotFound when the row is absent.
func (d *DB) Account(ctx context.Context, telegramID int64) (Account, error) {
	var c Account
	var balance int64
	err := d.db.QueryRowContext(ctx,
		`SELECT telegram_id, balance, created_at FROM customers WHERE telegram_id = ?`, telegramID).
		Scan(&c.TelegramID, &balance, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, err
	}
	c.Balance = ledger.Amount(balance)
	return c, nil
}

// UsageWindow sums one customer's usage since a unix timestamp. The dashboard
// uses it for "last 7 days" without a rollup table: the usage index on
// (telegram_id, created_at) answers it directly. Keep all aggregates in one
// result set so a concurrent charge cannot produce a mixed snapshot.
func (d *DB) UsageWindow(ctx context.Context, telegramID, since int64) (requests, prompt, completion int64, spend ledger.Amount, err error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT prompt_tokens, completion_tokens, cost
		 FROM usage WHERE telegram_id = ? AND created_at >= ?`, telegramID, since)
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

// pruneBatch is how many rows one DELETE may remove. The store runs on a single
// connection, so an unbounded DELETE held the only writer while every bill
// queued behind it and could lose a charge to its 15 s context.
const pruneBatch = 500

// PruneUsage deletes usage rows older than the given timestamp and reports how
// many went, in bounded batches on the shared connection.
func (d *DB) PruneUsage(ctx context.Context, before int64) (int64, error) {
	return d.pruneInBatches(ctx, `DELETE FROM usage WHERE id IN (SELECT id FROM usage WHERE created_at < ? LIMIT ?)`, before)
}

// PruneUpstreamErrors deletes failure rows older than the given timestamp, in
// batches for the same reason as PruneUsage.
func (d *DB) PruneUpstreamErrors(ctx context.Context, before int64) (int64, error) {
	return d.pruneInBatches(ctx, `DELETE FROM upstream_errors WHERE id IN (SELECT id FROM upstream_errors WHERE created_at < ? LIMIT ?)`, before)
}

// pruneInBatches runs a bounded DELETE until it stops removing rows, so the
// single write connection is released between batches.
func (d *DB) pruneInBatches(ctx context.Context, stmt string, before int64) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := d.db.ExecContext(ctx, stmt, before, pruneBatch)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < pruneBatch {
			return total, nil
		}
	}
}
