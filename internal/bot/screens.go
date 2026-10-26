package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/authz"
	"tgbot/internal/screens"
	"tgbot/internal/store"
)

// keyboard signs every button intent; the client never sees a raw action.
func (b *Bot) keyboard(v screens.View) (tgbotapi.InlineKeyboardMarkup, error) {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(v.Rows))
	for _, row := range v.Rows {
		out := make([]tgbotapi.InlineKeyboardButton, 0, len(row))
		for _, btn := range row {
			data, err := b.Payload(btn.Action, btn.Owner, btn.Arg)
			if err != nil {
				return tgbotapi.InlineKeyboardMarkup{}, err
			}
			out = append(out, tgbotapi.NewInlineKeyboardButtonData(btn.Text, data))
		}
		rows = append(rows, out)
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...), nil
}

// show renders a view into a chat, editing the button's message when known.
func (b *Bot) show(chatID int64, messageID int, v screens.View) error {
	kb, err := b.keyboard(v)
	if err != nil {
		return err
	}
	if messageID > 0 {
		edit := tgbotapi.NewEditMessageText(chatID, messageID, v.Text)
		edit.ReplyMarkup = &kb
		// NewEditMessageText sets no parse mode, and Telegram then renders the
		// bold markers literally: every in-place screen edit needs this.
		edit.ParseMode = parseMode(v)
		if _, err = b.api.Send(edit); err == nil || isNotModified(err) {
			return nil
		} else if !isNoTextError(err) {
			return err
		}
		// The dashboard is a photo message (welcome banner): Telegram has no text
		// to replace, so the caption carries the screen. Captions are capped at
		// 1024 characters, and a long price list can exceed that, so a screen
		// that cannot fit becomes its own message instead of failing.
		if len(v.Text) > MaxCaption {
			msg := tgbotapi.NewMessage(chatID, v.Text)
			msg.ParseMode = parseMode(v)
			msg.ReplyMarkup = kb
			_, err = b.api.Send(msg)
			return err
		}
		caption := tgbotapi.NewEditMessageCaption(chatID, messageID, v.Text)
		caption.ParseMode = parseMode(v)
		caption.ReplyMarkup = &kb
		if _, err = b.api.Send(caption); err == nil || isNotModified(err) {
			return nil
		}
		return err
	}
	msg, err := b.buildMessage(chatID, v)
	if err != nil {
		return err
	}
	_, err = b.api.Send(msg)
	return err
}

// buildMessage renders a view into the message the bot would send. Every screen
// goes through here, so the contract checks see exactly what Telegram would.
func (b *Bot) buildMessage(chatID int64, v screens.View) (tgbotapi.MessageConfig, error) {
	kb, err := b.keyboard(v)
	if err != nil {
		return tgbotapi.MessageConfig{}, err
	}
	msg := tgbotapi.NewMessage(chatID, v.Text)
	msg.ParseMode = parseMode(v)
	msg.ReplyMarkup = kb
	return msg, nil
}

// parseMode is Markdown for screens whose text this program fully controls, and
// empty for screens carrying outside data (model ids, customer names).
func parseMode(v screens.View) string {
	if v.Plain {
		return ""
	}
	return tgbotapi.ModeMarkdown
}

// isNotModified reports a no-op edit, which Telegram returns as an error.
func isNotModified(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "message is not modified")
}

// isNoTextError reports whether Telegram refused an edit because the message
// carries no text (a photo or other media message).
func isNoTextError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no text in the message") ||
		strings.Contains(msg, "message caption is not modified")
}

// showPhoto renders a view as one photo message: banner, caption and keyboard
// together, so /start does not land as two separate messages.
func (b *Bot) showPhoto(chatID int64, imagePath string, v screens.View) error {
	photo, err := b.buildPhoto(chatID, v, imagePath)
	if err != nil {
		return err
	}
	_, err = b.api.Send(photo)
	return err
}

// swapPhoto replaces the media of a message that is already a photo, keeping it
// the same message. Telegram cannot attach media to a text message, which is why
// the text-dashboard path deletes and resends instead.
func (b *Bot) swapPhoto(chatID int64, messageID int, imagePath string, v screens.View) error {
	kb, err := b.keyboard(v)
	if err != nil {
		return err
	}
	media := tgbotapi.NewInputMediaPhoto(tgbotapi.FilePath(imagePath))
	media.Caption = v.Text
	media.ParseMode = parseMode(v)
	edit := tgbotapi.EditMessageMediaConfig{
		BaseEdit: tgbotapi.BaseEdit{
			ChatID:      chatID,
			MessageID:   messageID,
			ReplyMarkup: &kb,
		},
		Media: media,
	}
	if _, err = b.api.Send(edit); err == nil || isNotModified(err) {
		return nil
	}
	return err
}

// deleteMessage removes a message, best effort.
func (b *Bot) deleteMessage(chatID int64, messageID int) {
	if messageID <= 0 {
		return
	}
	if _, err := b.api.Request(tgbotapi.NewDeleteMessage(chatID, messageID)); err != nil {
		b.log.Warnf("delete message %d: %v", messageID, err)
	}
}

// isPhotoMessage reports whether the message a button sits on carries a photo.
func isPhotoMessage(msg *tgbotapi.Message) bool { return msg != nil && len(msg.Photo) > 0 }

// buildPhoto renders a view as a single photo message: image, caption, keyboard.
func (b *Bot) buildPhoto(chatID int64, v screens.View, imagePath string) (tgbotapi.PhotoConfig, error) {
	kb, err := b.keyboard(v)
	if err != nil {
		return tgbotapi.PhotoConfig{}, err
	}
	photo := tgbotapi.NewPhoto(chatID, tgbotapi.FilePath(imagePath))
	photo.Caption = v.Text
	photo.ParseMode = parseMode(v)
	photo.ReplyMarkup = kb
	return photo, nil
}

// target is the chat and message a callback should edit.
func target(cq *tgbotapi.CallbackQuery) (chatID int64, messageID int) {
	if cq.Message == nil {
		return cq.From.ID, 0
	}
	return cq.Message.Chat.ID, cq.Message.MessageID
}

func (b *Bot) registerDefaultHandlers() {
	b.Handle(authz.ActionMenu, b.onMenuScreen)
	b.Handle(authz.ActionViewBalance, b.onBalance)
	b.Handle(authz.ActionViewUsage, b.onUsage)
	b.Handle(authz.ActionListKeys, b.onKeys)
	b.Handle(authz.ActionViewPrices, b.onPrices)
	b.Handle(authz.ActionGenerateKey, b.onGenerateKey)
	b.Handle(authz.ActionRevokeKey, b.onRevokeKey)
	b.registerClaimHandlers()
	b.registerAdminHandlers()
}

func (b *Bot) onMenuScreen(ctx context.Context, cq *tgbotapi.CallbackQuery, owner, _ int64) error {
	// Navigating away abandons any half-finished price edit: a stray number
	// typed minutes later must not be read as the price of an old draft.
	b.drafts.clear(cq.From.ID)
	if err := b.db.EnsureCustomer(ctx, cq.From.ID); err != nil {
		return fmt.Errorf("ensure customer: %w", err)
	}
	chat, msg := target(cq)
	view := screens.Menu(cq.From.ID, b.admins[cq.From.ID])
	// Coming back from the QR screen: restore the banner so the dashboard does
	// not keep the payment image on it.
	if isPhotoMessage(cq.Message) && b.welcomePath != "" {
		if err := b.swapPhoto(chat, msg, b.welcomePath, view); err == nil {
			return nil
		}
	}
	return b.show(chat, msg, view)
}

func (b *Bot) onBalance(ctx context.Context, cq *tgbotapi.CallbackQuery, owner, _ int64) error {
	if owner != cq.From.ID && !b.admins[cq.From.ID] {
		return errors.New("balance view for a foreign owner")
	}
	acct, err := b.db.Account(ctx, owner)
	if err != nil {
		return err
	}
	keys, err := b.db.ActiveKeyCount(ctx, owner)
	if err != nil {
		return err
	}
	chat, msg := target(cq)
	return b.show(chat, msg, screens.Balance(acct, int(keys)))
}

func (b *Bot) onUsage(ctx context.Context, cq *tgbotapi.CallbackQuery, owner, _ int64) error {
	req, prompt, completion, spend, err := b.db.UsageTotals(ctx, owner)
	if err != nil {
		return err
	}
	wreq, wprompt, wcompletion, wspend, err := b.db.UsageWindow(ctx, owner, time.Now().AddDate(0, 0, -7).Unix())
	if err != nil {
		return err
	}
	byKey, err := b.db.UsageByKey(ctx, owner, 0)
	if err != nil {
		return err
	}
	life := store.WindowSummary{Requests: req, PromptTokens: prompt, CompletionTokens: completion, Spend: spend}
	window := store.WindowSummary{Requests: wreq, PromptTokens: wprompt, CompletionTokens: wcompletion, Spend: wspend}
	chat, msg := target(cq)
	return b.show(chat, msg, screens.Usage(owner, life, window, byKey))
}

func (b *Bot) onKeys(ctx context.Context, cq *tgbotapi.CallbackQuery, owner, _ int64) error {
	keys, err := b.db.ListKeys(ctx, owner)
	if err != nil {
		return err
	}
	chat, msg := target(cq)
	return b.show(chat, msg, screens.Keys(owner, keys))
}

func (b *Bot) onPrices(ctx context.Context, cq *tgbotapi.CallbackQuery, owner, _ int64) error {
	prices, err := b.db.Prices(ctx)
	if err != nil {
		return err
	}
	chat, msg := target(cq)
	return b.show(chat, msg, screens.Prices(owner, prices))
}

// onGenerateKey mints a key and shows it exactly once.
func (b *Bot) onGenerateKey(ctx context.Context, cq *tgbotapi.CallbackQuery, owner, _ int64) error {
	if owner != cq.From.ID {
		return errors.New("key mint for a foreign owner")
	}
	plain, key, err := b.db.CreateKey(ctx, owner, "API key")
	if err != nil {
		if errors.Is(err, store.ErrKeyCap) {
			b.answer(cq.ID, "You already hold the maximum number of active keys.")
			return nil
		}
		return err
	}
	chat, dashID := target(cq)
	view := screens.APIKeyShown(owner, plain, key.Name)
	if dashID > 0 {
		return b.show(chat, dashID, view)
	}
	return b.show(chat, 0, view)
}

func (b *Bot) onRevokeKey(ctx context.Context, cq *tgbotapi.CallbackQuery, owner, arg int64) error {
	if owner != cq.From.ID {
		return errors.New("revoke for a foreign owner")
	}
	if err := b.db.RevokeKey(ctx, owner, arg); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			b.answer(cq.ID, "That key is already revoked.")
			return nil
		}
		return err
	}
	return b.onKeys(ctx, cq, owner, 0)
}
