package bot

import (
	"context"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/ledger"
)

// creditTap is one real tap: a fresh callback id, the message it sits on.
func creditTap(userID int64, data string, messageID int, callbackID string) tgbotapi.Update {
	u := callbackUpdateWithMessage(userID, data, messageID)
	u.CallbackQuery.ID = callbackID
	return u
}

// The dashboard reuses one message id for every screen, so two different
// customers can be credited from the same message with the same amount. The old
// ref (`admin:<admin>:<msg>:<amount>`) made the second one a silent no-op that
// still answered "Credited".
func TestAdminCreditTwoCustomersFromOneMessage(t *testing.T) {
	b, _, codec := newTestBot(t)
	ctx := context.Background()
	for _, id := range []int64{7, 8} {
		if err := b.db.EnsureCustomer(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	amount := ledger.Amount(10) * ledger.UsecPerMYR

	tokA, err := codec.Sign("admin_credit:7:" + itoa(int64(amount)))
	if err != nil {
		t.Fatal(err)
	}
	tokB, err := codec.Sign("admin_credit:8:" + itoa(int64(amount)))
	if err != nil {
		t.Fatal(err)
	}

	b.Route(ctx, creditTap(1, tokA, 500, "tap-a"))
	b.Route(ctx, creditTap(1, tokB, 500, "tap-b"))

	for _, id := range []int64{7, 8} {
		bal, err := b.db.Balance(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if bal != amount {
			t.Fatalf("customer %d balance = %d, want %d (a credit was swallowed)", id, bal, amount)
		}
	}
}

func TestAdminCreditSameCustomerAndAmountAgainFromSameMessage(t *testing.T) {
	b, _, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}
	amount := ledger.Amount(50) * ledger.UsecPerMYR
	token, err := codec.Sign("admin_credit:7:" + itoa(int64(amount)))
	if err != nil {
		t.Fatal(err)
	}
	b.Route(ctx, creditTap(1, token, 500, "payment-1"))
	b.Route(ctx, creditTap(1, token, 500, "payment-2"))

	bal, err := b.db.Balance(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if bal != 2*amount {
		t.Fatalf("balance = %d, want two credits of %d", bal, 2*amount)
	}
}

// A double tap on the same button (two callback ids, same message, owner and
// amount) must credit once: the second tap is refused, not silently ignored.
func TestAdminCreditDoubleTapWithUnchangedBalanceClaimsOnce(t *testing.T) {
	guard := newCreditGuard()
	// Both taps came from the same rendered card, before either credit changed
	// the balance, so the handler derives the same key for both.
	key := "admin:7:500:50000000:0"
	if !guard.claim(key) {
		t.Fatal("first tap was not claimed")
	}
	if guard.claim(key) {
		t.Fatal("second tap with unchanged balance was claimed")
	}
}
