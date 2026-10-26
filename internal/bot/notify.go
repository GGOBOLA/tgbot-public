package bot

import (
	"context"
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/ledger"
)

// SendAdminNotice messages one admin. Used by the gateway's disaster alert.
func (b *Bot) SendAdminNotice(ctx context.Context, tgID int64, text string) {
	if _, err := b.api.Send(tgbotapi.NewMessage(tgID, text)); err != nil {
		b.log.Warnf("admin notice to %d: %v", tgID, err)
	}
}

// NotifyLowBalance tells a customer their balance crossed below the warning
// line. It is wired to store.DB.SetLowBalanceHook, which calls it once per
// downward crossing, off the request path.
func (b *Bot) NotifyLowBalance(ctx context.Context, telegramID int64, balance ledger.Amount) {
	text := fmt.Sprintf("⚠️ Your balance is low: %s\nReload to keep your API keys working.", ledger.FormatMYR(balance))
	if _, err := b.api.Send(tgbotapi.NewMessage(telegramID, text)); err != nil {
		b.log.Warnf("low-balance notice to %d: %v", telegramID, err)
	}
}
