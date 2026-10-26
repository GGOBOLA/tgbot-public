package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/authz"
	"tgbot/internal/ledger"
	"tgbot/internal/screens"
	"tgbot/internal/store"
)

// textUpdate is a plain (non-command) message, which is what the bot used to
// ignore entirely and now routes only while a price draft is live.
func textUpdate(userID int64, text string) tgbotapi.Update {
	return tgbotapi.Update{Message: &tgbotapi.Message{
		From: &tgbotapi.User{ID: userID},
		Chat: &tgbotapi.Chat{ID: userID},
		Text: text,
	}}
}

// commandUpdate is what Telegram actually delivers for "/cancel": the text plus
// a bot_command entity at offset 0, which is what IsCommand checks.
func commandUpdate(userID int64, text string) tgbotapi.Update {
	u := textUpdate(userID, text)
	u.Message.Entities = []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: len(text)}}
	return u
}

func mustSign(t *testing.T, sign func(string) (string, error), payload string) string {
	t.Helper()
	token, err := sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestPriceEditHappyPath(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.SetPrice(ctx, store.ModelPrice{
		Model: "m1", PromptPerMillion: ledger.UsecPerMYR, CompletionPerMillion: 2 * ledger.UsecPerMYR,
	}); err != nil {
		t.Fatal(err)
	}

	// The editable list is a button per model, not the customer price list.
	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_prices:1:0")))
	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_prices_priced:1:0")))
	if last := ft.sent[len(ft.sent)-1]; !strings.Contains(last, "Priced models") || !strings.Contains(last, "m1") {
		t.Fatalf("admin price list = %q", last)
	}
	if ft.last == nil {
		t.Fatal("no screen was sent")
	}
	kb, ok := ft.last.ReplyMarkup.(tgbotapi.InlineKeyboardMarkup)
	if !ok || len(kb.InlineKeyboard) == 0 {
		t.Fatalf("the admin price list carries no buttons: %T", ft.last.ReplyMarkup)
	}

	// Open the model, then choose the input price.
	arg := screens.ModelArg("m1")
	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_price_model:1:"+itoa(arg))))
	if last := ft.sent[len(ft.sent)-1]; !strings.Contains(last, "Which price") {
		t.Fatalf("edit screen = %q", last)
	}
	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_price_edit:1:"+itoa(arg*2))))
	if _, ok := b.drafts.get(1); !ok {
		t.Fatal("no draft was recorded for the admin")
	}
	if last := ft.sent[len(ft.sent)-1]; !strings.Contains(last, "Send the new price") {
		t.Fatalf("prompt = %q", last)
	}

	// A plain message is now accepted, and produces a confirm screen.
	b.Route(ctx, textUpdate(1, "1.5"))
	if last := ft.sent[len(ft.sent)-1]; !strings.Contains(last, "Confirm price change") ||
		!strings.Contains(last, "RM 1.00 → RM 1.50") {
		t.Fatalf("confirm screen = %q", last)
	}

	// Nothing is written until Apply.
	if p, err := b.db.Price(ctx, "m1"); err != nil || p.PromptPerMillion != ledger.UsecPerMYR {
		t.Fatalf("price changed before apply: %+v, %v", p, err)
	}

	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_price_apply:1:0")))

	want := ledger.Amount(150_000_000) // RM 1.50 per 1M tokens
	p, err := b.db.Price(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if p.PromptPerMillion != want || p.CompletionPerMillion != 2*ledger.UsecPerMYR {
		t.Fatalf("price = %+v, want in %d and out unchanged", p, want)
	}
	if last := ft.sent[len(ft.sent)-1]; !strings.Contains(last, "Set prices") {
		t.Fatalf("after apply the list was not refreshed: %q", last)
	}

	// The change is attributable.
	// Two rows: the seed write that created the fixture, then this edit. The
	// newest is the dashboard edit and must carry the admin's id.
	audit, err := b.db.PriceChanges(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 2 {
		t.Fatalf("audit rows = %d, want 2 (seed + edit)", len(audit))
	}
	if audit[0].ChangedBy != 1 || audit[0].NewPrompt != want || audit[0].OldPrompt != ledger.UsecPerMYR {
		t.Fatalf("newest audit row = %+v", audit[0])
	}
	if audit[1].ChangedBy != store.SeedActor {
		t.Fatalf("seed row actor = %d, want %d", audit[1].ChangedBy, store.SeedActor)
	}

	// Apply is single-shot: the same button again cannot move the price.
	if err := b.db.SetPrice(ctx, store.ModelPrice{Model: "m1", PromptPerMillion: 42, CompletionPerMillion: 84}); err != nil {
		t.Fatal(err)
	}
	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_price_apply:1:0")))
	if p, _ := b.db.Price(ctx, "m1"); p.PromptPerMillion != 42 {
		t.Fatalf("a replayed apply changed the price to %+v", p)
	}
}

func TestStalePriceButtonDoesNotRetargetAfterEarlierModelInserted(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	for _, model := range []string{"b-model", "z-model"} {
		if err := b.db.SetPrice(ctx, store.ModelPrice{Model: model, PromptPerMillion: 100, CompletionPerMillion: 200}); err != nil {
			t.Fatal(err)
		}
	}
	prices, err := b.db.Prices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view := screens.AdminPricesPriced(1, prices, 0)
	var staleArg int64
	for _, row := range view.Rows {
		for _, button := range row {
			if button.Action == authz.ActionAdminPriceModel && strings.Contains(button.Text, "z-model") {
				staleArg = button.Arg
			}
		}
	}
	if staleArg == 0 {
		t.Fatal("rendered list did not contain z-model button")
	}
	if err := b.db.SetPrice(ctx, store.ModelPrice{Model: "a-model", PromptPerMillion: 300, CompletionPerMillion: 400}); err != nil {
		t.Fatal(err)
	}
	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_price_model:1:"+itoa(staleArg))))
	if last := ft.sent[len(ft.sent)-1]; !strings.Contains(last, "z-model") || strings.Contains(last, "b-model") {
		t.Fatalf("stale button opened the wrong model: %q", last)
	}
}

func TestPriceEditIsAdminOnly(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.SetPrice(ctx, store.ModelPrice{Model: "m1", PromptPerMillion: 100, CompletionPerMillion: 200}); err != nil {
		t.Fatal(err)
	}

	// Customer 7 is not an admin: the signed button is refused by authz, so no
	// draft exists and no prompt is sent.
	b.Route(ctx, callbackUpdate(7, mustSign(t, codec.Sign, "admin_price_edit:7:"+itoa(screens.ModelArg("m1")*2))))
	if _, ok := b.drafts.get(7); ok {
		t.Fatal("a customer got a price draft")
	}
	for _, s := range ft.sent {
		if strings.Contains(s, "Send the new price") {
			t.Fatalf("customer was prompted for a price: %q", s)
		}
	}

	// Their plain messages are ignored, as before the feature.
	before := len(ft.sent)
	b.Route(ctx, textUpdate(7, "1.5"))
	if len(ft.sent) != before {
		t.Fatalf("a customer message got a reply: %v", ft.sent[before:])
	}
	if p, _ := b.db.Price(ctx, "m1"); p.PromptPerMillion != 100 {
		t.Fatalf("price changed by a customer: %+v", p)
	}
}

func TestPriceInputRejectsBadNumbersAndKeepsTheDraft(t *testing.T) {
	b, ft, _ := newTestBot(t)
	ctx := context.Background()

	b.drafts.put(1, priceDraft{model: "m1", field: screens.PricePrompt, old: 100})

	for _, bad := range []string{"abc", "1e3", "1,5", "-2", "10001"} {
		b.Route(ctx, textUpdate(1, bad))
		last := ft.sent[len(ft.sent)-1]
		if strings.Contains(last, "Confirm price change") {
			t.Fatalf("%q was accepted as a price", bad)
		}
		if !strings.Contains(last, "not a price I can read") && !strings.Contains(last, "above") {
			t.Fatalf("%q produced %q", bad, last)
		}
		if _, ok := b.drafts.get(1); !ok {
			t.Fatalf("%q dropped the draft, so the admin has to start over", bad)
		}
	}

	// Zero is refused: a model priced at zero bills that half at nothing.
	b.Route(ctx, textUpdate(1, "0"))
	if last := ft.sent[len(ft.sent)-1]; strings.Contains(last, "Confirm price change") {
		t.Fatalf("a zero price reached the confirm screen: %q", last)
	}
	if last := ft.sent[len(ft.sent)-1]; !strings.Contains(last, "greater than zero") {
		t.Fatalf("zero produced %q", last)
	}
}

func TestPriceDraftExpires(t *testing.T) {
	b, ft, _ := newTestBot(t)
	ctx := context.Background()
	b.drafts.ttl = time.Millisecond
	b.drafts.put(1, priceDraft{model: "m1", field: screens.PricePrompt, old: 100})

	time.Sleep(5 * time.Millisecond)
	before := len(ft.sent)
	b.Route(ctx, textUpdate(1, "1.5"))
	if len(ft.sent) != before {
		t.Fatalf("an expired draft still consumed the message: %v", ft.sent[before:])
	}
}

func TestCancelClearsTheDraft(t *testing.T) {
	b, ft, _ := newTestBot(t)
	ctx := context.Background()
	b.drafts.put(1, priceDraft{model: "m1", field: screens.PricePrompt, old: 100})

	b.Route(ctx, commandUpdate(1, "/cancel"))
	if _, ok := b.drafts.get(1); ok {
		t.Fatal("the draft survived /cancel")
	}
	if last := ft.sent[len(ft.sent)-1]; !strings.Contains(last, "Cancelled") {
		t.Fatalf("cancel reply = %q", last)
	}
}

// Navigating away must abandon the edit, or a number typed later (for any
// reason) would be read as the price of a draft the admin has forgotten about.
func TestMenuClearsTheDraft(t *testing.T) {
	b, _, codec := newTestBot(t)
	ctx := context.Background()
	b.drafts.put(1, priceDraft{model: "m1", field: screens.PricePrompt, old: 100})

	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "menu:1:0")))
	if _, ok := b.drafts.get(1); ok {
		t.Fatal("the draft survived a return to the menu")
	}
}

// The price flow must draw every step on the dashboard message it started from.
// Sending the prompt or confirm as new messages is what stranded the welcome
// photo one screen behind, leaving the dashboard as plain text.
func TestPricePromptsStayOnTheDashboardMessage(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.SetPrice(ctx, store.ModelPrice{Model: "m1", PromptPerMillion: ledger.UsecPerMYR, CompletionPerMillion: 2 * ledger.UsecPerMYR}); err != nil {
		t.Fatal(err)
	}

	const dash = 500
	b.Route(ctx, callbackUpdateWithMessage(1, mustSign(t, codec.Sign, "admin_price_edit:1:"+itoa(screens.ModelArg("m1")*2)), dash))
	if len(ft.sent) != 0 {
		t.Fatalf("the prompt started a new message instead of editing the dashboard: %v", ft.sent)
	}
	if len(ft.edits) != 1 || ft.edits[0].MessageID != dash {
		t.Fatalf("prompt edit = %+v", ft.edits)
	}
	if !strings.Contains(ft.edits[0].Text, "Send the new price") {
		t.Fatalf("prompt text = %q", ft.edits[0].Text)
	}

	b.Route(ctx, textUpdate(1, "1.5"))
	if len(ft.sent) != 0 {
		t.Fatalf("the confirm screen started a new message: %v", ft.sent)
	}
	last := ft.edits[len(ft.edits)-1]
	if last.MessageID != dash || !strings.Contains(last.Text, "Confirm price change") {
		t.Fatalf("confirm edit = id %d, %q", last.MessageID, last.Text)
	}

	b.Route(ctx, callbackUpdateWithMessage(1, mustSign(t, codec.Sign, "admin_price_apply:1:0"), dash))
	last = ft.edits[len(ft.edits)-1]
	if last.MessageID != dash || !strings.Contains(last.Text, "Set prices") {
		t.Fatalf("after apply the dashboard was not reused: id %d, %q", last.MessageID, last.Text)
	}
	if p, err := b.db.Price(ctx, "m1"); err != nil || p.PromptPerMillion != 150_000_000 {
		t.Fatalf("price = %+v, %v", p, err)
	}
}
