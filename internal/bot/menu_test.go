package bot

import (
	"context"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/authz"
)

// recordingTransport captures outgoing text messages for assertions.
type recordingTransport struct {
	messages []tgbotapi.MessageConfig
	edits    []tgbotapi.EditMessageTextConfig
}

func (r *recordingTransport) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	switch m := c.(type) {
	case tgbotapi.MessageConfig:
		r.messages = append(r.messages, m)
	case tgbotapi.EditMessageTextConfig:
		r.edits = append(r.edits, m)
	}
	return tgbotapi.Message{}, nil
}

func (r *recordingTransport) Request(c tgbotapi.Chattable) (*tgbotapi.APIResponse, error) {
	return &tgbotapi.APIResponse{Ok: true}, nil
}

func startMessage(userID int64) tgbotapi.Update {
	msg := &tgbotapi.Message{
		MessageID: 1,
		From:      &tgbotapi.User{ID: userID},
		Chat:      &tgbotapi.Chat{ID: userID},
		Text:      "/start",
		Entities:  []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 6}},
	}
	return tgbotapi.Update{Message: msg}
}

// /start must answer with the dashboard and a signed keyboard: a text-only
// answer is the difference between a bot and a wall.
func TestStartSendsMenuWithButtons(t *testing.T) {
	b, ft, codec := newTestBot(t)
	b.Route(context.Background(), startMessage(7))

	if ft.last == nil {
		t.Fatal("no message was sent")
	}
	markup, ok := ft.last.ReplyMarkup.(tgbotapi.InlineKeyboardMarkup)
	if !ok {
		t.Fatalf("no inline keyboard attached: %T", ft.last.ReplyMarkup)
	}

	var labels []string
	var reloadData string
	for _, row := range markup.InlineKeyboard {
		for _, btn := range row {
			labels = append(labels, btn.Text)
			if strings.Contains(btn.Text, "Reload") && btn.CallbackData != nil {
				reloadData = *btn.CallbackData
			}
		}
	}
	if len(labels) == 0 {
		t.Fatal("keyboard is empty")
	}
	for _, want := range []string{"Balance", "Reload", "Usage", "keys"} {
		found := false
		for _, l := range labels {
			if strings.Contains(l, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("menu lacks a %q button: %v", want, labels)
		}
	}

	// Every button must be signed and authorized for its own owner.
	if reloadData == "" {
		t.Fatal("reload button carries no payload")
	}
	payload, err := codec.Verify(reloadData)
	if err != nil {
		t.Fatalf("button payload does not verify: %v", err)
	}
	action, owner, _, ok := parsePayload(payload)
	if !ok || action != authz.ActionClaimTopUp || owner != 7 {
		t.Fatalf("reload payload = %q (%s/%d)", payload, action, owner)
	}

	// The customer was persisted, so a later claim has a row to attach to.
	if _, err := b.db.Balance(context.Background(), 7); err != nil {
		t.Fatalf("customer row missing after /start: %v", err)
	}
}

// Admins get the extra rows; customers must not.
func TestStartHidesAdminRowsFromCustomers(t *testing.T) {
	b, ft, _ := newTestBot(t)
	b.Route(context.Background(), startMessage(7))
	for _, row := range ft.last.ReplyMarkup.(tgbotapi.InlineKeyboardMarkup).InlineKeyboard {
		for _, btn := range row {
			if strings.Contains(btn.Text, "Users") || strings.Contains(btn.Text, "Today") {
				t.Fatalf("customer saw an admin button: %q", btn.Text)
			}
		}
	}

	adminFt := &fakeTransport{}
	b.api = adminFt
	b.Route(context.Background(), startMessage(1)) // 1 is the admin in the fixture
	sawUsers := false
	for _, row := range adminFt.last.ReplyMarkup.(tgbotapi.InlineKeyboardMarkup).InlineKeyboard {
		for _, btn := range row {
			if strings.Contains(btn.Text, "Users") {
				sawUsers = true
			}
		}
	}
	if !sawUsers {
		t.Fatal("admin menu lacks the Users button")
	}
}

// Every button the screens can build must fit Telegram's 64-byte limit, or the
// send fails with BUTTON_DATA_INVALID at runtime instead of here.
func TestEveryMenuButtonFitsTelegram(t *testing.T) {
	b, ft, _ := newTestBot(t)
	b.Route(context.Background(), startMessage(1)) // admin sees the most rows

	markup, ok := ft.last.ReplyMarkup.(tgbotapi.InlineKeyboardMarkup)
	if !ok {
		t.Fatal("no keyboard")
	}
	for _, row := range markup.InlineKeyboard {
		for _, btn := range row {
			if btn.CallbackData == nil {
				t.Fatalf("%q has no data", btn.Text)
			}
			if n := len(*btn.CallbackData); n > MaxCallbackData {
				t.Fatalf("%q payload is %d bytes, limit %d", btn.Text, n, MaxCallbackData)
			}
		}
	}
}

// The guard has to bite for a payload Telegram would reject, rather than
// letting the send fail later.
func TestPayloadRefusesOversizeData(t *testing.T) {
	b, _, _ := newTestBot(t)
	if _, err := b.Payload(authz.ActionAdminViewCustomer, -9223372036854775808, -9223372036854775808); err == nil {
		t.Fatal("oversize payload was accepted")
	}
}

// With a banner configured, /start is one photo message carrying the caption and
// the keyboard — not a photo followed by a separate menu.
func TestStartSendsOnePhotoMessageWithMenu(t *testing.T) {
	b, ft, _ := newTestBot(t)
	b.welcomePath = "assets/welcome.png"

	b.Route(context.Background(), startMessage(7))

	if ft.photos != 1 {
		t.Fatalf("photos sent = %d, want 1", ft.photos)
	}
	if len(ft.sent) != 0 {
		t.Fatalf("a second text message was sent: %v", ft.sent)
	}
	if ft.lastPhoto == nil {
		t.Fatal("no photo captured")
	}
	if !strings.Contains(ft.lastPhoto.Caption, "Menu") {
		t.Fatalf("caption = %q, want the menu text", ft.lastPhoto.Caption)
	}
	markup, ok := ft.lastPhoto.ReplyMarkup.(tgbotapi.InlineKeyboardMarkup)
	if !ok || len(markup.InlineKeyboard) == 0 {
		t.Fatalf("photo carries no keyboard: %T", ft.lastPhoto.ReplyMarkup)
	}
}

// The key is revealed inside the dashboard message: one screen, one place, with a
// single "I saved it" button. No second bubble.
func TestGeneratedKeyRevealsInDashboard(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}

	token, err := codec.Sign("generate_key:7:0")
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingTransport{}
	b.api = rec
	b.Route(ctx, callbackUpdateWithMessage(7, token, 900))

	if len(rec.messages) != 0 {
		t.Fatalf("a separate message was sent: %d", len(rec.messages))
	}
	if len(rec.edits) != 1 {
		t.Fatalf("edits = %d, want the dashboard updated once", len(rec.edits))
	}
	edit := rec.edits[0]
	if edit.MessageID != 900 {
		t.Fatalf("edited message %d, want the dashboard 900", edit.MessageID)
	}
	if !strings.Contains(edit.Text, "`sk-") {
		t.Fatalf("key is not in a code span: %q", edit.Text)
	}

	if edit.ReplyMarkup == nil {
		t.Fatal("no keyboard on the key screen")
	}
	markup := *edit.ReplyMarkup
	if len(markup.InlineKeyboard) != 1 || len(markup.InlineKeyboard[0]) != 1 {
		t.Fatalf("expected exactly one button, got %#v", markup)
	}
	if label := markup.InlineKeyboard[0][0].Text; !strings.Contains(label, "saved") {
		t.Fatalf("the only button should acknowledge the key, got %q", label)
	}
	_ = ft
}

// With a banner configured, /start is one photo message carrying the caption and
// the keyboard — not a photo followed by a separate menu.
