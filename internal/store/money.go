package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"tgbot/internal/ledger"
)

// errRefTaken means a concurrent writer already inserted this ledger ref; the
// outer transaction rolls back and reports the winning balance.
var errRefTaken = errors.New("ledger ref already taken")

// Credit adds money to a customer and appends one ledger row in one
// transaction. A non-empty ref makes the call idempotent.
func (d *DB) Credit(ctx context.Context, telegramID int64, amount ledger.Amount, reason, ref string) (ledger.Amount, error) {
	if amount <= 0 {
		return 0, ledger.ErrNegative
	}
	return d.apply(ctx, telegramID, amount, reason, ref)
}

// Charge debits a customer, allowing a visible debt rather than dropping a
// served call. A zero cost is a no-op.
func (d *DB) Charge(ctx context.Context, telegramID int64, cost ledger.Amount, reason, ref string) (ledger.Amount, error) {
	if cost < 0 {
		return 0, ledger.ErrNegative
	}
	if cost == 0 {
		return d.Balance(ctx, telegramID)
	}
	return d.apply(ctx, telegramID, -cost, reason, ref)
}

// apply runs applyTx in its own transaction and fires the low-balance hook
// after the commit.
func (d *DB) apply(ctx context.Context, telegramID int64, delta ledger.Amount, reason, ref string) (ledger.Amount, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	// A panic between BeginTx and Commit must not leak the transaction: the pool
	// holds one connection, so a leaked tx wedges every later request.
	defer func() { _ = tx.Rollback() }()
	balance, crossed, err := applyTx(ctx, tx, telegramID, delta, reason, ref)
	if err != nil {
		_ = tx.Rollback()
		if errors.Is(err, errRefTaken) {
			if rerr := resolveRefTaken(ctx, d.db, telegramID, delta, ref); rerr != nil {
				return 0, rerr
			}
			// The replay lost the unique index; the first writer's balance
			// is the correct answer.
			return d.Balance(ctx, telegramID)
		}
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if crossed {
		d.notifyLowBalance(telegramID, balance)
	}
	return balance, nil
}

// applyTx moves money inside an existing transaction so a caller can pair it
// with another write (a claim decision) atomically. crossed reports a downward
// crossing of ledger.LowBalanceThreshold; the caller fires the hook after
// commit.
func applyTx(ctx context.Context, tx *sql.Tx, telegramID int64, delta ledger.Amount, reason, ref string) (ledger.Amount, bool, error) {
	if telegramID <= 0 {
		return 0, false, fmt.Errorf("telegram id must be positive, got %d", telegramID)
	}
	if strings.TrimSpace(reason) == "" {
		return 0, false, errors.New("a ledger reason is required")
	}
	if delta == 0 {
		// The schema rejects this too, but as a raw CHECK error; naming it here
		// keeps a future caller's bug out of the database layer's failures.
		return 0, false, errors.New("a ledger delta must not be zero")
	}

	if ref != "" {
		existingOwner, existingDelta, err := lookupRef(ctx, tx, ref)
		switch {
		case err == nil:
			// A replay is only a replay for the same customer and amount. Any
			// other match means the ref collided, and reporting success would
			// silently drop this caller's money.
			if existingOwner != telegramID || existingDelta != int64(delta) {
				return 0, false, ErrRefMismatch
			}
			balance, err := balanceInTx(ctx, tx, telegramID)
			return balance, false, err
		case !errors.Is(err, sql.ErrNoRows):
			return 0, false, err
		}
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE customers SET balance = balance + ? WHERE telegram_id = ?`,
		int64(delta), telegramID)
	if err != nil {
		// The balance CHECK is the last line of defence against an amount that
		// would leave the money range; report it as money, not as a DB fault.
		if strings.Contains(err.Error(), "CHECK constraint failed") {
			return 0, false, ledger.ErrOverflow
		}
		return 0, false, err
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if moved == 0 {
		return 0, false, ErrNotFound
	}

	balance, err := balanceInTx(ctx, tx, telegramID)
	if err != nil {
		return 0, false, err
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO ledger (telegram_id, delta, balance_after, reason, ref, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		telegramID, int64(delta), int64(balance), reason, ref, nowUnix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return 0, false, errRefTaken
		}
		return 0, false, err
	}

	// Only a downward crossing warns; a customer parked below the line is not
	// pinged again on every request.
	before := balance - delta
	crossed := delta < 0 && balance < ledger.LowBalanceThreshold && before >= ledger.LowBalanceThreshold
	return balance, crossed, nil
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// resolveRefTaken decides what a lost unique-ref race means: the same customer
// and delta is the original replay, anything else is a collision that must not
// be reported as success.
func resolveRefTaken(ctx context.Context, q rowQueryer, telegramID int64, delta ledger.Amount, ref string) error {
	owner, existingDelta, err := lookupRef(ctx, q, ref)
	if err != nil {
		return err
	}
	if owner != telegramID || existingDelta != int64(delta) {
		return ErrRefMismatch
	}
	return nil
}

func lookupRef(ctx context.Context, q rowQueryer, ref string) (int64, int64, error) {
	var owner, delta int64
	err := q.QueryRowContext(ctx,
		`SELECT telegram_id, delta FROM ledger WHERE ref = ?`, ref).
		Scan(&owner, &delta)
	return owner, delta, err
}

func balanceInTx(ctx context.Context, tx *sql.Tx, telegramID int64) (ledger.Amount, error) {
	var balance int64
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM customers WHERE telegram_id = ?`, telegramID).Scan(&balance); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return ledger.Amount(balance), nil
}
