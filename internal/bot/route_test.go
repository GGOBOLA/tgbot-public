package bot

import (
	"context"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/authz"
	"tgbot/internal/screens"
)

// The dashboard shows a balance, and after a key is minted it shows the
// plaintext key. A /start in a group must render nothing at all.
func TestGroupStartRendersNothing(t *testing.T) {
	b, ft, _ := newTestBot(t)
	ctx := context.Background()

	u := textUpdate(1, "/start")
	u.Message.Entities = []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: len("/start")}}
	u.Message.Chat = &tgbotapi.Chat{ID: -1001234567890, Type: "group"}
	b.Route(ctx, u)

	if len(ft.sent) != 0 || ft.photos != 0 {
		t.Fatalf("a group /start rendered %d messages and %d photos", len(ft.sent), ft.photos)
	}
}

// A button pressed in a group is answered but never acted on.
func TestGroupCallbackIsRefused(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.EnsureCustomer(ctx, 1); err != nil {
		t.Fatal(err)
	}
	u := callbackUpdateWithMessage(1, mustSign(t, codec.Sign, "generate_key:1:0"), 500)
	u.CallbackQuery.Message.Chat = &tgbotapi.Chat{ID: -1001234567890, Type: "supergroup"}
	b.Route(ctx, u)

	if len(ft.sent) != 0 {
		t.Fatalf("a group button rendered a screen: %v", ft.sent)
	}
	found := false
	for _, a := range ft.answers {
		if strings.Contains(a, "private chat") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refusal was not answered: %v", ft.answers)
	}
}

// A price draft belongs to the chat its dashboard lives in: a number typed in
// another chat must not be captured as the price.
func TestPriceDraftIgnoresOtherChats(t *testing.T) {
	b, ft, _ := newTestBot(t)
	ctx := context.Background()
	b.drafts.put(1, priceDraft{
		mode: draftEdit, model: "m1", field: screens.PricePrompt,
		old: 1, chatID: 1,
	})

	other := textUpdate(1, "1.5")
	other.Message.Chat = &tgbotapi.Chat{ID: 2, Type: "private"}
	b.Route(ctx, other)

	if len(ft.sent) != 0 || len(ft.edits) != 0 {
		t.Fatalf("a message from another chat was consumed: sent=%v edits=%d", ft.sent, len(ft.edits))
	}
	if d, ok := b.drafts.get(1); !ok || d.hasValue {
		t.Fatalf("the draft changed on a foreign message: %+v", d)
	}

	// The right chat still works. With no dashboard message id recorded, the
	// confirm screen is a new message rather than an edit.
	right := textUpdate(1, "1.5")
	b.Route(ctx, right)
	confirmed := len(ft.edits) > 0 && strings.Contains(ft.edits[len(ft.edits)-1].Text, "Confirm price change")
	for _, sent := range ft.sent {
		if strings.Contains(sent, "Confirm price change") {
			confirmed = true
		}
	}
	if !confirmed {
		t.Fatalf("the draft's own chat could not set the price: sent=%v edits=%d", ft.sent, len(ft.edits))
	}
}

// A message update without a sender must not panic: the bot shares a process
// with the gateway, so one bad update used to take billing down with it.
func TestRouteIgnoresMessageWithoutSender(t *testing.T) {
	b, ft, _ := newTestBot(t)
	u := textUpdate(1, "hello")
	u.Message.From = nil
	b.Route(context.Background(), u) // must not panic
	if len(ft.sent) != 0 {
		t.Fatalf("a senderless message produced a screen: %v", ft.sent)
	}
}

// A panic inside a handler is one dropped update, not a process exit.
func TestHandlerPanicIsRecovered(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}
	b.Handle(authz.ActionViewBalance, func(context.Context, *tgbotapi.CallbackQuery, int64, int64) error {
		panic("boom")
	})
	token, err := codec.Sign("view_balance:7:0")
	if err != nil {
		t.Fatal(err)
	}
	b.Route(ctx, callbackUpdate(7, token)) // must not panic

	// The bot is still alive and routes the next update.
	before := len(ft.answers)
	b.Route(ctx, callbackUpdate(7, token))
	if len(ft.answers) == before {
		t.Fatal("the bot stopped answering after a recovered panic")
	}
}
