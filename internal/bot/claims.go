package bot

import (
	"context"
	"errors"
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/authz"
	"tgbot/internal/ledger"
	"tgbot/internal/screens"
	"tgbot/internal/store"
)

// ReloadAmounts are the only amounts a claim may be opened for. The signed
// button carries the amount, and the handler re-checks it against this list, so
// neither a forged signature nor a bug in a keyboard can invent RM 9999.
var ReloadAmounts = []ledger.Amount{
	10 * ledger.UsecPerMYR,
	20 * ledger.UsecPerMYR,
	50 * ledger.UsecPerMYR,
}

// IsReloadAmount reports whether a is an offered top-up amount.
func IsReloadAmount(a ledger.Amount) bool {
	for _, want := range ReloadAmounts {
		if a == want {
			return true
		}
	}
	return false
}

func (b *Bot) registerClaimHandlers() {
	b.Handle(authz.ActionClaimTopUp, b.onClaimTopUp)
	b.Handle(authz.ActionConfirmClaim, b.onConfirmClaim)
	b.Handle(authz.ActionAdminApproveClaim, b.onApproveClaim)
	b.Handle(authz.ActionAdminRejectClaim, b.onRejectClaim)
}

// onClaimTopUp turns the dashboard into the QR screen. When the dashboard is
// already a photo the media is swapped in place, so the QR replaces the banner
// in the same message instead of arriving as a second bubble. A text dashboard
// cannot take media, so it is deleted and re-sent as a photo.
func (b *Bot) onClaimTopUp(ctx context.Context, cq *tgbotapi.CallbackQuery, owner, _ int64) error {
	if owner != cq.From.ID {
		return errors.New("reload for a foreign owner")
	}
	chat, msg := target(cq)
	view := screens.Reload(owner, ReloadAmounts)

	if b.qrPath == "" {
		return b.show(chat, msg, view)
	}
	if isPhotoMessage(cq.Message) {
		return b.swapPhoto(chat, msg, b.qrPath, view)
	}
	b.deleteMessage(chat, msg)
	return b.showPhoto(chat, b.qrPath, view)
}

// onConfirmClaim opens one pending claim and asks every admin to decide.
func (b *Bot) onConfirmClaim(ctx context.Context, cq *tgbotapi.CallbackQuery, owner, arg int64) error {
	if owner != cq.From.ID {
		return errors.New("claim for a foreign owner")
	}
	amount := ledger.Amount(arg)
	if !IsReloadAmount(amount) {
		b.answer(cq.ID, "That amount is not offered.")
		return nil
	}

	claim, err := b.db.OpenClaim(ctx, owner, amount)
	switch {
	case errors.Is(err, store.ErrClaimOpen):
		b.answer(cq.ID, "You already have a claim waiting for an admin.")
		return nil
	case errors.Is(err, store.ErrNotFound):
		b.answer(cq.ID, "Start the bot again with /start first.")
		return nil
	case err != nil:
		return err
	}

	for adminID, isAdmin := range b.admins {
		if !isAdmin {
			continue
		}
		if err := b.show(adminID, 0, screens.AdminClaim(claim.ID, owner, amount)); err != nil {
			b.log.Warnf("notify admin %d: %v", adminID, err)
		}
	}

	chat, msg := target(cq)
	return b.show(chat, msg, screens.ClaimSent(owner, amount))
}

// onApproveClaim credits the claim once and tells the customer.
func (b *Bot) onApproveClaim(ctx context.Context, cq *tgbotapi.CallbackQuery, _, claimID int64) error {
	claim, err := b.db.Claim(ctx, claimID)
	if errors.Is(err, store.ErrNotFound) {
		b.answer(cq.ID, "That claim no longer exists.")
		return nil
	}
	if err != nil {
		return err
	}

	balance, err := b.db.ApproveClaim(ctx, claimID, cq.From.ID)
	switch {
	case errors.Is(err, store.ErrClaimSettled):
		b.answer(cq.ID, "That claim was already decided.")
		return nil
	case errors.Is(err, store.ErrClaimExpired):
		b.answer(cq.ID, "That claim has expired.")
		return nil
	case err != nil:
		return err
	}

	text := fmt.Sprintf("✅ Top-up confirmed: %s\nNew balance: %s",
		ledger.FormatMYR(claim.Amount), ledger.FormatMYR(balance))
	if _, err := b.api.Send(tgbotapi.NewMessage(claim.TelegramID, text)); err != nil {
		b.log.Warnf("tell customer %d about credit: %v", claim.TelegramID, err)
	}
	b.answer(cq.ID, "Credited "+ledger.FormatMYR(claim.Amount))
	return nil
}

// onRejectClaim marks the claim rejected and tells the customer.
func (b *Bot) onRejectClaim(ctx context.Context, cq *tgbotapi.CallbackQuery, _, claimID int64) error {
	claim, err := b.db.Claim(ctx, claimID)
	if errors.Is(err, store.ErrNotFound) {
		b.answer(cq.ID, "That claim no longer exists.")
		return nil
	}
	if err != nil {
		return err
	}

	err = b.db.RejectClaim(ctx, claimID, cq.From.ID)
	switch {
	case errors.Is(err, store.ErrClaimSettled):
		b.answer(cq.ID, "That claim was already decided.")
		return nil
	case err != nil:
		return err
	}

	if _, err := b.api.Send(tgbotapi.NewMessage(claim.TelegramID,
		"❌ Your top-up claim was rejected. Nothing was charged. Ask an admin if this is a mistake.")); err != nil {
		b.log.Warnf("tell customer %d about rejection: %v", claim.TelegramID, err)
	}
	b.answer(cq.ID, "Rejected")
	return nil
}
