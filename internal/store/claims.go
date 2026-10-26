package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"tgbot/internal/ledger"
)

// TopUpClaim is one DuitNow top-up claim.
type TopUpClaim struct {
	ID         int64
	TelegramID int64
	Amount     ledger.Amount
	Status     string
	CreatedAt  int64
}

// claimTTL is how long a pending top-up claim can be approved. Claims expire
// after 72 hours so abandoned claims stop blocking a customer's next claim.
const claimTTL = 72 * time.Hour

var (
	// ErrClaimOpen means the customer already has a pending claim.
	ErrClaimOpen = errors.New("a claim is already pending")
	// ErrClaimSettled means the claim is no longer pending.
	ErrClaimSettled = errors.New("claim already decided")
	// ErrClaimExpired means the pending claim is past its approval deadline.
	ErrClaimExpired = errors.New("claim expired")
)

// OpenClaim opens one pending claim. The partial unique index makes a second
// open claim for the same customer fail even under a double tap.
func (d *DB) OpenClaim(ctx context.Context, telegramID int64, amount ledger.Amount) (TopUpClaim, error) {
	if telegramID <= 0 {
		return TopUpClaim{}, errors.New("telegram id must be positive")
	}
	if amount <= 0 {
		return TopUpClaim{}, ledger.ErrNegative
	}
	created := nowUnix()
	expires := created + int64(claimTTL/time.Second)
	res, err := d.db.ExecContext(ctx,
		`INSERT INTO claims (telegram_id, amount, status, created_at, expires_at) VALUES (?, ?, 'pending', ?, ?)`,
		telegramID, int64(amount), created, expires)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return TopUpClaim{}, ErrClaimOpen
		}
		return TopUpClaim{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return TopUpClaim{}, err
	}
	return TopUpClaim{ID: id, TelegramID: telegramID, Amount: amount, Status: "pending", CreatedAt: created}, nil
}

// ApproveClaim marks the claim approved and credits the balance in the same
// transaction. The ledger ref claim:<id> is the idempotency guard, so a
// repeated approval cannot credit twice even if the status guard is bypassed.
func (d *DB) ApproveClaim(ctx context.Context, claimID, adminID int64) (ledger.Amount, error) {
	if adminID <= 0 {
		return 0, ErrNoActor
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var telegramID, amount int64
	var status string
	var expiresAt sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT telegram_id, amount, status, expires_at FROM claims WHERE id = ?`, claimID).
		Scan(&telegramID, &amount, &status, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if status != "pending" {
		return 0, ErrClaimSettled
	}
	if expiresAt.Valid && expiresAt.Int64 <= nowUnix() {
		return 0, ErrClaimExpired
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE claims SET status = 'approved', decided_at = ?, decided_by = ?
		 WHERE id = ? AND status = 'pending'`, nowUnix(), adminID, claimID)
	if err != nil {
		return 0, err
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if moved != 1 {
		return 0, ErrClaimSettled
	}

	balance, _, err := applyTx(ctx, tx, telegramID, ledger.Amount(amount), "claim", fmt.Sprintf("claim:%d", claimID))
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return balance, nil
}

// SweepExpiredClaims rejects pending claims whose approval deadline has passed.
// The rejected status and expiry timestamp preserve the reason for the
// decision without adding a separate decision-reason column.
func (d *DB) SweepExpiredClaims(ctx context.Context) (int64, error) {
	res, err := d.db.ExecContext(ctx,
		`UPDATE claims SET status = 'rejected', decided_at = ?
		 WHERE status = 'pending' AND expires_at IS NOT NULL AND expires_at <= ?`, nowUnix(), nowUnix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RejectClaim marks a pending claim rejected. No money moves.
func (d *DB) RejectClaim(ctx context.Context, claimID, adminID int64) error {
	if adminID <= 0 {
		return ErrNoActor
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE claims SET status = 'rejected', decided_at = ?, decided_by = ?
		 WHERE id = ? AND status = 'pending'`, nowUnix(), adminID, claimID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrClaimSettled
	}
	return nil
}

// OpenClaims lists pending claims, oldest first.
func (d *DB) OpenClaims(ctx context.Context) ([]TopUpClaim, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, telegram_id, amount, status, created_at
		 FROM claims WHERE status = 'pending' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var claims []TopUpClaim
	for rows.Next() {
		var c TopUpClaim
		if err := rows.Scan(&c.ID, &c.TelegramID, &c.Amount, &c.Status, &c.CreatedAt); err != nil {
			return nil, err
		}
		claims = append(claims, c)
	}
	return claims, rows.Err()
}
