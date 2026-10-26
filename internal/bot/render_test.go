package bot

import (
	"errors"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/screens"
	"tgbot/internal/store"
)

// Every in-place screen edit must carry a parse mode, or Telegram renders the
// bold markers literally ("*Menu*"), which is what a customer sees after a
// screen is edited instead of re-sent.
func TestShowSetsParseModeOnEdits(t *testing.T) {
	b, ft, _ := newTestBot(t)
	if err := b.show(1, 77, screens.Menu(1, true)); err != nil {
		t.Fatal(err)
	}
	if len(ft.edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(ft.edits))
	}
	if got := ft.edits[0].ParseMode; got != tgbotapi.ModeMarkdown {
		t.Fatalf("edit parse mode = %q, want Markdown", got)
	}
}

// Screens carrying outside data (model ids, key names) go out without a parse
// mode: Markdown v1 has no escape syntax, so a stray '_' would either render
// wrong or make Telegram reject the message outright.
func TestShowSendsPlainScreensWithoutParseMode(t *testing.T) {
	b, ft, _ := newTestBot(t)
	prices := []store.ModelPrice{{Model: "provider/model_with_underscore*v2", PromptPerMillion: 100}}
	if err := b.show(1, 78, screens.AdminPricesPriced(1, prices, 0)); err != nil {
		t.Fatal(err)
	}
	if len(ft.edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(ft.edits))
	}
	if got := ft.edits[0].ParseMode; got != "" {
		t.Fatalf("edit parse mode = %q, want none for a plain screen", got)
	}
}

// The price screens are built from upstream ids, so they must be plain.
func TestPriceScreensArePlain(t *testing.T) {
	customer := screens.Prices(1, []store.ModelPrice{{Model: "a_b"}})
	if !customer.Plain {
		t.Fatal("customer price list is not plain")
	}
	admin := screens.AdminPricesUnpriced(1, []string{"a_b"}, 0)
	if !admin.Plain {
		t.Fatal("admin price list is not plain")
	}
	edit := screens.AdminPriceEdit(1, store.ModelPrice{Model: "a_b"})
	if !edit.Plain {
		t.Fatal("admin price edit is not plain")
	}
	confirm := screens.AdminPriceConfirm(1, "a_b", screens.PricePrompt, 0, 1)
	if !confirm.Plain {
		t.Fatal("price confirm is not plain")
	}
	add := screens.AdminPriceAddConfirm(1, "a_b", 0, 1)
	if !add.Plain {
		t.Fatal("price add confirm is not plain")
	}
}

// A photo dashboard has no text to edit: the caption edit that follows must
// treat Telegram's "message is not modified" as success, not as a failed
// screen, or every Refresh toast reads "Something went wrong".
func TestShowTreatsUnmodifiedCaptionAsSuccess(t *testing.T) {
	b, ft, _ := newTestBot(t)
	ft.errOn = func(c tgbotapi.Chattable) error {
		switch c.(type) {
		case tgbotapi.EditMessageTextConfig:
			return errors.New("Bad Request: there is no text in the message to edit")
		case tgbotapi.EditMessageCaptionConfig:
			return errors.New("Bad Request: message is not modified: specified new message content is the same")
		}
		return nil
	}
	if err := b.show(1, 9, screens.Menu(1, true)); err != nil {
		t.Fatalf("an unchanged caption was reported as an error: %v", err)
	}
}

// And a real caption failure is still reported.
func TestShowReportsRealCaptionFailure(t *testing.T) {
	b, ft, _ := newTestBot(t)
	ft.errOn = func(c tgbotapi.Chattable) error {
		switch c.(type) {
		case tgbotapi.EditMessageTextConfig:
			return errors.New("Bad Request: there is no text in the message to edit")
		case tgbotapi.EditMessageCaptionConfig:
			return errors.New("Bad Request: message caption is too long")
		}
		return nil
	}
	if err := b.show(1, 9, screens.Menu(1, true)); err == nil {
		t.Fatal("a rejected caption was swallowed")
	}
}
