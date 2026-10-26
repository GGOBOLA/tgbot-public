// Package bot implements the Telegram transport.
//
// Two rules are structural here rather than repeated in each screen:
//   - every button payload is verified by the callback codec before it is
//     parsed, so a forged or edited payload never reaches a handler;
//   - every action resolves through authz before its handler runs.
package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/authz"
	"tgbot/internal/callback"
	"tgbot/internal/obs"
	"tgbot/internal/screens"
	"tgbot/internal/store"
)

// transport is the slice of *tgbotapi.BotAPI the bot needs, so tests can drive
// routing without a network.
type transport interface {
	Send(tgbotapi.Chattable) (tgbotapi.Message, error)
	Request(tgbotapi.Chattable) (*tgbotapi.APIResponse, error)
}

// Handler runs one authorized callback. owner is the resource owner the button
// was signed for; arg is the optional id it carried.
type Handler func(ctx context.Context, cq *tgbotapi.CallbackQuery, owner, arg int64) error

// Options configures the bot.
type Options struct {
	Admins map[int64]bool
	// PaymentQRPath is the image shown on the reload screen. Empty hides it.
	PaymentQRPath string
	// WelcomeImagePath is posted above the dashboard on /start. Empty skips it.
	WelcomeImagePath string
	// ModelLister reads the upstream model catalogue for the admin price
	// screen. Nil keeps the screen to already-priced models only.
	ModelLister modelLister
}

// Bot owns the update loop and the dispatch rules.
type Bot struct {
	api         transport
	db          *store.DB
	authz       authz.Authorizer
	codec       *callback.Codec
	log         *obs.Logger
	admins      map[int64]bool
	handlers    map[authz.Action]Handler
	qrPath      string
	welcomePath string

	// drafts holds one in-progress price edit per admin; see priceinput.go.
	drafts *priceDrafts
	// catalogue caches the upstream model list; see catalogue.go.
	catalogue *catalogue
	// credits suppresses a double tap on an admin credit; see creditguard.go.
	credits *creditGuard

	// Button loading state for the update being handled; see answerOnce.
	answeredMu  sync.Mutex
	answeredNow bool
}

// New wires a bot. codec may not be nil: unsigned buttons are not supported.
func New(api transport, db *store.DB, codec *callback.Codec, log *obs.Logger, opts Options) (*Bot, error) {
	if api == nil || db == nil {
		return nil, errors.New("bot needs a transport and a store")
	}
	if codec == nil {
		return nil, errors.New("bot needs a callback codec")
	}
	admins := make(map[int64]bool, len(opts.Admins))
	for id, ok := range opts.Admins {
		admins[id] = ok
	}
	if log == nil {
		log = obs.New(nil)
	}
	b := &Bot{
		api:         api,
		db:          db,
		authz:       authz.Rules{},
		codec:       codec,
		log:         log,
		admins:      admins,
		handlers:    make(map[authz.Action]Handler),
		qrPath:      opts.PaymentQRPath,
		welcomePath: opts.WelcomeImagePath,
		drafts:      newPriceDrafts(),
		catalogue:   newCatalogue(opts.ModelLister),
		credits:     newCreditGuard(),
	}
	b.registerDefaultHandlers()
	return b, nil
}

// Handle registers a handler for an authorized action.
func (b *Bot) Handle(action authz.Action, h Handler) {
	b.handlers[action] = h
}

// Run polls Telegram until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	api, ok := b.api.(*tgbotapi.BotAPI)
	if !ok {
		return errors.New("run needs a real BotAPI")
	}
	offset, err := b.updateOffset(ctx)
	if err != nil {
		return fmt.Errorf("read Telegram update offset: %w", err)
	}
	u := tgbotapi.NewUpdate(offset)
	u.Timeout = 30
	updates := api.GetUpdatesChan(u)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case upd, ok := <-updates:
			if !ok {
				return nil
			}
			b.Route(ctx, upd)
		}
	}
}

func (b *Bot) updateOffset(ctx context.Context) (int, error) {
	lastID, err := b.db.LastUpdateID(ctx)
	if err != nil {
		return 0, err
	}
	return lastID + 1, nil
}

// Route dispatches one update.
//
// The dashboard is a private-chat surface: it renders a balance and, once a key
// is minted, the plaintext key itself. Telegram lets a bot be added to a group,
// so a /start there would post both to everyone in it. Non-private updates are
// refused here, before any screen is built.
func (b *Bot) Route(ctx context.Context, upd tgbotapi.Update) {
	if upd.UpdateID > 0 {
		lastID, err := b.db.LastUpdateID(ctx)
		if err != nil {
			b.log.Errorf("read Telegram update id: %v", err)
			return
		}
		if upd.UpdateID <= lastID {
			return
		}
	}
	defer func() {
		if r := recover(); r != nil {
			b.log.Errorf("panic while routing an update: %v", r)
		}
	}()
	b.routeUpdate(ctx, upd)
	if upd.UpdateID > 0 {
		if err := b.db.SetLastUpdateID(ctx, upd.UpdateID); err != nil {
			b.log.Errorf("save Telegram update id: %v", err)
		}
	}
}

func (b *Bot) routeUpdate(ctx context.Context, upd tgbotapi.Update) {
	// One malformed update must cost one update, not the process: the bot and
	// the gateway share it, so a panic here took billing down too.
	if upd.Message != nil && upd.Message.From == nil {
		b.log.Warnf("ignored a message with no sender")
		return
	}
	if upd.CallbackQuery != nil && upd.CallbackQuery.From == nil {
		b.log.Warnf("ignored a callback with no sender")
		return
	}
	if upd.Message != nil && !isPrivateChat(upd.Message.Chat) {
		b.log.Warnf("ignored a message from a %q chat (%d)", upd.Message.Chat.Type, upd.Message.Chat.ID)
		return
	}
	if upd.CallbackQuery != nil && upd.CallbackQuery.Message != nil && !isPrivateChat(upd.CallbackQuery.Message.Chat) {
		b.answer(upd.CallbackQuery.ID, "This dashboard only works in a private chat.")
		return
	}
	switch {
	case upd.CallbackQuery != nil:
		b.routeCallback(ctx, upd.CallbackQuery)
	case upd.Message != nil && upd.Message.IsCommand():
		b.routeCommand(ctx, upd.Message)
	case upd.Message != nil:
		// Plain text is only interesting while an admin is typing a price;
		// onPriceText returns false for everyone else, and the update is
		// dropped exactly as it was before the price editor existed.
		b.onPriceText(ctx, upd.Message)
	}
}

// routeCallback enforces, in order: a valid signature, a parseable payload, an
// authorization decision, and only then a handler.
func (b *Bot) routeCallback(ctx context.Context, cq *tgbotapi.CallbackQuery) {
	// Whatever happens below, the button must stop spinning: every early return
	// and every successful path answers exactly once.
	b.beginCallback()
	defer b.answerQuietly(cq.ID)

	payload, err := b.codec.Verify(cq.Data)
	if err != nil {
		b.answer(cq.ID, "This button expired — reopen the menu.")
		return
	}
	action, owner, arg, ok := parsePayload(payload)
	if !ok {
		b.answer(cq.ID, "Unsupported action.")
		return
	}
	actor := authz.Actor{UserID: cq.From.ID, IsAdmin: b.admins[cq.From.ID]}
	if !b.authz.Can(actor, action, authz.Resource{OwnerID: owner}) {
		b.answer(cq.ID, "You cannot do that.")
		return
	}
	h, ok := b.handlers[action]
	if !ok {
		b.answer(cq.ID, "Unsupported action.")
		return
	}
	if err := h(ctx, cq, owner, arg); err != nil {
		b.log.Errorf("callback %s for %d: %v", action, cq.From.ID, err)
		b.answer(cq.ID, "Something went wrong — try again.")
	}
}

func (b *Bot) routeCommand(ctx context.Context, msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start", "menu":
		if err := b.onMenu(ctx, msg); err != nil {
			b.log.Errorf("menu for %d: %v", msg.From.ID, err)
		}
	case "cancel":
		b.cancelDraft(msg)
	}
}

// answer replies to a callback with a toast, best effort. Telegram rejects a
// second answer for the same callback, so only the first one is sent.
func (b *Bot) answer(callbackID, text string) {
	if b.alreadyAnswered() {
		return
	}
	if _, err := b.api.Request(tgbotapi.NewCallback(callbackID, text)); err != nil {
		b.log.Warnf("answer callback: %v", err)
	}
}

// beginCallback resets the once-per-update answered flag. Updates are handled
// one at a time, so one flag is enough.
func (b *Bot) beginCallback() {
	b.answeredMu.Lock()
	b.answeredNow = false
	b.answeredMu.Unlock()
}

// alreadyAnswered records the answer and reports whether one was already sent.
func (b *Bot) alreadyAnswered() bool {
	b.answeredMu.Lock()
	defer b.answeredMu.Unlock()
	if b.answeredNow {
		return true
	}
	b.answeredNow = true
	return false
}

// answerQuietly dismisses the button's loading state. Telegram spins the button
// until the callback is answered; a screen edit does not reliably stop it, so a
// tap that produces no visible change looked like a blinking button for ~15s.
func (b *Bot) answerQuietly(callbackID string) { b.answer(callbackID, "") }

// MaxCallbackData is Telegram's limit on inline button data. Exceeding it is a
// runtime 400 (BUTTON_DATA_INVALID), so it fails here instead.
const MaxCallbackData = 64

// Payload builds the signed string for a button: action:owner:arg plus tag.
func (b *Bot) Payload(action authz.Action, owner, arg int64) (string, error) {
	data, err := b.codec.Sign(string(action) + ":" +
		strconv.FormatInt(owner, 10) + ":" + strconv.FormatInt(arg, 10))
	if err != nil {
		return "", err
	}
	if len(data) > MaxCallbackData {
		return "", fmt.Errorf("callback data is %d bytes, Telegram allows %d (%s)",
			len(data), MaxCallbackData, action)
	}
	return data, nil
}

// parsePayload splits "action:owner:arg".
func parsePayload(payload string) (authz.Action, int64, int64, bool) {
	parts := strings.Split(payload, ":")
	if len(parts) != 3 || parts[0] == "" {
		return "", 0, 0, false
	}
	owner, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	arg, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	return authz.Action(parts[0]), owner, arg, true
}

// isPrivateChat treats a chat with no type as private: the library leaves Type
// empty in tests and in some synthetic updates, and refusing those would break
// every existing caller for no security gain.
func isPrivateChat(chat *tgbotapi.Chat) bool {
	return chat == nil || chat.Type == "" || chat.Type == "private"
}

// onMenu is the entry point: it renders the dashboard and attaches the signed
// keyboard. Without this the bot answers /start with text and no buttons.
func (b *Bot) onMenu(ctx context.Context, msg *tgbotapi.Message) error {
	b.drafts.clear(msg.From.ID)
	if err := b.db.EnsureCustomer(ctx, msg.From.ID); err != nil {
		return fmt.Errorf("ensure customer: %w", err)
	}
	v := screens.Menu(msg.From.ID, b.admins[msg.From.ID])
	if b.welcomePath != "" {
		if err := b.showPhoto(msg.Chat.ID, b.welcomePath, v); err == nil {
			return nil
		} else {
			// A rejected image must not cost the customer their menu.
			b.log.Warnf("welcome banner: %v", err)
		}
	}
	return b.show(msg.Chat.ID, 0, v)
}
