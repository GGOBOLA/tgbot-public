package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/authz"
	"tgbot/internal/callback"
	"tgbot/internal/ledger"
	"tgbot/internal/screens"
	"tgbot/internal/store"
)

// fakeLister stands in for the upstream /v1/models call.
type fakeLister struct {
	mu     sync.Mutex
	calls  int
	models []string
	err    error
}

func (f *fakeLister) Models(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.models, nil
}

func (f *fakeLister) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func priceListBot(t *testing.T, l *fakeLister) (*Bot, *fakeTransport, *callback.Codec) {
	t.Helper()
	b, ft, codec := newTestBot(t)
	b.catalogue = newCatalogue(l)
	return b, ft, codec
}

func TestAdminPriceListShowsUnpricedUpstreamModels(t *testing.T) {
	l := &fakeLister{models: []string{"riverai/a", "riverai/b", "riverai/priced"}}
	b, ft, codec := priceListBot(t, l)
	ctx := context.Background()
	if err := b.db.SetPrice(ctx, store.ModelPrice{Model: "riverai/priced", PromptPerMillion: 100, CompletionPerMillion: 200}); err != nil {
		t.Fatal(err)
	}

	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_prices:1:0")))
	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_prices_unpriced:1:0")))
	last := ft.sent[len(ft.sent)-1]
	if !strings.Contains(last, "Unpriced models") {
		t.Fatalf("screen does not count the unpriced models: %q", last)
	}
	// The catalogue lives in the keyboard, never in the message body: text that
	// grew with the upstream would hit Telegram's 4096-character limit.
	if strings.Contains(last, "riverai/a") || strings.Contains(last, "riverai/b") {
		t.Fatalf("unpriced model names leaked into the text: %q", last)
	}
	labels := keyboardLabels(t, ft)
	for _, want := range []string{"riverai/a", "riverai/b"} {
		if !containsSubstring(labels, want) {
			t.Fatalf("%q missing from the keyboard: %v", want, labels)
		}
	}

}

func TestAdminPriceListSurvivesUpstreamFailure(t *testing.T) {
	l := &fakeLister{err: errors.New("connection refused")}
	b, ft, codec := priceListBot(t, l)
	ctx := context.Background()
	if err := b.db.SetPrice(ctx, store.ModelPrice{Model: "riverai/priced", PromptPerMillion: 100, CompletionPerMillion: 200}); err != nil {
		t.Fatal(err)
	}

	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_prices:1:0")))
	last := ft.sent[len(ft.sent)-1]
	if !strings.Contains(last, "Upstream catalogue unavailable") {
		t.Fatalf("failure was not reported: %q", last)
	}
	if !strings.Contains(last, "1 priced") {
		t.Fatalf("priced count disappeared with the upstream: %q", last)
	}
}

func TestAdminPriceAddFlow(t *testing.T) {
	l := &fakeLister{models: []string{"riverai/new-model"}}
	b, ft, codec := priceListBot(t, l)
	ctx := context.Background()

	hash := screens.ModelArg("riverai/new-model")
	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_price_add:1:"+itoa(hash))))
	if last := ft.sent[len(ft.sent)-1]; !strings.Contains(last, "in/out") {
		t.Fatalf("prompt = %q", last)
	}
	draft, ok := b.drafts.get(1)
	if !ok || draft.mode != draftAdd || draft.model != "riverai/new-model" {
		t.Fatalf("draft = %+v, ok=%v", draft, ok)
	}

	b.Route(ctx, textUpdate(1, "1.00/2.50"))
	last := ft.sent[len(ft.sent)-1]
	if !strings.Contains(last, "New model price") || !strings.Contains(last, "RM 1.00") || !strings.Contains(last, "RM 2.50") {
		t.Fatalf("confirm screen = %q", last)
	}
	if _, err := b.db.Price(ctx, "riverai/new-model"); err == nil {
		t.Fatal("the model was priced before Apply")
	}

	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_price_add_apply:1:0")))
	p, err := b.db.Price(ctx, "riverai/new-model")
	if err != nil {
		t.Fatal(err)
	}
	if p.PromptPerMillion != ledger.UsecPerMYR || p.CompletionPerMillion != 250_000_000 {
		t.Fatalf("price = %+v", p)
	}
	audit, err := b.db.PriceChanges(ctx, "riverai/new-model")
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 || audit[0].ChangedBy != 1 || audit[0].OldPrompt != 0 {
		t.Fatalf("audit = %+v", audit)
	}
	// After the add the model moved to the priced section.
	last = ft.sent[len(ft.sent)-1]
	if strings.Contains(last, "upstream model(s) with no price") {
		t.Fatalf("the priced model is still offered as unpriced: %q", last)
	}
}

func TestAdminPriceAddRejectsBadPairs(t *testing.T) {
	l := &fakeLister{models: []string{"riverai/new-model"}}
	b, ft, codec := priceListBot(t, l)
	ctx := context.Background()
	hash := screens.ModelArg("riverai/new-model")
	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_price_add:1:"+itoa(hash))))

	for _, bad := range []string{"1.00", "/2.00", "1.00/", "abc/2", "1/2/3", "10001/2.00", "1.00/-2"} {
		b.Route(ctx, textUpdate(1, bad))
		last := ft.sent[len(ft.sent)-1]
		if strings.Contains(last, "New model price") {
			t.Fatalf("%q was accepted: %q", bad, last)
		}
		if _, ok := b.drafts.get(1); !ok {
			t.Fatalf("%q dropped the draft", bad)
		}
	}
	if p, err := b.db.Price(ctx, "riverai/new-model"); err == nil {
		t.Fatalf("a rejected pair still created a price: %+v", p)
	}
}

func TestAdminPriceAddIgnoresUnknownHash(t *testing.T) {
	l := &fakeLister{models: []string{"riverai/a"}}
	b, _, codec := priceListBot(t, l)
	ctx := context.Background()

	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_price_add:1:-123456789")))
	if _, ok := b.drafts.get(1); ok {
		t.Fatal("an unknown model hash started a draft")
	}
}

func TestCatalogueCachesAFailedFetch(t *testing.T) {
	l := &fakeLister{err: errors.New("down")}
	c := newCatalogue(l)
	if _, err := c.list(context.Background(), false); err == nil {
		t.Fatal("expected the first call to fail")
	}
	if _, err := c.list(context.Background(), false); err == nil {
		t.Fatal("expected the cached failure")
	}
	if got := l.callCount(); got != 1 {
		t.Fatalf("upstream was called %d times inside the TTL, want 1", got)
	}
	if _, err := c.list(context.Background(), true); err == nil {
		t.Fatal("expected the forced refresh to fail")
	}
	if got := l.callCount(); got != 2 {
		t.Fatalf("forced refresh did not re-call upstream: %d", got)
	}
}

func TestCatalogueCachesModels(t *testing.T) {
	l := &fakeLister{models: []string{"a", "b"}}
	c := newCatalogue(l)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		models, err := c.list(ctx, false)
		if err != nil || len(models) != 2 {
			t.Fatalf("list = %v, %v", models, err)
		}
	}
	if got := l.callCount(); got != 1 {
		t.Fatalf("upstream called %d times, want 1", got)
	}
}

func TestUnpricedUpstreamExcludesPriced(t *testing.T) {
	priced := []store.ModelPrice{{Model: "b"}}
	got := unpricedUpstream([]string{"c", "a", "b"}, priced)
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("unpriced = %v", got)
	}
	if all := unpricedUpstream(nil, priced); all != nil {
		t.Fatalf("no upstream list produced %v", all)
	}
}

// keyboardLabels flattens every button label of the last sent screen.
func keyboardLabels(t *testing.T, ft *fakeTransport) []string {
	t.Helper()
	if ft.last == nil {
		t.Fatal("no screen was sent")
	}
	kb, ok := ft.last.ReplyMarkup.(tgbotapi.InlineKeyboardMarkup)
	if !ok {
		t.Fatalf("no keyboard: %T", ft.last.ReplyMarkup)
	}
	out := make([]string, 0, len(kb.InlineKeyboard))
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			out = append(out, btn.Text)
		}
	}
	return out
}

func containsSubstring(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// The text must not grow with the upstream catalogue; the keyboard pages.
func TestAdminPriceListPagesManyUnpricedModels(t *testing.T) {
	models := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		models = append(models, fmt.Sprintf("provider/model-%02d", i))
	}
	l := &fakeLister{models: models}
	b, ft, codec := priceListBot(t, l)
	ctx := context.Background()

	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_prices_unpriced:1:0")))
	last := ft.sent[len(ft.sent)-1]
	if len(last) > 700 {
		t.Fatalf("screen text is %d chars for 12 models; it must not grow with the catalogue: %q", len(last), last)
	}
	if strings.Contains(last, "provider/model-") {
		t.Fatalf("model ids leaked into the text: %q", last)
	}
	labels := keyboardLabels(t, ft)
	if got := countPrefixed(labels, "➕ "); got != screens.UnpricedPageSize {
		t.Fatalf("first page has %d unpriced buttons, want %d: %v", got, screens.UnpricedPageSize, labels)
	}
	if !containsSubstring(labels, "Next (4 left)") {
		t.Fatalf("no More button: %v", labels)
	}

	// The second page shows the rest and drops the More button.
	b.Route(ctx, callbackUpdate(1, mustSign(t, codec.Sign, "admin_prices_page:1:8")))
	labels = keyboardLabels(t, ft)
	if got := countPrefixed(labels, "➕ "); got != 4 {
		t.Fatalf("second page has %d unpriced buttons, want 4: %v", got, labels)
	}
	if containsSubstring(labels, "Next") {
		t.Fatalf("second page still offers More: %v", labels)
	}
}

func countPrefixed(labels []string, prefix string) int {
	n := 0
	for _, s := range labels {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

// The hashed model argument must leave the signed payload comfortably inside
// Telegram's 64-byte callback_data limit, even with a 10-digit owner id.
func TestModelArgFitsTheCallbackBudget(t *testing.T) {
	b, _, _ := newTestBot(t)
	long := "provider/a-very-long-model-name-with-many-segments/v4.1-flash-1017"
	payload, err := b.Payload(authz.ActionAdminPriceAdd, 1234567890, screens.ModelArg(long))
	if err != nil {
		t.Fatalf("payload for a long model id: %v", err)
	}
	if len(payload) > MaxCallbackData {
		t.Fatalf("payload is %d bytes, limit %d", len(payload), MaxCallbackData)
	}
	if got := screens.ModelArg(long); got >= 1<<48 {
		t.Fatalf("model arg %d does not fit 48 bits", got)
	}
}
