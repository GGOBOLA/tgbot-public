package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/authz"
	"tgbot/internal/ledger"
	"tgbot/internal/screens"
	"tgbot/internal/store"
)

// priceDraftTTL is how long an admin has to type a price after picking a field.
// Long enough to read a statement, short enough that a forgotten prompt cannot
// be completed by an unrelated message an hour later.
const priceDraftTTL = 5 * time.Minute

// draftMode distinguishes editing a price that exists from adding a model the
// upstream serves but nobody has priced yet.
type draftMode int

const (
	// draftEdit changes one half of an existing price.
	draftEdit draftMode = iota
	// draftAdd creates a price for an upstream model, both halves at once.
	draftAdd
)

// priceDraft is one in-progress price edit. It lives in memory on purpose: a
// restart (or the TTL) drops the edit and the admin starts again, which is
// safer than a half-finished value surviving to be applied to a model.
type priceDraft struct {
	mode     draftMode
	model    string
	field    screens.PriceField
	old      ledger.Amount
	value    ledger.Amount
	in, out  ledger.Amount
	hasValue bool
	expires  time.Time
	// chatID and msgID identify the dashboard message the prompt was drawn on,
	// so the confirm screen can replace it instead of starting a second one.
	chatID int64
	msgID  int
}

// priceDrafts holds at most one draft per admin. Telegram delivers updates one
// at a time, but the map is still locked: the bot may be restarted or rebuilt
// while a draft is live.
type priceDrafts struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[int64]priceDraft
}

func newPriceDrafts() *priceDrafts {
	return &priceDrafts{ttl: priceDraftTTL, m: make(map[int64]priceDraft)}
}

func (d *priceDrafts) put(admin int64, dr priceDraft) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dr.expires = time.Now().Add(d.ttl)
	d.m[admin] = dr
}

// get returns a live draft, dropping an expired one.
func (d *priceDrafts) get(admin int64) (priceDraft, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dr, ok := d.m[admin]
	if !ok {
		return priceDraft{}, false
	}
	if time.Now().After(dr.expires) {
		delete(d.m, admin)
		return priceDraft{}, false
	}
	return dr, true
}

func (d *priceDrafts) clear(admin int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.m, admin)
}

// take returns a live draft and removes it, so Apply is single-shot even when
// the admin taps the button twice.
func (d *priceDrafts) take(admin int64) (priceDraft, bool) {
	dr, ok := d.get(admin)
	if ok {
		d.clear(admin)
	}
	return dr, ok
}

func (b *Bot) registerPriceHandlers() {
	b.Handle(authz.ActionAdminPrices, b.onAdminPriceList)
	b.Handle(authz.ActionAdminPricesPriced, b.onAdminPricedList)
	b.Handle(authz.ActionAdminPricesUnpriced, b.onAdminUnpricedList)
	b.Handle(authz.ActionAdminPricesRefresh, b.onAdminPriceRefresh)
	b.Handle(authz.ActionAdminPricesPage, b.onAdminPricePage)
	b.Handle(authz.ActionAdminPricesPricedPage, b.onAdminPricedPricePage)
	b.Handle(authz.ActionAdminPriceModel, b.onAdminPriceModel)
	b.Handle(authz.ActionAdminPriceEdit, b.onAdminPriceField)
	b.Handle(authz.ActionAdminPriceApply, b.onAdminPriceApply)
	b.Handle(authz.ActionAdminPriceAdd, b.onAdminPriceAdd)
	b.Handle(authz.ActionAdminPriceAddApply, b.onAdminPriceAddApply)
}

// onAdminPriceList is the editable list: priced models open their editor, and
// the upstream catalogue contributes the models that have no price yet. The
// callback argument is the page offset into the unpriced list.
func (b *Bot) onAdminPriceList(ctx context.Context, cq *tgbotapi.CallbackQuery, _, _ int64) error {
	return b.showPriceHub(ctx, cq, false)
}

// onAdminPriceRefresh is the same screen with the upstream fetch forced.
func (b *Bot) onAdminPriceRefresh(ctx context.Context, cq *tgbotapi.CallbackQuery, _, _ int64) error {
	return b.showPriceHub(ctx, cq, true)
}

// onAdminPricePage moves to the next page of unpriced models.
func (b *Bot) onAdminPricePage(ctx context.Context, cq *tgbotapi.CallbackQuery, _, arg int64) error {
	return b.showUnpricedList(ctx, cq, int(arg))
}

func (b *Bot) onAdminPricedPricePage(ctx context.Context, cq *tgbotapi.CallbackQuery, _, arg int64) error {
	return b.showPricedList(ctx, cq, int(arg))
}

func (b *Bot) onAdminPricedList(ctx context.Context, cq *tgbotapi.CallbackQuery, _, _ int64) error {
	return b.showPricedList(ctx, cq, 0)
}
func (b *Bot) onAdminUnpricedList(ctx context.Context, cq *tgbotapi.CallbackQuery, _, _ int64) error {
	return b.showUnpricedList(ctx, cq, 0)
}

func (b *Bot) showPriceHub(ctx context.Context, cq *tgbotapi.CallbackQuery, force bool) error {
	prices, err := b.db.Prices(ctx)
	if err != nil {
		return err
	}
	models, upstreamErr := b.catalogue.list(ctx, force)
	extra := unpricedUpstream(models, prices)
	chat, msg := target(cq)
	return b.show(chat, msg, screens.AdminPrices(cq.From.ID, len(prices), len(extra), upstreamErr != nil))
}

func (b *Bot) showPricedList(ctx context.Context, cq *tgbotapi.CallbackQuery, page int) error {
	prices, err := b.db.Prices(ctx)
	if err != nil {
		return err
	}
	chat, msg := target(cq)
	return b.show(chat, msg, screens.AdminPricesPriced(cq.From.ID, prices, page))
}

func (b *Bot) showUnpricedList(ctx context.Context, cq *tgbotapi.CallbackQuery, offset int) error {
	prices, err := b.db.Prices(ctx)
	if err != nil {
		return err
	}
	models, _ := b.catalogue.list(ctx, false)
	chat, msg := target(cq)
	return b.show(chat, msg, screens.AdminPricesUnpriced(cq.From.ID, unpricedUpstream(models, prices), offset))
}

// onAdminPriceAdd starts the both-halves flow for a model the upstream serves
// but nobody has priced. The callback carries a hash of the id, resolved here
// against the live catalogue, so a stale button cannot add the wrong model.
func (b *Bot) onAdminPriceAdd(ctx context.Context, cq *tgbotapi.CallbackQuery, _, arg int64) error {
	model, err := b.resolveModel(ctx, arg)
	if err != nil {
		b.answer(cq.ID, "The upstream list is unavailable — tap Refresh.")
		return nil
	}
	if model == "" {
		b.answer(cq.ID, "That model is no longer offered upstream — tap Refresh.")
		return nil
	}
	chat, msg := target(cq)
	b.drafts.put(cq.From.ID, priceDraft{mode: draftAdd, model: model, chatID: chat, msgID: msg})
	if err := b.show(chat, msg, screens.PriceAddInputPrompt(cq.From.ID, model)); err != nil {
		return err
	}
	b.answer(cq.ID, "Send in/out")
	return nil
}

// onAdminPriceAddApply writes the new model's price, both halves, with the same
// audit row an edit gets.
func (b *Bot) onAdminPriceAddApply(ctx context.Context, cq *tgbotapi.CallbackQuery, _, _ int64) error {
	draft, ok := b.drafts.take(cq.From.ID)
	if !ok || draft.mode != draftAdd || !draft.hasValue {
		b.answer(cq.ID, "That edit expired — reopen the price list.")
		return nil
	}
	if _, err := b.db.SetPriceAudited(ctx, store.ModelPrice{
		Model:                draft.model,
		PromptPerMillion:     draft.in,
		CompletionPerMillion: draft.out,
	}, cq.From.ID); err != nil {
		return err
	}
	b.log.Infof("price added by %d: %s in %s out %s",
		cq.From.ID, draft.model, ledger.FormatMYR(draft.in), ledger.FormatMYR(draft.out))
	return b.showPriceHub(ctx, cq, false)
}

// resolveModel maps a hashed model button back to an id from the current
// catalogue. An empty result means the hash no longer matches anything.
func (b *Bot) resolveModel(ctx context.Context, arg int64) (string, error) {
	models, err := b.catalogue.list(ctx, false)
	if err != nil {
		return "", err
	}
	var found string
	for _, m := range models {
		if screens.ModelArg(m) == arg {
			if found != "" {
				return "", errors.New("ambiguous model hash")
			}
			found = m
		}
	}
	return found, nil
}

func (b *Bot) onAdminPriceModel(ctx context.Context, cq *tgbotapi.CallbackQuery, _, arg int64) error {
	prices, err := b.db.Prices(ctx)
	if err != nil {
		return err
	}
	p, ok := priceByModelArg(prices, arg)
	if !ok {
		b.answer(cq.ID, "That model is no longer priced — reopen the price list.")
		return nil
	}
	chat, msg := target(cq)
	return b.show(chat, msg, screens.AdminPriceEdit(cq.From.ID, p))
}

func priceByModelArg(prices []store.ModelPrice, arg int64) (store.ModelPrice, bool) {
	var found store.ModelPrice
	matched := false
	for _, p := range prices {
		if screens.ModelArg(p.Model) == arg {
			if matched {
				return store.ModelPrice{}, false
			}
			found, matched = p, true
		}
	}
	return found, matched
}

// onAdminPriceField records which half of which model is being edited and asks
// for the number. The model name comes from the database row the index pointed
// at, never from the callback text.
func (b *Bot) onAdminPriceField(ctx context.Context, cq *tgbotapi.CallbackQuery, _, arg int64) error {
	if arg < 0 {
		b.answer(cq.ID, "That model is no longer priced — reopen the price list.")
		return nil
	}
	modelArg, fieldArg := arg/2, arg%2
	if fieldArg > int64(screens.PriceCompletion) {
		b.answer(cq.ID, "That model is no longer priced — reopen the price list.")
		return nil
	}
	field := screens.PriceField(fieldArg)
	prices, err := b.db.Prices(ctx)
	if err != nil {
		return err
	}
	p, ok := priceByModelArg(prices, modelArg)
	if !ok {
		b.answer(cq.ID, "That model is no longer priced — reopen the price list.")
		return nil
	}
	old := p.PromptPerMillion
	if field == screens.PriceCompletion {
		old = p.CompletionPerMillion
	}
	chat, msg := target(cq)
	b.drafts.put(cq.From.ID, priceDraft{
		mode: draftEdit, model: p.Model, field: field, old: old, chatID: chat, msgID: msg,
	})
	// The prompt replaces the dashboard in place: a new text message here is
	// what used to strand the welcome photo one screen behind.
	if err := b.show(chat, msg, screens.PriceInputPrompt(cq.From.ID, p.Model, field, old)); err != nil {
		return err
	}
	b.answer(cq.ID, "Send the new price")
	return nil
}

// onPriceText consumes a plain message only when its author has a live draft.
// Every other message is ignored, exactly as before this feature existed.
func (b *Bot) onPriceText(_ context.Context, msg *tgbotapi.Message) bool {
	draft, ok := b.drafts.get(msg.From.ID)
	if !ok {
		return false
	}
	// A draft belongs to the chat its dashboard lives in. Without this, a
	// number typed in any other chat the admin uses is eaten as a price.
	if draft.chatID != 0 && msg.Chat.ID != draft.chatID {
		return false
	}
	if !b.admins[msg.From.ID] {
		// A draft cannot exist for a non-admin, but if the admin list shrank
		// while one was live, that draft must not be honoured.
		b.drafts.clear(msg.From.ID)
		return false
	}

	if draft.mode == draftAdd {
		in, out, err := parsePricePair(msg.Text)
		if err != nil {
			b.reply(msg.Chat.ID, fmt.Sprintf("%v — send in/out like 1.00/2.00, or /cancel.", err))
			return true
		}
		draft.in, draft.out, draft.hasValue = in, out, true
		b.drafts.put(msg.From.ID, draft)
		if err := b.show(draftChat(draft, msg), draftMsg(draft), screens.AdminPriceAddConfirm(msg.From.ID, draft.model, in, out)); err != nil {
			b.log.Errorf("price confirm for %d: %v", msg.From.ID, err)
		}
		return true
	}

	amount, err := ledger.ParseAmount(msg.Text)
	if err != nil {
		b.reply(msg.Chat.ID, "That is not a price I can read — send a number like 1.25, or /cancel.")
		return true
	}
	if amount == 0 {
		b.reply(msg.Chat.ID, "A price must be greater than zero. Send a number like 1.25, or /cancel.")
		return true
	}
	if amount > ledger.MaxRatePerMillion {
		b.reply(msg.Chat.ID, fmt.Sprintf("That is above the %s per 1M cap. Send a smaller number, or /cancel.",
			ledger.FormatMYR(ledger.MaxRatePerMillion)))
		return true
	}

	draft.value, draft.hasValue = amount, true
	b.drafts.put(msg.From.ID, draft)
	view := screens.AdminPriceConfirm(msg.From.ID, draft.model, draft.field, draft.old, amount)
	if err := b.show(draftChat(draft, msg), draftMsg(draft), view); err != nil {
		b.log.Errorf("price confirm for %d: %v", msg.From.ID, err)
	}
	return true
}

// draftChat is where the draft's screens live: the dashboard message when one
// was recorded, else the chat the admin typed in.
func draftChat(draft priceDraft, msg *tgbotapi.Message) int64 {
	if draft.chatID != 0 {
		return draft.chatID
	}
	return msg.Chat.ID
}

// draftMsg is the dashboard message id, or 0 when the bot must send a new one.
func draftMsg(draft priceDraft) int {
	return draft.msgID
}

// parsePricePair reads "in/out" as two MYR prices per 1M tokens. Both halves
// are required: a model priced on one side only would bill the other at zero.
func parsePricePair(s string) (ledger.Amount, ledger.Amount, error) {
	left, right, ok := strings.Cut(s, "/")
	if !ok {
		return 0, 0, errors.New("both prices are required")
	}
	in, err := ledger.ParseAmount(left)
	if err != nil {
		return 0, 0, fmt.Errorf("input price: %w", err)
	}
	out, err := ledger.ParseAmount(right)
	if err != nil {
		return 0, 0, fmt.Errorf("output price: %w", err)
	}
	if in == 0 || out == 0 {
		return 0, 0, errors.New("both prices must be greater than zero")
	}
	if in > ledger.MaxRatePerMillion || out > ledger.MaxRatePerMillion {
		return 0, 0, fmt.Errorf("a price is above the %s per 1M cap",
			ledger.FormatMYR(ledger.MaxRatePerMillion))
	}
	return in, out, nil
}

// onAdminPriceApply writes the confirmed price and its audit row in one
// transaction, then shows the refreshed list.
func (b *Bot) onAdminPriceApply(ctx context.Context, cq *tgbotapi.CallbackQuery, _, _ int64) error {
	draft, ok := b.drafts.take(cq.From.ID)
	if !ok || !draft.hasValue {
		b.answer(cq.ID, "That edit expired — reopen the price list.")
		return nil
	}

	current, err := b.db.Price(ctx, draft.model)
	if errors.Is(err, store.ErrNotFound) {
		b.answer(cq.ID, "That model is no longer priced.")
		return nil
	}
	if err != nil {
		return err
	}

	next := current
	if draft.field == screens.PriceCompletion {
		next.CompletionPerMillion = draft.value
	} else {
		next.PromptPerMillion = draft.value
	}
	old, err := b.db.SetPriceAudited(ctx, next, cq.From.ID)
	if err != nil {
		return err
	}
	oldValue := old.PromptPerMillion
	if draft.field == screens.PriceCompletion {
		oldValue = old.CompletionPerMillion
	}
	b.log.Infof("price changed by %d: %s %s %s -> %s",
		cq.From.ID, next.Model, draft.field, ledger.FormatMYR(oldValue), ledger.FormatMYR(draft.value))

	if err := b.showPriceHub(ctx, cq, false); err != nil {
		return err
	}
	b.answer(cq.ID, "Saved "+ledger.FormatMYR(draft.value))
	return nil
}

// cancelDraft drops a pending edit. It is reached from /cancel only.
func (b *Bot) cancelDraft(msg *tgbotapi.Message) {
	if _, ok := b.drafts.get(msg.From.ID); !ok {
		return
	}
	b.drafts.clear(msg.From.ID)
	b.reply(msg.Chat.ID, "Cancelled — no price was changed.")
}

// reply sends one plain message; a failed send is logged, never fatal.
func (b *Bot) reply(chatID int64, text string) {
	plain := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(plain); err != nil {
		b.log.Warnf("reply to %d: %v", chatID, err)
	}
}
