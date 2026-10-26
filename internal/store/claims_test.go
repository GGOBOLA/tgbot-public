package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"tgbot/internal/ledger"
)

func TestClaimApproveCreditsOnce(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 200); err != nil {
		t.Fatal(err)
	}

	claim, err := d.OpenClaim(ctx, 200, ledger.Amount(10)*ledger.UsecPerMYR)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := d.OpenClaim(ctx, 200, ledger.Amount(20)*ledger.UsecPerMYR); !errors.Is(err, ErrClaimOpen) {
		t.Fatalf("second open = %v, want ErrClaimOpen", err)
	}

	bal, err := d.ApproveClaim(ctx, claim.ID, 1)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if want := ledger.Amount(10) * ledger.UsecPerMYR; bal != want {
		t.Fatalf("balance = %d, want %d", bal, want)
	}

	if _, err := d.ApproveClaim(ctx, claim.ID, 1); !errors.Is(err, ErrClaimSettled) {
		t.Fatalf("second approve = %v, want ErrClaimSettled", err)
	}
}

func TestExpiredClaimCannotBeApproved(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 202); err != nil {
		t.Fatal(err)
	}
	claim, err := d.OpenClaim(ctx, 202, ledger.Amount(10)*ledger.UsecPerMYR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.ExecContext(ctx, `UPDATE claims SET expires_at = ? WHERE id = ?`, nowUnix()-1, claim.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApproveClaim(ctx, claim.ID, 1); !errors.Is(err, ErrClaimExpired) {
		t.Fatalf("approve expired claim = %v, want ErrClaimExpired", err)
	}
	bal, err := d.Balance(ctx, 202)
	if err != nil {
		t.Fatal(err)
	}
	if bal != 0 {
		t.Fatalf("balance after expired approval = %d, want 0", bal)
	}
	var status string
	if err := d.db.QueryRowContext(ctx, `SELECT status FROM claims WHERE id = ?`, claim.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("expired claim status = %q, want pending", status)
	}
}

func TestSweepExpiredClaimsAllowsNewClaim(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 203); err != nil {
		t.Fatal(err)
	}
	claim, err := d.OpenClaim(ctx, 203, ledger.Amount(10)*ledger.UsecPerMYR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.ExecContext(ctx, `UPDATE claims SET expires_at = ? WHERE id = ?`, nowUnix()-1, claim.ID); err != nil {
		t.Fatal(err)
	}
	n, err := d.SweepExpiredClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("swept = %d claims, want 1", n)
	}
	var status string
	var decidedAt sql.NullInt64
	if err := d.db.QueryRowContext(ctx, `SELECT status, decided_at FROM claims WHERE id = ?`, claim.ID).Scan(&status, &decidedAt); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || !decidedAt.Valid {
		t.Fatalf("swept claim status/decision = %q/%v, want rejected with decision time", status, decidedAt)
	}
	if _, err := d.OpenClaim(ctx, 203, ledger.Amount(5)*ledger.UsecPerMYR); err != nil {
		t.Fatalf("open after sweep: %v", err)
	}
}

func TestClaimRejectMovesNoMoney(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 201); err != nil {
		t.Fatal(err)
	}
	claim, err := d.OpenClaim(ctx, 201, ledger.Amount(5)*ledger.UsecPerMYR)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RejectClaim(ctx, claim.ID, 1); err != nil {
		t.Fatalf("reject: %v", err)
	}
	bal, err := d.Balance(ctx, 201)
	if err != nil {
		t.Fatal(err)
	}
	if bal != 0 {
		t.Fatalf("balance after reject = %d, want 0", bal)
	}
}

// A claim decision without a deciding admin would credit money and record
// decided_by = 0, which is indistinguishable from "no human approved this".
func TestClaimDecisionNeedsAnAdmin(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 300); err != nil {
		t.Fatal(err)
	}
	claim, err := d.OpenClaim(ctx, 300, ledger.Amount(5)*ledger.UsecPerMYR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApproveClaim(ctx, claim.ID, 0); !errors.Is(err, ErrNoActor) {
		t.Fatalf("approve with admin 0 = %v, want ErrNoActor", err)
	}
	if err := d.RejectClaim(ctx, claim.ID, -1); !errors.Is(err, ErrNoActor) {
		t.Fatalf("reject with admin -1 = %v, want ErrNoActor", err)
	}

	// The claim is untouched and still approvable by a real admin.
	var status string
	if err := d.db.QueryRowContext(ctx, `SELECT status FROM claims WHERE id = ?`, claim.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("status after refused decisions = %q, want pending", status)
	}
	if _, err := d.ApproveClaim(ctx, claim.ID, 7); err != nil {
		t.Fatalf("approve with a real admin: %v", err)
	}
}
