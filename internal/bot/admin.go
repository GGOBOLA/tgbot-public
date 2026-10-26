package bot

import (
	"context"
	"errors"
	"fmt"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/authz"
	"tgbot/internal/ledger"
	"tgbot/internal/screens"
	"tgbot/internal/store"
)

// adminPageSize is how many customers one Users page lists.
const adminPageSize = 8

// AdminCreditAmounts are the presets offered on a customer card. Like the
// reload amounts, the value travels signed and is re-checked here.
var AdminCreditAmounts = []ledger.Amount{
	10 * ledger.UsecPerMYR,
	25 * ledger.UsecPerMYR,
	50 * ledger.UsecPerMYR,
	100 * ledger.UsecPerMYR,
}

// isAdminCreditAmount reports whether a is an offered credit amount.
func isAdminCreditAmount(a ledger.Amount) bool {
	for _, want := range AdminCreditAmounts {
		if a == want {
			return true
		}
	}
	return false
}

func (b *Bot) registerAdminHandlers() {
	b.Handle(authz.ActionAdminListUsers, b.onAdminUsers)
	b.Handle(authz.ActionAdminViewCustomer, b.onAdminCustomer)
	b.Handle(authz.ActionAdminCredit, b.onAdminCredit)
	b.Handle(authz.ActionAdminToday, b.onAdminToday)
	b.registerPriceHandlers()
}

func (b *Bot) onAdminUsers(ctx context.Context, cq *tgbotapi.CallbackQuery, _, arg int64) error {
	offset := int(arg)
	if offset < 0 {
		offset = 0
	}
	rows, err := b.db.Customers(ctx, adminPageSize, offset)
	if err != nil {
		return err
	}
	total, err := b.db.CustomerCount(ctx)
	if err != nil {
		return err
	}
	chat, msg := target(cq)
	return b.show(chat, msg, screens.AdminUsers(cq.From.ID, rows, total, offset, adminPageSize))
}

func (b *Bot) onAdminCustomer(ctx context.Context, cq *tgbotapi.CallbackQuery, owner, _ int64) error {
	acct, err := b.db.Account(ctx, owner)
	if errors.Is(err, store.ErrNotFound) {
		b.answer(cq.ID, "That customer no longer exists.")
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := b.db.LedgerEntries(ctx, owner, 8)
	if err != nil {
		return err
	}
	chat, msg := target(cq)
	return b.show(chat, msg, screens.AdminCustomer(acct, entries, AdminCreditAmounts))
}

// onAdminCredit credits a customer. The ledger ref is keyed to the admin, the
// message and the amount, so a double tap on the same button is one credit —
// the unique index, not the button state, is what enforces it.
func (b *Bot) onAdminCredit(ctx context.Context, cq *tgbotapi.CallbackQuery, owner, arg int64) error {
	amount := ledger.Amount(arg)
	if !isAdminCreditAmount(amount) {
		b.answer(cq.ID, "That amount is not offered.")
		return nil
	}

	_, msgID := target(cq)

	// A double tap must not credit twice, but a second deliberate credit from
	// the same dashboard message (this bot reuses one message id for every
	// screen) must not be swallowed either. The guard covers the first; the ref
	// below carries the customer *and* the callback id, so two customers — or
	// two real taps — can never collide on the ledger's unique ref.
	priorBalance, err := b.db.Balance(ctx, owner)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			b.answer(cq.ID, "That customer no longer exists.")
			return nil
		}
		return err
	}
	key := fmt.Sprintf("%d:%d:%d:%d:%d", cq.From.ID, owner, msgID, int64(amount), int64(priorBalance))
	if !b.credits.claim(key) {
		b.answer(cq.ID, "Duplicate press ignored. Press again if this credit was deliberate.")
		return nil
	}
	ref := fmt.Sprintf("admin:%d:%d:%d:%d:%s", cq.From.ID, owner, int64(amount), msgID, cq.ID)

	balance, err := b.db.Credit(ctx, owner, amount, "admin", ref)
	if errors.Is(err, store.ErrNotFound) {
		b.answer(cq.ID, "That customer no longer exists.")
		return nil
	}
	if err != nil {
		return err
	}

	acct := store.Account{TelegramID: owner, Balance: balance}
	chat, msg := target(cq)
	if err := b.show(chat, msg, screens.AdminCreditDone(acct, amount)); err != nil {
		return err
	}

	note := fmt.Sprintf("✅ An admin credited %s to your balance.\nNew balance: %s",
		ledger.FormatMYR(amount), ledger.FormatMYR(balance))
	if _, err := b.api.Send(tgbotapi.NewMessage(owner, note)); err != nil {
		b.log.Warnf("tell customer %d about credit: %v", owner, err)
	}
	b.answer(cq.ID, "Credited "+ledger.FormatMYR(amount))
	return nil
}

func (b *Bot) onAdminToday(ctx context.Context, cq *tgbotapi.CallbackQuery, _, _ int64) error {
	since := time.Now().Add(-24 * time.Hour).Unix()
	sum, err := b.db.SummaryFor(ctx, since)
	if err != nil {
		return err
	}
	top, err := b.db.TopSpenders(ctx, since, 5)
	if err != nil {
		return err
	}
	chat, msg := target(cq)
	return b.show(chat, msg, screens.AdminToday(cq.From.ID, sum, top))
}
