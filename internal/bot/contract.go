package bot

import (
	"context"
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/ledger"
	"tgbot/internal/screens"
	"tgbot/internal/store"
)

// Telegram's documented limits. Exceeding any of them is a 400 at send time,
// which is how three UI bugs reached production: the code was correct Go and
// still invalid to Telegram.
const (
	// MaxMessageText is the limit for a message body.
	MaxMessageText = 4096
	// MaxCaption is the limit for a photo caption.
	MaxCaption = 1024
	// MaxButtonText is the practical limit for a button label.
	MaxButtonText = 64
	// MaxButtons is the limit on buttons in one keyboard.
	MaxButtons = 100
)

// Screen is one rendered screen together with the chattable the bot would send
// for it, so a caller can validate limits without touching the network.
type Screen struct {
	Name string
	Chat tgbotapi.Chattable
}

// contractUnpriced builds n distinct synthetic upstream model ids, so the paged
// variant of the price screen renders its "More" button.
func contractUnpriced(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("contract/model-%02d", i))
	}
	return out
}

// ContractScreens renders every screen the bot can show for one user, using
// synthetic data. It runs the same build path the handlers use, so a screen that
// Telegram would reject fails here rather than in a customer's chat.
func (b *Bot) ContractScreens(ctx context.Context, userID int64) ([]Screen, error) {
	if err := b.db.EnsureCustomer(ctx, userID); err != nil {
		return nil, err
	}
	if _, err := b.db.Credit(ctx, userID, 10*ledger.UsecPerMYR, "topup", "contract"); err != nil {
		return nil, err
	}
	plain, _, err := b.db.CreateKey(ctx, userID, "contract")
	if err != nil {
		return nil, err
	}
	if err := b.db.SetPrice(ctx, store.ModelPrice{
		Model:                "contract/model",
		PromptPerMillion:     ledger.UsecPerMYR,
		CompletionPerMillion: 2 * ledger.UsecPerMYR,
	}); err != nil {
		return nil, err
	}
	claim, err := b.db.OpenClaim(ctx, userID, 10*ledger.UsecPerMYR)
	if err != nil {
		return nil, err
	}

	acct, err := b.db.Account(ctx, userID)
	if err != nil {
		return nil, err
	}
	prices, err := b.db.Prices(ctx)
	if err != nil {
		return nil, err
	}
	keys, err := b.db.ListKeys(ctx, userID)
	if err != nil {
		return nil, err
	}
	entries, err := b.db.LedgerEntries(ctx, userID, 10)
	if err != nil {
		return nil, err
	}
	life, err := b.db.SummaryFor(ctx, 0)
	if err != nil {
		return nil, err
	}
	week, err := b.db.SummaryFor(ctx, 0)
	if err != nil {
		return nil, err
	}
	byKey, err := b.db.UsageByKey(ctx, userID, 0)
	if err != nil {
		return nil, err
	}
	rows, err := b.db.Customers(ctx, 8, 0)
	if err != nil {
		return nil, err
	}
	total, err := b.db.CustomerCount(ctx)
	if err != nil {
		return nil, err
	}
	top, err := b.db.TopSpenders(ctx, 0, 5)
	if err != nil {
		return nil, err
	}

	views := []struct {
		name string
		view screens.View
	}{
		{"menu.customer", screens.Menu(userID, false)},
		{"menu.admin", screens.Menu(userID, true)},
		{"balance", screens.Balance(acct, len(keys))},
		{"usage", screens.Usage(userID, life, week, byKey)},
		{"keys", screens.Keys(userID, keys)},
		{"prices", screens.Prices(userID, prices)},
		{"reload", screens.Reload(userID, ReloadAmounts)},
		{"claim.sent", screens.ClaimSent(userID, 10*ledger.UsecPerMYR)},
		{"claim.admin", screens.AdminClaim(claim.ID, userID, 10*ledger.UsecPerMYR)},
		{"admin.users", screens.AdminUsers(userID, rows, total, 0, 8)},
		{"admin.customer", screens.AdminCustomer(acct, entries, AdminCreditAmounts)},
		{"admin.today", screens.AdminToday(userID, life, top)},
		{"admin.prices", screens.AdminPrices(userID, len(prices), 1, false)},
		{"admin.prices.priced", screens.AdminPricesPriced(userID, prices, 0)},
		{"admin.prices.unpriced", screens.AdminPricesUnpriced(userID, contractUnpriced(12), 8)},
		{"admin.prices.offline", screens.AdminPrices(userID, len(prices), 0, true)},
		{"admin.price.add", screens.AdminPriceAddConfirm(userID, "contract/extra-model", ledger.UsecPerMYR, 2*ledger.UsecPerMYR)},
		{"admin.price.edit", screens.AdminPriceEdit(userID, prices[0])},
		{"admin.price.confirm", screens.AdminPriceConfirm(userID, prices[0].Model, screens.PricePrompt,
			prices[0].PromptPerMillion, 2*ledger.UsecPerMYR)},
	}

	out := make([]Screen, 0, len(views)+2)
	for _, v := range views {
		chat, err := b.buildMessage(userID, v.view)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", v.name, err)
		}
		out = append(out, Screen{Name: v.name, Chat: chat})
	}

	// The photo variants: the dashboard banner and the payment QR.
	if b.welcomePath != "" {
		photo, err := b.buildPhoto(userID, screens.Menu(userID, true), b.welcomePath)
		if err != nil {
			return nil, fmt.Errorf("menu.photo: %w", err)
		}
		out = append(out, Screen{Name: "menu.photo", Chat: photo})
	}
	if b.qrPath != "" {
		photo, err := b.buildPhoto(userID, screens.Reload(userID, ReloadAmounts), b.qrPath)
		if err != nil {
			return nil, fmt.Errorf("reload.photo: %w", err)
		}
		out = append(out, Screen{Name: "reload.photo", Chat: photo})
	}

	// The key reveal lives in the dashboard itself, so it is checked like any
	// other screen: the key must fit a caption when the dashboard is a photo.
	keyView := screens.APIKeyShown(userID, plain, "API key")
	keyChat, err := b.buildMessage(userID, keyView)
	if err != nil {
		return nil, fmt.Errorf("key.minted: %w", err)
	}
	out = append(out, Screen{Name: "key.minted", Chat: keyChat})

	return out, nil
}
