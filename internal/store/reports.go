package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sort"

	"tgbot/internal/ledger"
)

// CustomerRow is one row of the admin customer list.
type CustomerRow struct {
	TelegramID int64
	Balance    ledger.Amount
	CreatedAt  int64
}

// Customers returns a page of customers ordered by id.
func (d *DB) Customers(ctx context.Context, limit, offset int) ([]CustomerRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT telegram_id, balance, created_at FROM customers
		 ORDER BY telegram_id LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []CustomerRow
	for rows.Next() {
		var c CustomerRow
		var bal int64
		if err := rows.Scan(&c.TelegramID, &bal, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Balance = ledger.Amount(bal)
		out = append(out, c)
	}
	return out, rows.Err()
}

// CustomerCount is the number of customer rows.
func (d *DB) CustomerCount(ctx context.Context) (int64, error) {
	var n int64
	err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM customers`).Scan(&n)
	return n, err
}

// TotalFloat is the money currently held across positive balances. The sum is
// taken in Go: SQLite turns an int64 SUM overflow into REAL, and scanning that
// into int64 fails the query, which used to break the whole dashboard.
func (d *DB) TotalFloat(ctx context.Context) (ledger.Amount, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT balance FROM customers WHERE balance > 0`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var total int64
	for rows.Next() {
		var b int64
		if err := rows.Scan(&b); err != nil {
			return 0, err
		}
		if b > math.MaxInt64-total {
			return ledger.Amount(math.MaxInt64), nil
		}
		total += b
	}
	return ledger.Amount(total), rows.Err()
}

// BalanceAlerts lists customers whose balance has fallen below threshold.
func (d *DB) BalanceAlerts(ctx context.Context, threshold ledger.Amount) ([]CustomerRow, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT telegram_id, balance, created_at FROM customers
		 WHERE balance < ? ORDER BY balance, telegram_id`, int64(threshold))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []CustomerRow
	for rows.Next() {
		var c CustomerRow
		var bal int64
		if err := rows.Scan(&c.TelegramID, &bal, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Balance = ledger.Amount(bal)
		out = append(out, c)
	}
	return out, rows.Err()
}

// WindowSummary is one window of platform activity.
type WindowSummary struct {
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	Spend            ledger.Amount
	ActiveCustomers  int64
}

// SummaryFor aggregates usage since a unix timestamp.
func (d *DB) SummaryFor(ctx context.Context, since int64) (WindowSummary, error) {
	var s WindowSummary
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		        COUNT(DISTINCT telegram_id)
		 FROM usage WHERE created_at >= ?`, since).
		Scan(&s.Requests, &s.PromptTokens, &s.CompletionTokens, &s.ActiveCustomers)
	if err != nil {
		return s, err
	}
	s.Spend, err = d.sumUsageCosts(ctx, costScopeSince, since)
	return s, err
}

// Spender is one customer's spend in a window.
type Spender struct {
	TelegramID int64
	Spend      ledger.Amount
}

// TopSpenders ranks customers by spend since a unix timestamp.
func (d *DB) TopSpenders(ctx context.Context, since int64, limit int) ([]Spender, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT telegram_id, cost FROM usage WHERE created_at >= ?`, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Spender
	indices := make(map[int64]int)
	for rows.Next() {
		var id, cost int64
		if err := rows.Scan(&id, &cost); err != nil {
			return nil, err
		}
		if i, ok := indices[id]; ok {
			out[i].Spend = ledger.Amount(addUsageCosts(int64(out[i].Spend), cost))
		} else {
			indices[id] = len(out)
			out = append(out, Spender{TelegramID: id, Spend: ledger.Amount(cost)})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Spend > out[j].Spend })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// KeyActivity is one key's activity in a window. A row whose key was deleted
// keeps its spend under "(deleted key)" so no money disappears from the books.
type KeyActivity struct {
	KeyID            int64
	Name             string
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	Spend            ledger.Amount
	LastUsed         int64
}

// UsageByKey groups one customer's usage by key since a unix timestamp.
func (d *DB) UsageByKey(ctx context.Context, telegramID, since int64) ([]KeyActivity, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT u.key_id,
		        COALESCE(k.name, '(deleted key)'),
		        COUNT(*),
		        COALESCE(SUM(u.prompt_tokens), 0),
		        COALESCE(SUM(u.completion_tokens), 0),
		        COALESCE(SUM(u.cost), 0),
		        MAX(u.created_at)
		 FROM usage u LEFT JOIN api_keys k ON k.id = u.key_id
		 WHERE u.telegram_id = ? AND u.created_at >= ?
		 GROUP BY u.key_id
		 ORDER BY 6 DESC`, telegramID, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []KeyActivity
	for rows.Next() {
		var k KeyActivity
		var keyID sql.NullInt64
		var spend int64
		if err := rows.Scan(&keyID, &k.Name, &k.Requests, &k.PromptTokens,
			&k.CompletionTokens, &spend, &k.LastUsed); err != nil {
			return nil, err
		}
		k.KeyID = keyID.Int64
		k.Spend = ledger.Amount(spend)
		out = append(out, k)
	}
	return out, rows.Err()
}

// RenameKey renames one key the customer owns.
func (d *DB) RenameKey(ctx context.Context, telegramID, keyID int64, name string) error {
	if name == "" || len(name) > 64 {
		return errors.New("key name must be 1-64 characters")
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE api_keys SET name = ? WHERE id = ? AND telegram_id = ?`, name, keyID, telegramID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ActiveKeyCount is how many active keys a customer holds.
func (d *DB) ActiveKeyCount(ctx context.Context, telegramID int64) (int64, error) {
	var n int64
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM api_keys WHERE telegram_id = ? AND active = 1`, telegramID).Scan(&n)
	return n, err
}

// Claim reads one claim by id.
func (d *DB) Claim(ctx context.Context, claimID int64) (TopUpClaim, error) {
	var c TopUpClaim
	var amount int64
	err := d.db.QueryRowContext(ctx,
		`SELECT id, telegram_id, amount, status, created_at FROM claims WHERE id = ?`, claimID).
		Scan(&c.ID, &c.TelegramID, &amount, &c.Status, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TopUpClaim{}, ErrNotFound
	}
	if err != nil {
		return TopUpClaim{}, err
	}
	c.Amount = ledger.Amount(amount)
	return c, nil
}
