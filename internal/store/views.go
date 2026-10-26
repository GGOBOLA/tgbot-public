package store

import (
	"context"
	"time"

	"tgbot/internal/ledger"
)

// Entry is one ledger row as the dashboard shows it.
type Entry struct {
	ID           int64
	TelegramID   int64
	Delta        ledger.Amount
	BalanceAfter ledger.Amount
	Reason       string
	Ref          string
	CreatedAt    int64
}

// LedgerEntries returns a customer's newest ledger rows first.
func (d *DB) LedgerEntries(ctx context.Context, telegramID int64, limit int) ([]Entry, error) {
	if limit > 500 {
		// One customer's whole history is not a dashboard row list.
		limit = 500
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, telegram_id, delta, balance_after, reason, ref, created_at
		 FROM ledger WHERE telegram_id = ? ORDER BY id DESC LIMIT ?`, telegramID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var entries []Entry
	for rows.Next() {
		var e Entry
		var delta, after int64
		if err := rows.Scan(&e.ID, &e.TelegramID, &delta, &after, &e.Reason, &e.Ref, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Delta = ledger.Amount(delta)
		e.BalanceAfter = ledger.Amount(after)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Latency is the time-to-first-token profile of one model.
type Latency struct {
	Model      string
	Requests   int64
	AvgFirstMS float64
}

// LatencyByModel aggregates first-token latency since a unix timestamp. Rows
// with no first_ms sample (non-streamed calls) are excluded so the average is
// not diluted by zeros.
func (d *DB) LatencyByModel(ctx context.Context, since int64) ([]Latency, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT model, COUNT(*), AVG(first_ms)
		 FROM usage WHERE first_ms > 0 AND created_at >= ?
		 GROUP BY model ORDER BY AVG(first_ms) DESC`, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Latency
	for rows.Next() {
		var l Latency
		if err := rows.Scan(&l.Model, &l.Requests, &l.AvgFirstMS); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// UpstreamStats is how one window of upstream calls went.
type UpstreamStats struct {
	Failed     int64
	Recovered  int64
	GaveUp     int64
	LastStatus int
	LastAt     string
}

// UpstreamSummary aggregates recorded upstream failures since a unix timestamp.
// One row is one customer request that hit at least one failure, not one failed
// attempt, so a three-try retry does not read as three outages.
func (d *DB) UpstreamSummary(ctx context.Context, since int64) (UpstreamStats, error) {
	var u UpstreamStats
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(recovered), 0), COALESCE(SUM(1 - recovered), 0)
		 FROM upstream_errors WHERE created_at >= ?`, since).
		Scan(&u.Failed, &u.Recovered, &u.GaveUp)
	if err != nil {
		return UpstreamStats{}, err
	}
	if u.Failed == 0 {
		return u, nil
	}
	var last int64
	if err := d.db.QueryRowContext(ctx,
		`SELECT status, created_at FROM upstream_errors
		 WHERE created_at >= ? ORDER BY id DESC LIMIT 1`, since).
		Scan(&u.LastStatus, &last); err != nil {
		return u, err
	}
	u.LastAt = time.Unix(last, 0).UTC().Format(time.RFC3339)
	return u, nil
}
