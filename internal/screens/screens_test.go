package screens

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"tgbot/internal/authz"
	"tgbot/internal/ledger"
	"tgbot/internal/store"
)

func findButton(v View, action authz.Action) (Button, bool) {
	for _, row := range v.Rows {
		for _, btn := range row {
			if btn.Action == action {
				return btn, true
			}
		}
	}
	return Button{}, false
}

func TestMenuHidesAdminRowsFromCustomers(t *testing.T) {
	customer := Menu(7, false)
	if _, ok := findButton(customer, authz.ActionAdminListUsers); ok {
		t.Fatal("customer menu contains an admin row")
	}
	if btn, ok := findButton(customer, authz.ActionViewBalance); !ok || btn.Owner != 7 {
		t.Fatalf("balance button = %+v, %v", btn, ok)
	}

	admin := Menu(1, true)
	if _, ok := findButton(admin, authz.ActionAdminListUsers); !ok {
		t.Fatal("admin menu is missing the users row")
	}
}

func TestBalanceShowsDebtWarning(t *testing.T) {
	acct := store.Account{TelegramID: 7, Balance: -ledger.UsecPerMYR}
	v := Balance(acct, 2)
	if !strings.Contains(v.Text, "debt") {
		t.Fatalf("text = %q, want a debt warning", v.Text)
	}
	if btn, ok := findButton(v, authz.ActionClaimTopUp); !ok || btn.Owner != 7 {
		t.Fatalf("reload button = %+v, %v", btn, ok)
	}
}

func TestKeysButtonsCarryKeyID(t *testing.T) {
	v := Keys(7, []store.Key{
		{ID: 11, Name: "Claude Code", Prefix: "sk-aaaaaaa", Active: true},
		{ID: 12, Name: "revoked", Prefix: "sk-bbbbbbb", Active: false},
	})
	btn, ok := findButton(v, authz.ActionRevokeKey)
	if !ok {
		t.Fatal("no revoke button")
	}
	if btn.Arg != 11 || btn.Owner != 7 {
		t.Fatalf("revoke = %+v, want key 11 owned by 7", btn)
	}
	if strings.Contains(v.Text, "revoked") {
		t.Fatalf("inactive key listed: %q", v.Text)
	}
}

func TestUsageRendersBothWindows(t *testing.T) {
	life := store.WindowSummary{Requests: 10, PromptTokens: 100, CompletionTokens: 50, Spend: ledger.UsecPerMYR}
	window := store.WindowSummary{Requests: 2, Spend: ledger.UsecPerMYR / 2}
	v := Usage(7, life, window, nil)
	if !strings.Contains(v.Text, "Lifetime") || !strings.Contains(v.Text, "Last 7 days") {
		t.Fatalf("text = %q", v.Text)
	}
	if !strings.Contains(v.Text, "No keyed usage") {
		t.Fatalf("missing empty-state line: %q", v.Text)
	}
}

func TestUsageBoundsPerKeyLines(t *testing.T) {
	keys := make([]store.KeyActivity, 120)
	for i := range keys {
		keys[i] = store.KeyActivity{Name: "key-" + itoa(int64(i)), Requests: int64(i + 1)}
	}
	v := Usage(7, store.WindowSummary{}, store.WindowSummary{}, keys)
	if len(v.Text) > 4096 {
		t.Fatalf("usage screen is %d chars, Telegram allows 4096", len(v.Text))
	}
	if !strings.Contains(v.Text, "…and 100 more key(s), not shown.") {
		t.Fatalf("wrong truncation footer: %q", v.Text[len(v.Text)-80:])
	}
	if strings.Contains(v.Text, "key-20 —") || !strings.Contains(v.Text, "key-19 —") {
		t.Fatalf("unexpected keys shown at boundary: %q", v.Text[len(v.Text)-300:])
	}

	twoKeys := Usage(7, store.WindowSummary{}, store.WindowSummary{}, keys[:2])
	if strings.Contains(twoKeys.Text, "more key(s)") {
		t.Fatalf("small usage list has a truncation footer: %q", twoKeys.Text)
	}
	if !strings.Contains(twoKeys.Text, "• key-0 — 1 req") || !strings.Contains(twoKeys.Text, "• key-1 — 2 req") {
		t.Fatalf("small usage list changed: %q", twoKeys.Text)
	}
}

func TestAdminCustomerBoundsLedgerEntries(t *testing.T) {
	entries := make([]store.Entry, 200)
	for i := range entries {
		entries[i] = store.Entry{Delta: ledger.UsecPerMYR, BalanceAfter: ledger.UsecPerMYR, Reason: "credit"}
	}
	v := AdminCustomer(store.Account{TelegramID: 7, Balance: ledger.UsecPerMYR}, entries, nil)
	if len(v.Text) >= 4096 {
		t.Fatalf("customer screen is %d chars, Telegram allows 4096", len(v.Text))
	}
	if !strings.Contains(v.Text, "…and 192 older entr(y/ies), not shown.") {
		t.Fatalf("wrong ledger omission footer: %q", v.Text[len(v.Text)-80:])
	}

	small := AdminCustomer(store.Account{TelegramID: 7, Balance: ledger.UsecPerMYR}, entries[:8], nil)
	want := "👤 *Customer* 7\n\nBalance: RM 1.00\n\nRecent ledger\n"
	for range 8 {
		want += "• RM 1.00 credit → RM 1.00\n"
	}
	if small.Text != want {
		t.Fatalf("eight-entry customer screen changed:\n got %q\nwant %q", small.Text, want)
	}
}

func TestAdminTodayBoundsSpenders(t *testing.T) {
	spenders := make([]store.Spender, 200)
	for i := range spenders {
		spenders[i] = store.Spender{TelegramID: int64(i + 1), Spend: ledger.UsecPerMYR}
	}
	sum := store.WindowSummary{Requests: 10, ActiveCustomers: 4, Spend: ledger.UsecPerMYR}
	v := AdminToday(7, sum, spenders)
	if len(v.Text) >= 4096 {
		t.Fatalf("today screen is %d chars, Telegram allows 4096", len(v.Text))
	}
	if !strings.Contains(v.Text, "…and 195 more spender(s), not shown.") {
		t.Fatalf("wrong spender omission footer: %q", v.Text[len(v.Text)-80:])
	}

	small := AdminToday(7, sum, spenders[:5])
	want := "📈 *Today*\n\nrequests 10\ncustomers 4\nspend RM 1.00\n\nTop spenders\n"
	for i := 0; i < 5; i++ {
		want += "• " + itoa(int64(i+1)) + " — RM 1.00\n"
	}
	if small.Text != want {
		t.Fatalf("five-spender today screen changed:\n got %q\nwant %q", small.Text, want)
	}
}

func TestPricesEmptyState(t *testing.T) {
	v := Prices(7, nil)
	if !strings.Contains(v.Text, "No models are priced") {
		t.Fatalf("text = %q", v.Text)
	}
}

// Each Row is one Telegram line, so the layout is two buttons per row: a menu of
// six one-button rows was taller than the message needed to be.
func TestMenuPacksTwoButtonsPerRow(t *testing.T) {
	customer := Menu(7, false)
	if len(customer.Rows) != 3 {
		t.Fatalf("customer menu has %d rows, want 3", len(customer.Rows))
	}
	for i, row := range customer.Rows {
		if len(row) != 2 {
			t.Fatalf("row %d has %d buttons, want 2", i, len(row))
		}
	}

	admin := Menu(7, true)
	if len(admin.Rows) != 5 {
		t.Fatalf("admin menu has %d rows, want 5", len(admin.Rows))
	}
	if last := admin.Rows[len(admin.Rows)-1]; len(last) != 1 {
		t.Fatalf("last admin row has %d buttons, want the odd one out", len(last))
	}

	// Packing must not drop or duplicate a button.
	seen := map[authz.Action]int{}
	for _, row := range admin.Rows {
		for _, b := range row {
			seen[b.Action]++
		}
	}
	for action, n := range seen {
		if n != 1 {
			t.Fatalf("%s appears %d times", action, n)
		}
	}
	if len(seen) != 9 {
		t.Fatalf("admin menu has %d distinct actions, want 9", len(seen))
	}
}

// Telegram rejects a message over 4096 characters, so the customer price list
// must stop listing before that, whatever the catalogue size.
func TestPricesStayUnderTheMessageLimit(t *testing.T) {
	prices := make([]store.ModelPrice, 0, 60)
	for i := 0; i < 60; i++ {
		prices = append(prices, store.ModelPrice{
			Model:                "provider/a-fairly-long-model-name-" + itoa(int64(i)),
			PromptPerMillion:     100_000_000,
			CompletionPerMillion: 200_000_000,
		})
	}
	v := Prices(7, prices)
	if len(v.Text) > 4096 {
		t.Fatalf("price list is %d chars, Telegram allows 4096", len(v.Text))
	}
	if !strings.Contains(v.Text, "and 20 more model(s)") {
		t.Fatalf("truncation was not disclosed: %q", v.Text[len(v.Text)-80:])
	}
}

func TestAdminPricesHubShape(t *testing.T) {
	v := AdminPrices(7, 4, 7, false)
	if len(v.Rows) != 2 || len(v.Rows[0]) != 2 || len(v.Rows[1]) != 2 {
		t.Fatalf("hub rows = %#v", v.Rows)
	}
	if v.Rows[0][0].Text != "✏️ Priced (4)" || v.Rows[0][1].Text != "➕ Unpriced (7)" || v.Rows[1][0].Text != "🔄 Refresh" || v.Rows[1][1].Text != "⬅️ Menu" {
		t.Fatalf("hub labels = %#v", v.Rows)
	}
}
func TestAdminPriceListsShapesAndPaging(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want []int
	}{{3, []int{2, 1}}, {5, []int{2, 2, 1}}, {7, []int{2, 2, 2, 1}}, {8, []int{2, 2, 2, 2}}, {11, []int{2, 2, 2, 2}}} {
		ps := make([]store.ModelPrice, tc.n)
		us := make([]string, tc.n)
		for i := range ps {
			ps[i] = store.ModelPrice{Model: fmt.Sprintf("model-%d", i), PromptPerMillion: 10, CompletionPerMillion: 20}
			us[i] = fmt.Sprintf("up-%d", i)
		}
		for _, v := range []View{AdminPricesPriced(7, ps, 0), AdminPricesUnpriced(7, us, 0)} {
			if len(v.Text) > 4096 {
				t.Fatalf("text too long: %d", len(v.Text))
			}
			for i, n := range tc.want {
				if len(v.Rows[i]) != n {
					t.Fatalf("%d models row %d=%d want %d", tc.n, i, len(v.Rows[i]), n)
				}
			}
		}
		if tc.n == 11 {
			p := AdminPricesPriced(7, ps, 0)
			if p.Rows[4][0].Text != "➡️ Next (3 left)" || !strings.Contains(p.Text, "Page 1 of 2") {
				t.Fatalf("priced first page %#v %q", p.Rows, p.Text)
			}
			p = AdminPricesPriced(7, ps, 1)
			if p.Rows[2][0].Text != "⬅️ Prev" || !strings.Contains(p.Text, "Page 2 of 2") {
				t.Fatalf("priced second page %#v %q", p.Rows, p.Text)
			}
			u := AdminPricesUnpriced(7, us, 8)
			if len(u.Rows[0]) != 2 || u.Rows[2][0].Text != "⬅️ Prev" {
				t.Fatalf("unpriced second page %#v", u.Rows)
			}
		}
	}
	empty := AdminPricesUnpriced(7, nil, 0)
	if len(empty.Rows) != 1 || len(empty.Rows[0]) != 1 || !strings.Contains(empty.Text, "no upstream models") {
		t.Fatalf("empty unpriced = %#v", empty)
	}
}

// itoa keeps the fixtures readable without pulling strconv into every case.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// Telegram's message text limit; every price-list page must stay inside it even
// with absurdly long model names, which is what an upstream catalogue can hold.
const telegramTextLimit = 4096

func TestAdminPriceListsStayInsideTelegramLimits(t *testing.T) {
	longName := strings.Repeat("x", 120)
	priced := make([]store.ModelPrice, 200)
	for i := range priced {
		priced[i] = store.ModelPrice{
			Model:                fmt.Sprintf("riverai/vendor/%s-%03d", longName, i),
			PromptPerMillion:     ledger.Amount(100000000),
			CompletionPerMillion: ledger.Amount(200000000),
		}
	}
	unpriced := make([]string, 200)
	for i := range unpriced {
		unpriced[i] = fmt.Sprintf("riverai/vendor/%s-%03d", longName, i)
	}
	maxButtons := 0
	for page := 0; page <= 200/PricedPageSize; page++ {
		v := AdminPricesPriced(1, priced, page)
		if len(v.Text) > telegramTextLimit {
			t.Fatalf("priced page %d text is %d chars, over %d", page, len(v.Text), telegramTextLimit)
		}
		total := 0
		for _, r := range v.Rows {
			total += len(r)
			if len(r) == 0 {
				t.Fatalf("priced page %d has an empty row", page)
			}
		}
		if total > maxButtons {
			maxButtons = total
		}
	}
	for offset := 0; offset < len(unpriced); offset += UnpricedPageSize {
		v := AdminPricesUnpriced(1, unpriced, offset)
		if len(v.Text) > telegramTextLimit {
			t.Fatalf("unpriced offset %d text is %d chars, over %d", offset, len(v.Text), telegramTextLimit)
		}
		total := 0
		for _, r := range v.Rows {
			total += len(r)
			if len(r) == 0 {
				t.Fatalf("unpriced offset %d has an empty row", offset)
			}
		}
		if total > maxButtons {
			maxButtons = total
		}
	}
	if maxButtons > 12 {
		t.Fatalf("a price-list page carries %d buttons, more than the 8 models plus nav", maxButtons)
	}
	hub := AdminPrices(1, len(priced), len(unpriced), false)
	if got := len(hub.Rows[0]) + len(hub.Rows[1]); got != 4 {
		t.Fatalf("hub carries %d buttons, want 4", got)
	}
}

func TestAdminPricesUnpricedMiddlePagePagesBothWays(t *testing.T) {
	unpriced := make([]string, 24)
	for i := range unpriced {
		unpriced[i] = fmt.Sprintf("m-%d", i)
	}
	mid := AdminPricesUnpriced(1, unpriced, UnpricedPageSize)
	nav := mid.Rows[len(mid.Rows)-2]
	if len(nav) != 2 || nav[0].Text != "⬅️ Prev" || !strings.HasPrefix(nav[1].Text, "➡️ Next") {
		t.Fatalf("middle page nav = %#v, want Prev and Next", nav)
	}
	if nav[0].Arg != 0 || nav[1].Arg != 2*UnpricedPageSize {
		t.Fatalf("middle page nav args = %d,%d", nav[0].Arg, nav[1].Arg)
	}
}

func TestAdminPricesOmitEmptyPagingNoise(t *testing.T) {
	priced := []store.ModelPrice{{Model: "only", PromptPerMillion: ledger.Amount(1), CompletionPerMillion: ledger.Amount(2)}}
	one := AdminPricesPriced(1, priced, 0)
	if strings.Contains(one.Text, "Page 1 of 1") || strings.Contains(one.Text, "not shown") {
		t.Fatalf("single page carries paging noise: %q", one.Text)
	}
	un := AdminPricesUnpriced(1, []string{"solo"}, 0)
	if strings.Contains(un.Text, "Page 1 of 1") || strings.Contains(un.Text, "not shown") {
		t.Fatalf("single unpriced page carries paging noise: %q", un.Text)
	}
	if strings.Contains(un.Text, "solo") {
		t.Fatalf("unpriced model name leaked into the text, which must stay a count: %q", un.Text)
	}
}
