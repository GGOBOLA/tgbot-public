package screens

import (
	"fmt"
	"strings"

	"tgbot/internal/authz"
	"tgbot/internal/ledger"
	"tgbot/internal/store"
)

// Menu is the customer dashboard. Admins get three extra buttons.
// Buttons are laid out two per row so the dashboard stays short; each Row is one
// Telegram line, one Button is one line of its own.
func Menu(owner int64, isAdmin bool) View {
	btns := []Button{
		{Text: "💰 Balance", Action: authz.ActionViewBalance, Owner: owner},
		{Text: "🔄 Reload", Action: authz.ActionClaimTopUp, Owner: owner},
		{Text: "📊 Usage", Action: authz.ActionViewUsage, Owner: owner},
		{Text: "🏷 Prices", Action: authz.ActionViewPrices, Owner: owner},
		{Text: "🔑 New key", Action: authz.ActionGenerateKey, Owner: owner},
		{Text: "🗝 My keys", Action: authz.ActionListKeys, Owner: owner},
	}
	if isAdmin {
		btns = append(btns,
			Button{Text: "👥 Users", Action: authz.ActionAdminListUsers},
			Button{Text: "📈 Today", Action: authz.ActionAdminToday},
			Button{Text: "🏷 Set prices", Action: authz.ActionAdminPrices},
		)
	}

	rows := make([]Row, 0, (len(btns)+1)/2)
	for i := 0; i < len(btns); i += 2 {
		end := i + 2
		if end > len(btns) {
			end = len(btns)
		}
		rows = append(rows, Row(btns[i:end]))
	}
	return View{Text: "🏠 *Menu*\n\nPick a screen:", Rows: rows}
}

// Balance shows the wallet and the reload entry point.
func Balance(acct store.Account, activeKeys int) View {
	text := fmt.Sprintf("💰 *Balance*\n\nWallet: %s\nActive keys: %d/%d",
		ledger.FormatMYR(acct.Balance), activeKeys, store.MaxActiveKeys)
	if acct.Balance < 0 {
		text += "\n\n⚠️ Your balance is in debt. Reload to keep your keys working."
	}
	return View{
		Text: text,
		Rows: []Row{
			{
				{Text: "🔄 Reload", Action: authz.ActionClaimTopUp, Owner: acct.TelegramID},
				{Text: "⬅️ Menu", Action: authz.ActionMenu, Owner: acct.TelegramID},
			},
		},
	}
}

// maxUsageKeyLines bounds the per-key breakdown so the rendered text stays well
// inside Telegram's 4096-character message limit; dropped lines are counted in a
// footer. Deleted keys keep their rows, so this list only ever grows.
const maxUsageKeyLines = 20

// Usage shows lifetime and window totals plus the per-key breakdown.
func Usage(owner int64, life, window store.WindowSummary, byKey []store.KeyActivity) View {
	var b strings.Builder
	fmt.Fprintf(&b, "📊 Usage\n\nLifetime\nrequests %d\ntokens %d in / %d out\nspend %s\n\nLast 7 days\nrequests %d\nspend %s\n",
		life.Requests,
		life.PromptTokens, life.CompletionTokens, ledger.FormatMYR(life.Spend),
		window.Requests, ledger.FormatMYR(window.Spend))

	if len(byKey) > 0 {
		b.WriteString("\nPer key\n")
		shown := byKey
		if len(shown) > maxUsageKeyLines {
			shown = shown[:maxUsageKeyLines]
		}
		for _, k := range shown {
			name := k.Name
			if name == "" {
				name = "(deleted key)"
			}
			fmt.Fprintf(&b, "• %s — %d req, %s\n", name, k.Requests, ledger.FormatMYR(k.Spend))
		}
		if len(byKey) > len(shown) {
			fmt.Fprintf(&b, "\n…and %d more key(s), not shown.\n", len(byKey)-len(shown))
		}
	} else {
		b.WriteString("\nNo keyed usage yet.\n")
	}

	return View{
		// Key names are customer-supplied, so this screen carries no parse mode.
		Text:  b.String(),
		Rows:  []Row{{{Text: "⬅️ Menu", Action: authz.ActionMenu, Owner: owner}}},
		Plain: true,
	}
}

// APIKeyShown is the one-time reveal: the key lives in the dashboard message with
// a single "I saved it" button. Tapping anything else erases it, which is why the
// only button acknowledges the key instead of navigating away.
func APIKeyShown(owner int64, plain, name string) View {
	return View{
		// Markdown stays on: the key sits in a code span so a tap copies it.
		// The customer-supplied name is stripped of the characters that would
		// open an entity, because Markdown v1 cannot escape them.
		Text: fmt.Sprintf("🔑 *New API key*\n\n`%s`\n\n"+
			"Tap the key to copy it. It is shown once — if you tap another\n"+
			"button before saving it, it is gone.\n\nName: %s", plain, safeInline(name)),
		Rows: []Row{{{Text: "✅ I saved it", Action: authz.ActionListKeys, Owner: owner}}},
	}
}

// safeInline drops the characters that open a Markdown entity. Telegram's v1
// dialect has no escape syntax, so this is the only way to put a customer's own
// text inside a formatted message without risking a parse error.
func safeInline(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '`', '[', ']', '*', '_':
			return -1
		}
		return r
	}, s)
}

// Keys lists a customer's keys with a revoke button on each active row.
func Keys(owner int64, keys []store.Key) View {
	var b strings.Builder
	b.WriteString("🗝 My Keys\n")
	rows := make([]Row, 0, len(keys)+2)

	active := 0
	for _, k := range keys {
		if !k.Active {
			continue
		}
		active++
		label := k.Name
		if label == "" {
			label = k.Prefix
		}
		fmt.Fprintf(&b, "\n• %s (%s…)\n", label, k.Prefix)
		rows = append(rows, Row{{
			Text:   shortLabel("🚫 Revoke "+label, maxButtonRunes),
			Action: authz.ActionRevokeKey,
			Owner:  owner,
			Arg:    k.ID,
		}})
	}
	if active == 0 {
		b.WriteString("\nNo active keys yet.\n")
	}
	fmt.Fprintf(&b, "\n%d/%d active\n", active, store.MaxActiveKeys)

	rows = append(rows,
		Row{{Text: "🔑 Generate a key", Action: authz.ActionGenerateKey, Owner: owner}},
		Row{{Text: "⬅️ Menu", Action: authz.ActionMenu, Owner: owner}},
	)
	return View{Text: b.String(), Rows: rows, Plain: true}
}

// maxPriceLines bounds how many models one price screen lists. Each entry is
// roughly 70 characters, and Telegram rejects a message over 4096.
const maxPriceLines = 40

// Prices lists the offered models, up to one message's worth.
func Prices(owner int64, prices []store.ModelPrice) View {
	var b strings.Builder
	b.WriteString("🏷 Price list\n\nper 1M tokens\n")
	if len(prices) == 0 {
		b.WriteString("\nNo models are priced yet.\n")
	}
	shown := prices
	if len(shown) > maxPriceLines {
		shown = shown[:maxPriceLines]
	}
	for _, p := range shown {
		fmt.Fprintf(&b, "\n%s\nin %s · out %s\n",
			p.Model, ledger.FormatMYR(p.PromptPerMillion), ledger.FormatMYR(p.CompletionPerMillion))
	}
	if len(prices) > len(shown) {
		fmt.Fprintf(&b, "\n…and %d more model(s).\n", len(prices)-len(shown))
	}
	return View{
		Text:  b.String(),
		Rows:  []Row{{{Text: "⬅️ Menu", Action: authz.ActionMenu, Owner: owner}}},
		Plain: true,
	}
}

// PriceField is which half of a model's price an edit targets.
type PriceField int

const (
	// PricePrompt is the input price per million tokens.
	PricePrompt PriceField = iota
	// PriceCompletion is the output price per million tokens.
	PriceCompletion
)

// String names the field the way the screens show it.
func (f PriceField) String() string {
	if f == PriceCompletion {
		return "out"
	}
	return "in"
}

// maxButtonRunes keeps a label inside Telegram's 64-character button limit.
const maxButtonRunes = 60

// shortLabel trims a label to fit a button, keeping the tail readable.
func shortLabel(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// UnpricedPageSize bounds how many unpriced upstream models one keyboard shows.
// They are buttons, never message text: the text must stay proportional to what
// the operator sells, not to how many models the upstream happens to serve.
const UnpricedPageSize = 8

// AdminPrices is the editable price list: priced models open their editor,
// upstream models with no price open the "set both prices" flow. Customers
// never see the second section — their list is fed from the prices table only.
// offset pages through the unpriced models; the text only ever names a count.
const PricedPageSize = 8

// ModelButtonsPerRow keeps two model buttons on a row: upstream model names are
// long, and four to a row made every label a truncated stub on a phone.
const ModelButtonsPerRow = 2

// buttonRows groups buttons into rows without padding the final row.
func buttonRows(buttons []Button, perRow int) []Row {
	rows := make([]Row, 0, (len(buttons)+perRow-1)/perRow)
	for i := 0; i < len(buttons); i += perRow {
		end := i + perRow
		if end > len(buttons) {
			end = len(buttons)
		}
		rows = append(rows, Row(buttons[i:end]))
	}
	return rows
}

func AdminPrices(owner int64, pricedCount, unpricedCount int, upstreamErr bool) View {
	text := fmt.Sprintf("🏷 Set prices\n\nper 1M tokens\n\n%d priced · %d upstream without a price", pricedCount, unpricedCount)
	if upstreamErr {
		text += "\n\n⚠️ Upstream catalogue unavailable — showing priced models only."
	}
	return View{Text: text, Plain: true, Rows: []Row{
		{{Text: fmt.Sprintf("✏️ Priced (%d)", pricedCount), Action: authz.ActionAdminPricesPriced, Owner: owner}, {Text: fmt.Sprintf("➕ Unpriced (%d)", unpricedCount), Action: authz.ActionAdminPricesUnpriced, Owner: owner}},
		{{Text: "🔄 Refresh", Action: authz.ActionAdminPricesRefresh, Owner: owner}, {Text: "⬅️ Menu", Action: authz.ActionMenu, Owner: owner}},
	}}
}

func AdminPricesPriced(owner int64, priced []store.ModelPrice, page int) View {
	pages := (len(priced) + PricedPageSize - 1) / PricedPageSize
	if page < 0 || page >= pages {
		page = 0
	}
	start := page * PricedPageSize
	end := start + PricedPageSize
	if end > len(priced) {
		end = len(priced)
	}
	shown := priced[start:end]
	var b strings.Builder
	b.WriteString("🏷 Priced models")
	if pages > 1 {
		fmt.Fprintf(&b, "\nPage %d of %d", page+1, pages)
	}
	b.WriteString("\n")
	for _, p := range shown {
		fmt.Fprintf(&b, "\n%s - in %s · out %s", p.Model, ledger.FormatMYR(p.PromptPerMillion), ledger.FormatMYR(p.CompletionPerMillion))
	}
	if omitted := len(priced) - len(shown); omitted > 0 {
		fmt.Fprintf(&b, "\n\n%d priced model(s) not shown.", omitted)
	}
	buttons := make([]Button, 0, len(shown))
	for _, p := range shown {
		buttons = append(buttons, Button{Text: shortLabel("✏️ "+p.Model, maxButtonRunes), Action: authz.ActionAdminPriceModel, Owner: owner, Arg: ModelArg(p.Model)})
	}
	rows := buttonRows(buttons, ModelButtonsPerRow)
	if page > 0 || end < len(priced) {
		nav := Row{}
		if page > 0 {
			nav = append(nav, Button{Text: "⬅️ Prev", Action: authz.ActionAdminPricesPricedPage, Owner: owner, Arg: int64(page - 1)})
		}
		if end < len(priced) {
			nav = append(nav, Button{Text: fmt.Sprintf("➡️ Next (%d left)", len(priced)-end), Action: authz.ActionAdminPricesPricedPage, Owner: owner, Arg: int64(page + 1)})
		}
		rows = append(rows, nav)
	}
	rows = append(rows, Row{{Text: "⬅️ Back", Action: authz.ActionAdminPrices, Owner: owner}})
	return View{Text: b.String(), Rows: rows, Plain: true}
}

func AdminPricesUnpriced(owner int64, unpriced []string, offset int) View {
	if offset < 0 || offset >= len(unpriced) {
		offset = 0
	}
	end := offset + UnpricedPageSize
	if end > len(unpriced) {
		end = len(unpriced)
	}
	shown := unpriced[offset:end]
	text := "There are no upstream models without a price."
	if len(unpriced) > 0 {
		pages := (len(unpriced) + UnpricedPageSize - 1) / UnpricedPageSize
		var b strings.Builder
		b.WriteString("➕ Unpriced models")
		if pages > 1 {
			fmt.Fprintf(&b, "\nPage %d of %d", offset/UnpricedPageSize+1, pages)
		}
		// The catalogue never reaches the body: a provider serving hundreds of
		// models would push the text past Telegram's limit. Counts only; the
		// buttons carry the names, one page at a time.
		if omitted := len(unpriced) - end; omitted > 0 {
			fmt.Fprintf(&b, "\n\n%d upstream model(s) not shown.", omitted)
		}
		text = b.String()
	}
	buttons := make([]Button, 0, len(shown))
	for _, m := range shown {
		buttons = append(buttons, Button{Text: shortLabel("➕ "+m, maxButtonRunes), Action: authz.ActionAdminPriceAdd, Owner: owner, Arg: ModelArg(m)})
	}
	rows := buttonRows(buttons, ModelButtonsPerRow)
	if len(shown) > 0 && (offset > 0 || end < len(unpriced)) {
		nav := Row{}
		if offset > 0 {
			prev := offset - UnpricedPageSize
			if prev < 0 {
				prev = 0
			}
			nav = append(nav, Button{Text: "⬅️ Prev", Action: authz.ActionAdminPricesPage, Owner: owner, Arg: int64(prev)})
		}
		if end < len(unpriced) {
			nav = append(nav, Button{Text: fmt.Sprintf("➡️ Next (%d left)", len(unpriced)-end), Action: authz.ActionAdminPricesPage, Owner: owner, Arg: int64(end)})
		}
		rows = append(rows, nav)
	}
	rows = append(rows, Row{{Text: "⬅️ Back", Action: authz.ActionAdminPrices, Owner: owner}})
	return View{Text: text, Rows: rows, Plain: true}
}

// PriceInputPrompt asks for one half of an existing price. It is rendered on the
// dashboard message itself, so the welcome photo (and the single-message
// dashboard) survives the edit instead of being replaced by a text message.
func PriceInputPrompt(owner int64, model string, field PriceField, old ledger.Amount) View {
	text := fmt.Sprintf("✏️ %s\n\n%s is %s per 1M tokens.\n\n"+
		"Send the new price in MYR — for example 1, 1.25 or 0.075.\n/cancel to stop.",
		model, field, ledger.FormatMYR(old))
	return View{
		Text:  text,
		Rows:  []Row{{{Text: "⬅️ Cancel", Action: authz.ActionMenu, Owner: owner}}},
		Plain: true,
	}
}

// PriceAddInputPrompt asks for both halves of a new model's price, on the dashboard
// message, for the same reason.
func PriceAddInputPrompt(owner int64, model string) View {
	text := fmt.Sprintf("➕ %s\n\nSend both prices as in/out per 1M tokens, for example 1.00/2.00.\n/cancel to stop.", model)
	return View{
		Text:  text,
		Rows:  []Row{{{Text: "⬅️ Cancel", Action: authz.ActionMenu, Owner: owner}}},
		Plain: true,
	}
}

// AdminPriceAddConfirm shows both halves of a new model's price: a model added
// with one half unset would bill the other side at zero.
func AdminPriceAddConfirm(owner int64, model string, in, out ledger.Amount) View {
	text := fmt.Sprintf("➕ New model price\n\n%s\nin  %s\nout %s\nper 1M tokens\n\n"+
		"Apply adds it to the price list; customers can call it on their next request.",
		model, ledger.FormatMYR(in), ledger.FormatMYR(out))
	return View{
		Text: text,
		Rows: []Row{
			{
				{Text: "✅ Apply", Action: authz.ActionAdminPriceAddApply, Owner: owner},
				{Text: "⬅️ Cancel", Action: authz.ActionMenu, Owner: owner},
			},
		},
		Plain: true,
	}
}

// AdminPriceEdit shows one model and the two halves the admin can change. The
// argument carries the model identity and field together.
func AdminPriceEdit(owner int64, p store.ModelPrice) View {
	text := fmt.Sprintf("✏️ %s\n\nin %s\nout %s\n\nWhich price do you want to change?",
		p.Model, ledger.FormatMYR(p.PromptPerMillion), ledger.FormatMYR(p.CompletionPerMillion))
	return View{
		Text: text,
		Rows: []Row{
			{
				{Text: "✏️ in " + ledger.FormatMYR(p.PromptPerMillion), Action: authz.ActionAdminPriceEdit, Owner: owner, Arg: ModelArg(p.Model)*2 + int64(PricePrompt)},
				{Text: "✏️ out " + ledger.FormatMYR(p.CompletionPerMillion), Action: authz.ActionAdminPriceEdit, Owner: owner, Arg: ModelArg(p.Model)*2 + int64(PriceCompletion)},
			},
			{{Text: "⬅️ Prices", Action: authz.ActionAdminPrices, Owner: owner}},
		},
		Plain: true,
	}
}

// AdminPriceConfirm shows exactly what will change, and that history is not
// rewritten: billed usage rows keep the cost they were charged.
func AdminPriceConfirm(owner int64, model string, field PriceField, old, next ledger.Amount) View {
	text := fmt.Sprintf("💾 Confirm price change\n\n%s\n%s: %s → %s per 1M tokens\n\n"+
		"Every request from now on is billed at the new price. Usage already charged is not recalculated.",
		model, field, ledger.FormatMYR(old), ledger.FormatMYR(next))
	return View{
		Text: text,
		Rows: []Row{
			{
				{Text: "✅ Apply", Action: authz.ActionAdminPriceApply, Owner: owner},
				{Text: "⬅️ Cancel", Action: authz.ActionMenu, Owner: owner},
			},
		},
		Plain: true,
	}
}

// Reload offers the preset top-up amounts. The amount travels in the signed
// button, so a client cannot invent one, and the handler re-checks it against
// this list anyway.
func Reload(owner int64, amounts []ledger.Amount) View {
	rows := make([]Row, 0, len(amounts)+1)
	for _, a := range amounts {
		rows = append(rows, Row{{
			Text:   "I paid " + ledger.FormatMYR(a),
			Action: authz.ActionConfirmClaim,
			Owner:  owner,
			Arg:    int64(a),
		}})
	}
	rows = append(rows, Row{{Text: "⬅️ Menu", Action: authz.ActionMenu, Owner: owner}})
	return View{
		Text: "🔄 *Reload Balance*\n\nScan the QR, pay, then tap the amount you paid.\nAn admin confirms it.",
		Rows: rows,
	}
}

// ClaimSent confirms the claim is waiting for an admin.
func ClaimSent(owner int64, amount ledger.Amount) View {
	return View{
		Text: "⏳ *Claim sent*\n\n" + ledger.FormatMYR(amount) +
			" is waiting for an admin to confirm.\nYou will get a message when it lands.",
		Rows: []Row{{{Text: "⬅️ Menu", Action: authz.ActionMenu, Owner: owner}}},
	}
}

// AdminClaim is the approve/reject prompt an admin receives.
func AdminClaim(claimID, customer int64, amount ledger.Amount) View {
	return View{
		Text: fmt.Sprintf("💳 *Payment claim*\n\nCustomer `%d` says they paid %s.",
			customer, ledger.FormatMYR(amount)),
		Rows: []Row{{
			{Text: "✅ Credit", Action: authz.ActionAdminApproveClaim, Arg: claimID},
			{Text: "❌ Reject", Action: authz.ActionAdminRejectClaim, Arg: claimID},
		}},
	}
}

// AdminUsers lists a page of customers with a card button each.
func AdminUsers(admin int64, rows []store.CustomerRow, total int64, offset, pageSize int) View {
	var b strings.Builder
	fmt.Fprintf(&b, "👥 *Users* — %d total\n", total)
	if len(rows) == 0 {
		b.WriteString("\nNo customers yet.\n")
	}

	out := make([]Row, 0, len(rows)+3)
	for _, c := range rows {
		out = append(out, Row{{
			Text:   fmt.Sprintf("%d — %s", c.TelegramID, ledger.FormatMYR(c.Balance)),
			Action: authz.ActionAdminViewCustomer,
			Owner:  c.TelegramID,
		}})
	}
	if offset > 0 {
		prev := offset - pageSize
		if prev < 0 {
			prev = 0
		}
		out = append(out, Row{{Text: "⬅️ Previous", Action: authz.ActionAdminListUsers, Arg: int64(prev)}})
	}
	if int64(offset+pageSize) < total {
		out = append(out, Row{{Text: "Next ➡️", Action: authz.ActionAdminListUsers, Arg: int64(offset + pageSize)}})
	}
	out = append(out, Row{{Text: "⬅️ Menu", Action: authz.ActionMenu, Owner: admin}})
	return View{Text: b.String(), Rows: out}
}

// AdminCustomer is one customer's card: balance, recent ledger, credit presets.
const maxCustomerEntries = 8

func AdminCustomer(acct store.Account, entries []store.Entry, amounts []ledger.Amount) View {
	var b strings.Builder
	fmt.Fprintf(&b, "👤 *Customer* %d\n\nBalance: %s\n", acct.TelegramID, ledger.FormatMYR(acct.Balance))
	if len(entries) > 0 {
		b.WriteString("\nRecent ledger\n")
		shown := entries
		if len(shown) > maxCustomerEntries {
			shown = shown[:maxCustomerEntries]
		}
		for _, e := range shown {
			fmt.Fprintf(&b, "• %s %s → %s\n",
				ledger.FormatMYR(e.Delta), e.Reason, ledger.FormatMYR(e.BalanceAfter))
		}
		if omitted := len(entries) - len(shown); omitted > 0 {
			fmt.Fprintf(&b, "…and %d older entr(y/ies), not shown.\n", omitted)
		}
	}

	rows := make([]Row, 0, len(amounts)/2+1)
	for i := 0; i < len(amounts); i += 2 {
		row := Row{{
			Text:   "➕ " + ledger.FormatMYR(amounts[i]),
			Action: authz.ActionAdminCredit,
			Owner:  acct.TelegramID,
			Arg:    int64(amounts[i]),
		}}
		if i+1 < len(amounts) {
			row = append(row, Button{
				Text:   "➕ " + ledger.FormatMYR(amounts[i+1]),
				Action: authz.ActionAdminCredit,
				Owner:  acct.TelegramID,
				Arg:    int64(amounts[i+1]),
			})
		}
		rows = append(rows, row)
	}
	rows = append(rows, Row{{Text: "⬅️ Users", Action: authz.ActionAdminListUsers, Arg: 0}})
	return View{Text: b.String(), Rows: rows}
}

// AdminCreditDone confirms a credit and drops the credit buttons, so a stale
// tap cannot credit a second time by accident.
func AdminCreditDone(acct store.Account, amount ledger.Amount) View {
	return View{
		Text: fmt.Sprintf("✅ Credited %s to %d.\nNew balance: %s",
			ledger.FormatMYR(amount), acct.TelegramID, ledger.FormatMYR(acct.Balance)),
		Rows: []Row{{{Text: "⬅️ Users", Action: authz.ActionAdminListUsers, Arg: 0}}},
	}
}

// AdminToday is the platform summary for the last day.
const maxTodaySpenders = 5

func AdminToday(admin int64, sum store.WindowSummary, top []store.Spender) View {
	var b strings.Builder
	fmt.Fprintf(&b, "📈 *Today*\n\nrequests %d\ncustomers %d\nspend %s\n",
		sum.Requests, sum.ActiveCustomers, ledger.FormatMYR(sum.Spend))
	if len(top) > 0 {
		b.WriteString("\nTop spenders\n")
		shown := top
		if len(shown) > maxTodaySpenders {
			shown = shown[:maxTodaySpenders]
		}
		for _, s := range shown {
			fmt.Fprintf(&b, "• %d — %s\n", s.TelegramID, ledger.FormatMYR(s.Spend))
		}
		if omitted := len(top) - len(shown); omitted > 0 {
			fmt.Fprintf(&b, "…and %d more spender(s), not shown.\n", omitted)
		}
	} else {
		b.WriteString("\nNo usage yet today.\n")
	}
	return View{Text: b.String(), Rows: []Row{{{Text: "⬅️ Menu", Action: authz.ActionMenu, Owner: admin}}}}
}
