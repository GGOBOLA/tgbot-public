package gateway

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"tgbot/internal/ledger"
	"tgbot/internal/store"
)

type settlementOutcome struct {
	auth           store.Key
	model          string
	price          store.ModelPrice
	usage          usage
	firstMS        int64
	status         int
	delivered      bool
	hold           ledger.Amount
	promptEstimate int64
	upstreamPads   bool
}

const unbillableAlertWindow = 5 * time.Minute

type settlement struct {
	db                *store.DB
	alert             func(string)
	alertSpend        func(ledger.Amount)
	adminAlertPresent func() bool
	now               func() time.Time

	padWarnedAt atomic.Int64

	unbillableMu         sync.Mutex
	unbillableAlertedAt  time.Time
	unbillableSuppressed int
}

func (s *settlement) settle(ctx context.Context, outcome settlementOutcome) {
	auth := outcome.auth
	model := outcome.model
	price := outcome.price
	u := outcome.usage
	firstMs := outcome.firstMS
	status := outcome.status
	delivered := outcome.delivered
	hold := outcome.hold
	promptEstimate := outcome.promptEstimate
	upstreamPads := outcome.upstreamPads
	if status >= 300 {
		// Provider failures are not served requests, even when an error stream
		// includes token counts. The operator absorbs any upstream cost.
		return
	}
	if !delivered {
		// The upstream answered, but its body could not be relayed (a read error,
		// a broken stream or the size cap), so the customer was told 502 and
		// received no tokens. Charging the hold here billed them for a response
		// they never got, so this call is not billed and gets no usage row: a
		// failed call is not a served request and must not move the dashboard's
		// request totals. Provider failures take the status branch above.
		//
		// The tokens were spent all the same, against our own upstream account,
		// so this is money lost and the admins are told. The model is
		// request-derived and stays out of the log line (gosec G706): the alert
		// carries it instead.
		// Both values are int64 rendered with %d, so nothing here can carry a
		// line break: gosec's taint model flags the amount (it derives from the
		// request size) but log injection needs a string.
		log.Printf("gateway: response not delivered (customer %d), billing nothing: %d µsen not charged", // #nosec G706 -- %d of an int64 cannot inject a line break
			auth.TelegramID, int64(hold))
		s.alertUnbillable(s.now(), auth.TelegramID, model, hold)
		return
	}
	if u.PromptTokens == 0 && u.CompletionTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheCreationTokens == 0 {
		log.Printf("gateway: no usage reported for %s (customer %d)", model, auth.TelegramID)
		// A served call that reported nothing is billed at the pre-flight hold.
		// Charging zero would make missing usage free, and the estimate is the
		// only number we have. The row is marked estimated, which is what keeps
		// the gap visible: reconciliation drops marked rows instead of counting
		// this guess as real drift.
		if hold > 0 {
			if _, err := s.db.ChargeUsage(ctx, store.UsageEntry{
				TelegramID: auth.TelegramID,
				KeyID:      auth.ID,
				Model:      model,
				Cost:       hold,
				FirstMS:    firstMs,
				Estimated:  true,
			}); err != nil {
				log.Printf("gateway: charge estimated usage for %d: %v", auth.TelegramID, err)
			}
			s.alert(fmt.Sprintf("⚠️ <b>Missing usage</b>\n\nCustomer %d, model %s: the upstream reported no tokens. Billed the pre-flight estimate (%d µsen).",
				auth.TelegramID, model, int64(hold)))
			return
		}
		if err := s.db.RecordUsage(ctx, store.UsageEntry{
			TelegramID: auth.TelegramID,
			KeyID:      auth.ID,
			Model:      model,
			Cost:       0,
			FirstMS:    firstMs,
		}); err != nil {
			log.Printf("gateway: record usage: %v", err)
		}
		return
	}
	cacheWrite := u.CacheCreationTokens
	cache1h := u.CacheCreation1hTokens
	if cache1h > cacheWrite {
		cache1h = cacheWrite
	}
	// 9router pads the prompt count of every response it converts (see pad.go).
	// Charging the padded number bills the customer for tokens the model never
	// received, so the pad is removed before it reaches the price or the ledger.
	if upstreamPads {
		billed, padded := billedPromptTokens(u.PromptTokens, promptEstimate)
		if padded {
			log.Printf("gateway: proxy pad removed for %s (customer %d): reported %d prompt tokens, billing %d",
				model, auth.TelegramID, u.PromptTokens, billed)
			u.PromptTokens = billed
		} else if u.PromptTokens >= proxyPromptPad && promptEstimate > 0 {
			// The signature looks padded but the correction refused: the estimate is
			// too far away, which is what a resized or dropped pad looks like. The
			// customer would pay the pad silently, so say so once a minute.
			now := time.Now().Unix()
			if last := s.padWarnedAt.Load(); now-last >= 60 && s.padWarnedAt.CompareAndSwap(last, now) {
				log.Printf("gateway: proxy pad not removed (customer %d): reported %d prompt tokens, local estimate %d",
					auth.TelegramID, u.PromptTokens, promptEstimate)
			}
		}
	}
	if promptEstimate > 0 {
		ceiling := saturatingAdd(saturatingMul(promptEstimate, 4), proxyPromptPad)
		if u.PromptTokens > ceiling {
			reported := u.PromptTokens
			u.PromptTokens = ceiling
			log.Printf("gateway: prompt token count capped (customer %d): reported %d, billing %d", auth.TelegramID, reported, ceiling)
			s.alert(fmt.Sprintf("⚠️ <b>Prompt token count capped</b>\n\nCustomer %d, model %s: upstream reported %d prompt tokens; billing %d.",
				auth.TelegramID, model, reported, ceiling))
		}
	}
	// ledger.Cost saturates instead of wrapping negative, so a hostile or
	// corrupt token count can never credit the customer.
	cost := ledger.Cost(price.Rates(), ledger.Usage{
		Prompt:       u.PromptTokens,
		Completion:   u.CompletionTokens,
		CacheRead:    u.CacheReadTokens,
		CacheWrite5m: cacheWrite - cache1h,
		CacheWrite1h: cache1h,
	})

	// Each field can arrive from an upstream body, including a negative or an
	// absurd one. Adding them plainly wrapped to a negative total, the usage
	// CHECK then rejected the row, and a served call ended up recorded nowhere.
	promptTotal := saturatingAdd(saturatingAdd(clampTokens(u.PromptTokens), clampTokens(u.CacheReadTokens)), clampTokens(u.CacheCreationTokens))
	rec := store.UsageEntry{
		TelegramID: auth.TelegramID,
		KeyID:      auth.ID,
		Model:      model,
		// Cache reads and writes are prompt tokens: store them together so the
		// dashboard's token count is the real prompt size, while Cost prices
		// each class separately.
		PromptTokens:     promptTotal,
		CompletionTokens: clampTokens(u.CompletionTokens),
		Cost:             cost,
		FirstMS:          firstMs,
	}
	// Usage and the debit land in one transaction. The pre-flight check compares
	// against the balance but reserves nothing, so up to maxInflightPerCustomer
	// calls can all pass it before the first charge lands: exposure is a
	// multiple of the balance, not the per-call estimate gap. The ledger row
	// keeps the resulting debt visible.
	if _, cerr := s.db.ChargeUsage(ctx, rec); cerr != nil {
		log.Printf("gateway: charge usage for %d: %v", auth.TelegramID, cerr)
		// The charge context may be the thing that expired. Give the fallback its
		// own budget, detached from the request, so a served call is still
		// recorded when the debit could not commit.
		rctx, rc := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer rc()
		if rerr := s.db.RecordUsage(rctx, rec); rerr != nil {
			log.Printf("gateway: record unbooked usage for %d: %v", auth.TelegramID, rerr)
			s.alert(fmt.Sprintf("🔴 <b>Unbooked usage lost</b>\n\nCustomer %d, model %s, %d µsen: neither the charge nor the usage row could be written.",
				auth.TelegramID, model, cost))
		} else {
			s.alert(fmt.Sprintf("⚠️ <b>Unbooked usage</b>\n\nCustomer %d, model %s, %d µsen could not be charged. It is recorded in usage but not in the ledger.",
				auth.TelegramID, model, cost))
		}
	} else {
		// Only a committed charge counts toward the hourly spike: the alerts
		// above are about money that did not move.
		s.alertSpend(cost)
	}
}

// alertUnbillable throttles notices for upstream work that could not be billed.
func (s *settlement) alertUnbillable(now time.Time, telegramID int64, model string, hold ledger.Amount) {
	if s.adminAlertPresent == nil || !s.adminAlertPresent() {
		return
	}
	s.unbillableMu.Lock()
	if now.Sub(s.unbillableAlertedAt) < unbillableAlertWindow {
		// The next alert reports calls suppressed inside the current window.
		s.unbillableSuppressed++
		s.unbillableMu.Unlock()
		return
	}
	suppressed := s.unbillableSuppressed
	s.unbillableSuppressed = 0
	s.unbillableAlertedAt = now
	s.unbillableMu.Unlock()

	msg := fmt.Sprintf("⚠️ <b>Unbillable call</b>\n\nCustomer %d, model %s: %d µsen not charged — the response never reached the client.",
		telegramID, model, int64(hold))
	if suppressed > 0 {
		msg += fmt.Sprintf("\n\n…and %d more since the last alert.", suppressed)
	}
	s.alert(msg)
}
