package bot

import (
	"context"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/authz"
	"tgbot/internal/screens"
)

// TestEveryScreenSatisfiesTelegram is the contract: every screen the bot can
// render is checked against Telegram's documented limits, using the same build
// path the handlers use. Three UI bugs reached a customer because the code was
// valid Go and still invalid to Telegram — this is the test that would have
// caught all three.
func TestEveryScreenSatisfiesTelegram(t *testing.T) {
	// The size of the registry is part of the contract: the README advertises
	// 20 text-only screens and 22 once both images are configured. A floor of
	// 18 would still pass with three screens missing, so pin the exact count
	// each configuration must produce.
	for _, tc := range []struct {
		name        string
		welcome, qr string
		want        int
	}{
		{"both images", "assets/welcome.png", "assets/duitnow.jpg", 22},
		{"welcome image only", "assets/welcome.png", "", 21},
		{"text only", "", "", 20},
	} {
		tc := tc
		t.Run("registry/"+tc.name, func(t *testing.T) {
			b, _, _ := newTestBot(t)
			b.welcomePath = tc.welcome
			b.qrPath = tc.qr
			got, err := b.ContractScreens(context.Background(), 1234567890)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				t.Fatalf("%s: %d screens, want exactly %d", tc.name, len(got), tc.want)
			}
		})
	}

	b, _, _ := newTestBot(t)
	b.welcomePath = "assets/welcome.png"
	b.qrPath = "assets/duitnow.jpg"

	screens, err := b.ContractScreens(context.Background(), 1234567890)
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range screens {
		s := s
		t.Run(s.Name, func(t *testing.T) {
			var (
				text    string
				buttons [][]tgbotapi.InlineKeyboardButton
			)
			switch c := s.Chat.(type) {
			case tgbotapi.MessageConfig:
				text = c.Text
				buttons = markupButtons(c.ReplyMarkup)
			case tgbotapi.PhotoConfig:
				text = c.Caption
				if c.Caption == "" {
					t.Fatal("photo message has no caption")
				}
				if len(c.Caption) > MaxCaption {
					t.Fatalf("caption is %d chars, limit %d", len(c.Caption), MaxCaption)
				}
				buttons = markupButtons(c.ReplyMarkup)
			default:
				t.Fatalf("unhandled chattable %T", s.Chat)
			}

			if strings.TrimSpace(text) == "" {
				t.Fatal("empty screen text")
			}
			// Every screen is sent with ParseMode Markdown, so an HTML tag from
			// Telegram's other syntax shows up literally to the reader.
			for _, tag := range []string{"<code>", "</code>", "<b>", "</b>", "<i>", "</i>", "<a ", "<pre>"} {
				if strings.Contains(text, tag) {
					t.Fatalf("HTML tag %s in a Markdown message: %q", tag, text)
				}
			}
			if len(text) > MaxMessageText {
				t.Fatalf("text is %d chars, limit %d", len(text), MaxMessageText)
			}
			// Every screen the bot renders carries a keyboard, so a
			// keyboard-free message is a regression rather than a valid
			// one-way send.
			if len(buttons) == 0 {
				t.Fatal("screen has no keyboard")
			}

			total := 0
			for i, row := range buttons {
				if len(row) == 0 {
					t.Fatalf("row %d is empty", i)
				}
				for _, btn := range row {
					total++
					if strings.TrimSpace(btn.Text) == "" {
						t.Fatalf("row %d has a blank button", i)
					}
					if len([]rune(btn.Text)) > MaxButtonText {
						t.Fatalf("button %q is %d chars, limit %d", btn.Text, len([]rune(btn.Text)), MaxButtonText)
					}
					if btn.CallbackData == nil {
						t.Fatalf("button %q has no callback data", btn.Text)
					}
					// Telegram's hard limit; exceeding it is BUTTON_DATA_INVALID.
					if n := len(*btn.CallbackData); n > 64 {
						t.Fatalf("button %q payload is %d bytes, limit 64", btn.Text, n)
					}
				}
			}
			if total > MaxButtons {
				t.Fatalf("%d buttons, limit %d", total, MaxButtons)
			}
		})
	}
}

// Every button payload must also verify and decode, not merely fit.
func TestEveryContractButtonVerifies(t *testing.T) {
	b, _, codec := newTestBot(t)
	screens, err := b.ContractScreens(context.Background(), 1234567890)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, s := range screens {
		for _, row := range markupButtons(markupOf(s.Chat)) {
			for _, btn := range row {
				payload, err := codec.Verify(*btn.CallbackData)
				if err != nil {
					t.Fatalf("%s / %q: %v", s.Name, btn.Text, err)
				}
				if _, _, _, ok := parsePayload(payload); !ok {
					t.Fatalf("%s / %q decodes to %q", s.Name, btn.Text, payload)
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no buttons were verified")
	}
}

// A screen that overflows a caption must still be sendable: the bot drops to a
// plain message instead of failing on Telegram's 1024-character photo limit.
func TestLongScreenFallsBackToTextMessage(t *testing.T) {
	b, ft, _ := newTestBot(t)
	b.welcomePath = "assets/welcome.png"

	var long strings.Builder
	long.WriteString(strings.Repeat("model-name-and-price line\n", 80))
	if long.Len() <= MaxCaption {
		t.Fatalf("fixture is only %d chars", long.Len())
	}

	// Editing a photo message with an oversized caption must send a new message.
	b.api = &editRejectingTransport{inner: ft, reject: "no text in the message to edit"}
	if err := b.show(7, 900, screens.View{
		Text: long.String(),
		Rows: []screens.Row{{{Text: "⬅️ Menu", Action: authz.ActionMenu, Owner: 7}}},
	}); err != nil {
		t.Fatalf("long screen failed: %v", err)
	}
	if len(ft.sent) == 0 {
		t.Fatal("no fallback message was sent")
	}
}

func markupOf(c tgbotapi.Chattable) interface{} {
	switch m := c.(type) {
	case tgbotapi.MessageConfig:
		return m.ReplyMarkup
	case tgbotapi.PhotoConfig:
		return m.ReplyMarkup
	}
	return nil
}

func markupButtons(m interface{}) [][]tgbotapi.InlineKeyboardButton {
	if kb, ok := m.(tgbotapi.InlineKeyboardMarkup); ok {
		return kb.InlineKeyboard
	}
	return nil
}

// editRejectingTransport fails text edits the way a photo message does, so the
// caption fallback can be exercised.
type editRejectingTransport struct {
	inner  transport
	reject string
}

func (e *editRejectingTransport) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	if _, ok := c.(tgbotapi.EditMessageTextConfig); ok {
		return tgbotapi.Message{}, errorString(e.reject)
	}
	return e.inner.Send(c)
}

func (e *editRejectingTransport) Request(c tgbotapi.Chattable) (*tgbotapi.APIResponse, error) {
	return e.inner.Request(c)
}

type errorString string

func (e errorString) Error() string { return string(e) }

// Every tap must answer the callback exactly once, or Telegram spins the button
// until its own timeout — which reads as a blinking button.
func TestEveryTapAnswersItsCallback(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}

	// A normal screen tap must produce exactly one (quiet) answer.
	token, err := codec.Sign("view_balance:7:0")
	if err != nil {
		t.Fatal(err)
	}
	before := len(ft.answers)
	b.Route(ctx, callbackUpdate(7, token))
	if got := len(ft.answers) - before; got != 1 {
		t.Fatalf("a normal tap answered %d times, want 1", got)
	}

	// A refusal answers once, with its message, and is not overwritten by the
	// fallback answer.
	denied, err := codec.Sign("admin_credit:7:1000000000")
	if err != nil {
		t.Fatal(err)
	}
	before = len(ft.answers)
	b.Route(ctx, callbackUpdate(7, denied))
	if got := len(ft.answers) - before; got != 1 {
		t.Fatalf("a refused tap answered %d times, want 1", got)
	}
	if !strings.Contains(ft.answers[len(ft.answers)-1], "cannot") {
		t.Fatalf("refusal was replaced by the fallback: %q", ft.answers[len(ft.answers)-1])
	}
}
