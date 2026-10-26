package bot

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"tgbot/internal/authz"
	"tgbot/internal/ledger"
)

// openClaim drives the customer's confirm button and returns the claim id.
func openClaim(t *testing.T, b *Bot, codec interface {
	Sign(string) (string, error)
}, customer int64, amount ledger.Amount) int64 {
	t.Helper()
	token, err := codec.Sign("confirm_claim:" + itoa(customer) + ":" + itoa(int64(amount)))
	if err != nil {
		t.Fatal(err)
	}
	b.Route(context.Background(), callbackUpdate(customer, token))
	claims, err := b.db.OpenClaims(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(claims))
	}
	return claims[0].ID
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func TestConfirmClaimRejectsUnlistedAmount(t *testing.T) {
	b, ft, codec := newTestBot(t)
	if err := b.db.EnsureCustomer(context.Background(), 7); err != nil {
		t.Fatal(err)
	}

	token, err := codec.Sign("confirm_claim:7:999999999999")
	if err != nil {
		t.Fatal(err)
	}
	b.Route(context.Background(), callbackUpdate(7, token))

	claims, err := b.db.OpenClaims(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("a claim was opened for an unlisted amount: %+v", claims)
	}
	if len(ft.answers) == 0 || !strings.Contains(ft.answers[0], "not offered") {
		t.Fatalf("answers = %v", ft.answers)
	}
}

func TestApproveCreditsExactlyOnce(t *testing.T) {
	b, _, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}

	claimID := openClaim(t, b, codec, 7, 10*ledger.UsecPerMYR)

	approve, err := codec.Sign("admin_approve_claim:0:" + itoa(claimID))
	if err != nil {
		t.Fatal(err)
	}
	b.Route(ctx, callbackUpdate(1, approve)) // user 1 is the admin

	bal, err := b.db.Balance(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	want := ledger.Amount(10) * ledger.UsecPerMYR
	if bal != want {
		t.Fatalf("balance = %d, want %d", bal, want)
	}

	// The same button again must not credit twice.
	b.Route(ctx, callbackUpdate(1, approve))
	bal, err = b.db.Balance(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if bal != want {
		t.Fatalf("balance after replay = %d, want %d", bal, want)
	}
}

func TestCustomerCannotApproveOwnClaim(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}

	claimID := openClaim(t, b, codec, 7, 10*ledger.UsecPerMYR)

	approve, err := codec.Sign("admin_approve_claim:0:" + itoa(claimID))
	if err != nil {
		t.Fatal(err)
	}
	before := len(ft.answers)
	b.Route(ctx, callbackUpdate(7, approve)) // the customer, not an admin

	bal, err := b.db.Balance(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if bal != 0 {
		t.Fatalf("customer self-approved: balance = %d", bal)
	}
	if len(ft.answers) == before || !strings.Contains(ft.answers[before], "cannot") {
		t.Fatalf("answers = %v, want a refusal", ft.answers[before:])
	}
}

func TestRejectLeavesBalanceAlone(t *testing.T) {
	b, _, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}

	claimID := openClaim(t, b, codec, 7, 50*ledger.UsecPerMYR)
	reject, err := codec.Sign("admin_reject_claim:0:" + itoa(claimID))
	if err != nil {
		t.Fatal(err)
	}
	b.Route(ctx, callbackUpdate(1, reject))

	bal, err := b.db.Balance(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if bal != 0 {
		t.Fatalf("reject moved money: %d", bal)
	}
	claim, err := b.db.Claim(ctx, claimID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Status != "rejected" {
		t.Fatalf("status = %q, want rejected", claim.Status)
	}
}

func TestReloadAmountsAreOffered(t *testing.T) {
	for _, a := range ReloadAmounts {
		if !IsReloadAmount(a) {
			t.Fatalf("%d not accepted by IsReloadAmount", a)
		}
	}
	if IsReloadAmount(1) {
		t.Fatal("1 µsen accepted as a reload amount")
	}
	if !(authz.Rules{}).Can(authz.Actor{UserID: 7}, authz.ActionConfirmClaim, authz.Resource{OwnerID: 7}) {
		t.Fatal("a customer cannot open their own claim")
	}
}
