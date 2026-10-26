package bot

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/authz"
	"tgbot/internal/callback"
	"tgbot/internal/ledger"
	"tgbot/internal/store"
)

type fakeTransport struct {
	answers  []string
	sent     []string
	edits    []tgbotapi.EditMessageTextConfig
	captions []tgbotapi.EditMessageCaptionConfig
	// errOn, when set, lets a test make Telegram refuse a specific call.
	errOn     func(tgbotapi.Chattable) error
	last      *tgbotapi.MessageConfig // most recent text message
	lastPhoto *tgbotapi.PhotoConfig   // most recent photo message
	photos    int
	mediaEdit []tgbotapi.EditMessageMediaConfig
	deletes   int
}

func (f *fakeTransport) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	if f.errOn != nil {
		if err := f.errOn(c); err != nil {
			return tgbotapi.Message{}, err
		}
	}
	switch m := c.(type) {
	case tgbotapi.MessageConfig:
		f.sent = append(f.sent, m.Text)
		cfg := m
		f.last = &cfg
	case tgbotapi.PhotoConfig:
		f.photos++
		cfg := m
		f.lastPhoto = &cfg
	case tgbotapi.EditMessageMediaConfig:
		f.mediaEdit = append(f.mediaEdit, m)
	case tgbotapi.EditMessageTextConfig:
		f.edits = append(f.edits, m)
	case tgbotapi.EditMessageCaptionConfig:
		f.captions = append(f.captions, m)

	}
	return tgbotapi.Message{}, nil
}

func (f *fakeTransport) Request(c tgbotapi.Chattable) (*tgbotapi.APIResponse, error) {
	switch m := c.(type) {
	case tgbotapi.CallbackConfig:
		f.answers = append(f.answers, m.Text)
	case tgbotapi.DeleteMessageConfig:
		f.deletes++
	}
	return &tgbotapi.APIResponse{Ok: true}, nil
}

func newTestBot(t *testing.T) (*Bot, *fakeTransport, *callback.Codec) {
	t.Helper()
	db, err := store.OpenDB(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	codec, err := callback.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	ft := &fakeTransport{}
	b, err := New(ft, db, codec, nil, Options{Admins: map[int64]bool{1: true}})
	if err != nil {
		t.Fatal(err)
	}
	return b, ft, codec
}

func callbackUpdate(userID int64, data string) tgbotapi.Update {
	return tgbotapi.Update{
		CallbackQuery: &tgbotapi.CallbackQuery{
			ID:   "cb",
			From: &tgbotapi.User{ID: userID},
			Data: data,
		},
	}
}

func TestUnsignedCallbackIsRejected(t *testing.T) {
	b, ft, _ := newTestBot(t)
	called := false
	b.Handle(authz.ActionViewBalance, func(context.Context, *tgbotapi.CallbackQuery, int64, int64) error {
		called = true
		return nil
	})

	b.Route(context.Background(), callbackUpdate(7, "view_balance:7:0"))

	if called {
		t.Fatal("handler ran for an unsigned payload")
	}
	if len(ft.answers) == 0 || !strings.Contains(ft.answers[0], "expired") {
		t.Fatalf("answers = %v, want an expiry notice", ft.answers)
	}
}

func TestForgedSignatureIsRejected(t *testing.T) {
	b, ft, codec := newTestBot(t)
	token, err := codec.Sign("view_balance:7:0")
	if err != nil {
		t.Fatal(err)
	}
	// Flip the last character of the signature.
	last := token[len(token)-1]
	repl := byte('A')
	if last == 'A' {
		repl = 'B'
	}
	forged := token[:len(token)-1] + string(repl)

	called := false
	b.Handle(authz.ActionViewBalance, func(context.Context, *tgbotapi.CallbackQuery, int64, int64) error {
		called = true
		return nil
	})
	b.Route(context.Background(), callbackUpdate(7, forged))

	if called {
		t.Fatal("handler ran for a forged signature")
	}
	if len(ft.answers) == 0 || !strings.Contains(ft.answers[0], "expired") {
		t.Fatalf("answers = %v, want an expiry notice", ft.answers)
	}
}

func TestCustomerCannotUseAdminAction(t *testing.T) {
	b, ft, codec := newTestBot(t)
	token, err := codec.Sign("admin_credit:0:0")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	b.Handle(authz.ActionAdminCredit, func(context.Context, *tgbotapi.CallbackQuery, int64, int64) error {
		called = true
		return nil
	})

	b.Route(context.Background(), callbackUpdate(7, token))

	if called {
		t.Fatal("a customer reached an admin action")
	}
	if len(ft.answers) == 0 || !strings.Contains(ft.answers[0], "cannot") {
		t.Fatalf("answers = %v, want a refusal", ft.answers)
	}
}

func TestAdminReachesAdminAction(t *testing.T) {
	b, _, codec := newTestBot(t)
	token, err := codec.Sign("admin_credit:0:0")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	b.Handle(authz.ActionAdminCredit, func(context.Context, *tgbotapi.CallbackQuery, int64, int64) error {
		called = true
		return nil
	})

	b.Route(context.Background(), callbackUpdate(1, token))
	if !called {
		t.Fatal("an admin could not reach an admin action")
	}
}

func TestCustomerCannotActOnAnotherOwner(t *testing.T) {
	b, _, codec := newTestBot(t)
	token, err := codec.Sign("generate_key:99:0") // owner 99, actor 7
	if err != nil {
		t.Fatal(err)
	}
	called := false
	b.Handle(authz.ActionGenerateKey, func(context.Context, *tgbotapi.CallbackQuery, int64, int64) error {
		called = true
		return nil
	})

	b.Route(context.Background(), callbackUpdate(7, token))
	if called {
		t.Fatal("a customer acted on another owner's resource")
	}
}

func TestOwnResourceIsAllowed(t *testing.T) {
	b, _, codec := newTestBot(t)
	token, err := codec.Sign("generate_key:7:0")
	if err != nil {
		t.Fatal(err)
	}
	gotOwner := int64(0)
	b.Handle(authz.ActionGenerateKey, func(_ context.Context, _ *tgbotapi.CallbackQuery, owner, _ int64) error {
		gotOwner = owner
		return nil
	})

	b.Route(context.Background(), callbackUpdate(7, token))
	if gotOwner != 7 {
		t.Fatalf("handler owner = %d, want 7", gotOwner)
	}
}

func TestUnknownActionIsRefused(t *testing.T) {
	b, ft, codec := newTestBot(t)
	token, err := codec.Sign("not_a_real_action:7:0")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	b.Handle(authz.Action("not_a_real_action"), func(context.Context, *tgbotapi.CallbackQuery, int64, int64) error {
		called = true
		return nil
	})
	b.Route(context.Background(), callbackUpdate(7, token))
	if called {
		t.Fatal("an action nobody allow-listed reached a handler")
	}
	// The allow-list refuses before the handler lookup, so the wording is
	// "You cannot do that" rather than "Unsupported action"; both are refusals.
	if len(ft.answers) == 0 {
		t.Fatal("an unknown action produced no answer at all")
	}
}

func TestPayloadRoundTrip(t *testing.T) {
	b, _, _ := newTestBot(t)
	token, err := b.Payload(authz.ActionViewUsage, 42, 0)
	if err != nil {
		t.Fatal(err)
	}
	action, owner, _, ok := parsePayload(mustVerify(t, b, token))
	if !ok || action != authz.ActionViewUsage || owner != 42 {
		t.Fatalf("payload = %q %d %v", action, owner, ok)
	}
}

func TestUpdateOffsetSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	db, err := store.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetLastUpdateID(context.Background(), 41); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	codec, _ := callback.New([]byte("0123456789abcdef0123456789abcdef"))
	b, err := New(&fakeTransport{}, db, codec, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	offset, err := b.updateOffset(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if offset != 42 {
		t.Fatalf("starting offset = %d, want 42", offset)
	}
}

func TestDuplicateUpdateDoesNotRepeatCreditNoticeOrMintKey(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	if err := b.db.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}
	creditToken, err := codec.Sign("admin_credit:7:" + intString(int64(10*ledger.UsecPerMYR)))
	if err != nil {
		t.Fatal(err)
	}
	credit := callbackUpdate(1, creditToken)
	credit.UpdateID = 100
	credit.CallbackQuery.ID = "credit-callback"
	b.Route(ctx, credit)
	b.Route(ctx, credit)
	creditNotices := 0
	for _, message := range ft.sent {
		if strings.Contains(message, "An admin credited") {
			creditNotices++
		}
	}
	if creditNotices != 1 {
		t.Fatalf("customer credit notices = %d, want 1", creditNotices)
	}

	keyToken, err := codec.Sign("generate_key:7:0")
	if err != nil {
		t.Fatal(err)
	}
	keyUpdate := callbackUpdate(7, keyToken)
	keyUpdate.UpdateID = 101
	b.Route(ctx, keyUpdate)
	b.Route(ctx, keyUpdate)
	keys, err := b.db.ListKeys(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("active keys = %d, want 1", len(keys))
	}
}

func intString(n int64) string { return strconv.FormatInt(n, 10) }

func mustVerify(t *testing.T, b *Bot, token string) string {
	t.Helper()
	payload, err := b.codec.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
