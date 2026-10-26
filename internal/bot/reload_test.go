package bot

import (
	"context"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// photoCallback builds a button press on a photo message, which is what the
// dashboard is when the welcome banner is configured.
func photoCallback(userID int64, data string, messageID int) tgbotapi.Update {
	u := callbackUpdate(userID, data)
	u.CallbackQuery.Message = &tgbotapi.Message{
		MessageID: messageID,
		Chat:      &tgbotapi.Chat{ID: userID},
		Photo:     []tgbotapi.PhotoSize{{FileID: "banner", Width: 1280, Height: 720}},
	}
	return u
}

// Reload on a photo dashboard swaps the media in place: same message, QR instead
// of the banner, no extra bubble.
func TestReloadSwapsPhotoInPlace(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	b.qrPath = "assets/duitnow.jpg"
	if err := b.db.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}

	token, err := codec.Sign("claim_topup:7:0")
	if err != nil {
		t.Fatal(err)
	}
	b.Route(ctx, photoCallback(7, token, 900))

	if len(ft.mediaEdit) != 1 {
		t.Fatalf("media edits = %d, want 1", len(ft.mediaEdit))
	}
	if ft.mediaEdit[0].MessageID != 900 {
		t.Fatalf("edited message %d, want the dashboard 900", ft.mediaEdit[0].MessageID)
	}
	if ft.photos != 0 {
		t.Fatalf("a new photo message was sent (%d); the QR should replace the banner", ft.photos)
	}
	if ft.deletes != 0 {
		t.Fatalf("the dashboard was deleted (%d)", ft.deletes)
	}
	media, ok := ft.mediaEdit[0].Media.(tgbotapi.InputMediaPhoto)
	if !ok || media.Caption == "" {
		t.Fatalf("media = %#v, want a captioned photo", ft.mediaEdit[0].Media)
	}
}

// A text dashboard cannot take media, so it is removed and the QR becomes the
// dashboard. Still one message in the chat.
func TestReloadOnTextDashboardReplacesMessage(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	b.qrPath = "assets/duitnow.jpg"
	if err := b.db.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}

	token, err := codec.Sign("claim_topup:7:0")
	if err != nil {
		t.Fatal(err)
	}
	b.Route(ctx, callbackUpdateWithMessage(7, token, 900))

	if ft.deletes != 1 {
		t.Fatalf("deletes = %d, want the old dashboard removed", ft.deletes)
	}
	if ft.photos != 1 {
		t.Fatalf("photos = %d, want one QR message", ft.photos)
	}
	if len(ft.mediaEdit) != 0 {
		t.Fatalf("text message cannot take media, but an edit was attempted")
	}
}

// Returning to the menu from the QR screen puts the banner back.
func TestMenuRestoresBannerFromQR(t *testing.T) {
	b, ft, codec := newTestBot(t)
	ctx := context.Background()
	b.welcomePath = "assets/welcome.png"
	if err := b.db.EnsureCustomer(ctx, 7); err != nil {
		t.Fatal(err)
	}

	token, err := codec.Sign("menu:7:0")
	if err != nil {
		t.Fatal(err)
	}
	b.Route(ctx, photoCallback(7, token, 900))

	if len(ft.mediaEdit) != 1 {
		t.Fatalf("media edits = %d, want the banner restored in place", len(ft.mediaEdit))
	}
}
