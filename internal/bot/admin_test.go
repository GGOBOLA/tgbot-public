package bot

import (
	"context"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/ledger"
	"tgbot/internal/store"
)

func callbackUpdateWithMessage(userID int64, data string, messageID int) tgbotapi.Update {
	u := callbackUpdate(userID, data)
	u.CallbackQuery.Message = &tgbotapi.Message{
		MessageID: messageID,
		Chat:      &tgbotapi.Chat{ID: userID},
	}
	return u
}

func TestAdminUsersList(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	for _, id := range []int64{7, 8} {
		if err := b.db.EnsureCustomer(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	token, err := codec.Sign("admin_list_users:0:0")
	if err != nil {
		t.Fatal(err)
	}
	b.Route(ctx, callbackUpdate(1, token))

	if len(ft.sent) == 0 {
		t.Fatal("no screen was sent")
	}
	if !strings.Contains(ft.sent[len(ft.sent)-1], "Users") {
		t.Fatalf("screen = %q", ft.sent[len(ft.sent)-1])
	}
}

func TestAdminCreditIsIdempotentPerMessage(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}

	amount := ledger.Amount(10) * ledger.UsecPerMYR
	token, err := codec.Sign("admin_credit:7:" + itoa(int64(amount)))
	if err != nil {
		t.Fatal(err)
	}

	// The same button on the same message twice.
	b.Route(ctx, callbackUpdateWithMessage(1, token, 500))
	b.Route(ctx, callbackUpdateWithMessage(1, token, 500))

	bal, err := b.db.Balance(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if bal != amount {
		t.Fatalf("balance = %d, want exactly one credit of %d", bal, amount)
	}

	// The customer was told.
	found := false
	for _, s := range ft.sent {
		if strings.Contains(s, "credited") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("customer never told: %v", ft.sent)
	}
}

func TestAdminCreditRejectsUnlistedAmount(t *testing.T) {
	b, _, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}
	token, err := codec.Sign("admin_credit:7:12345")
	if err != nil {
		t.Fatal(err)
	}
	b.Route(ctx, callbackUpdateWithMessage(1, token, 501))

	bal, err := b.db.Balance(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if bal != 0 {
		t.Fatalf("unlisted credit applied: %d", bal)
	}
}

func TestCustomerCannotCredit(t *testing.T) {
	b, _, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}
	amount := ledger.Amount(10) * ledger.UsecPerMYR
	token, err := codec.Sign("admin_credit:7:" + itoa(int64(amount)))
	if err != nil {
		t.Fatal(err)
	}
	b.Route(ctx, callbackUpdateWithMessage(7, token, 502)) // not an admin

	bal, err := b.db.Balance(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if bal != 0 {
		t.Fatalf("customer self-credited: %d", bal)
	}
}

func TestAdminToday(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if err := b.db.RecordUsage(ctx, store.UsageEntry{
		TelegramID: 7, Model: "m", PromptTokens: 100, Cost: ledger.Amount(100),
	}); err != nil {
		t.Fatal(err)
	}

	token, err := codec.Sign("admin_today:0:0")
	if err != nil {
		t.Fatal(err)
	}
	b.Route(ctx, callbackUpdate(1, token))

	if len(ft.sent) == 0 || !strings.Contains(ft.sent[len(ft.sent)-1], "Today") {
		t.Fatalf("sent = %v", ft.sent)
	}
}
