package gateway

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"tgbot/internal/ledger"
)

// The threat model's last open code item: "a sudden spike in spend or a burst of
// failed auth is visible in logs but nothing pages anyone". These two watchers
// are deliberately simple in-memory counters, not a metrics system: they answer
// "is something happening right now" for an operator who is not reading logs.

const (
	// authFailWindow is how far back a burst is counted.
	authFailWindow = 10 * time.Minute
	// anomalyAlertCooldown keeps one condition from paging every window while it
	// persists; a repeat after an hour is a reminder, not noise.
	anomalyAlertCooldown = time.Hour

	defaultAuthFailBurst = 50
	defaultSpendAlertMYR = 100.0
)

// anomalyWatcher counts auth failures and spend in fixed windows and reports when
// a threshold is crossed. Zero value is not usable; use newAnomalyWatcher.
type anomalyWatcher struct {
	mu sync.Mutex

	authWindowStart time.Time
	authFails       int
	authLimit       int
	lastAuthAlert   time.Time

	spendWindowStart time.Time
	spend            int64
	spendLimit       int64
	lastSpendAlert   time.Time
}

// newAnomalyWatcher reads the two optional thresholds. SPEND_ALERT_MYR is a
// ringgit amount for one hour of charges; AUTH_ALERT_BURST is how many 401/403
// answers in ten minutes count as a burst.
func newAnomalyWatcher() *anomalyWatcher {
	limit := int64(defaultSpendAlertMYR * float64(ledger.UsecPerMYR))
	if raw := os.Getenv("SPEND_ALERT_MYR"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 {
			limit = int64(v * float64(ledger.UsecPerMYR))
		}
	}
	burst := defaultAuthFailBurst
	if raw := os.Getenv("AUTH_ALERT_BURST"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			burst = v
		}
	}
	return &anomalyWatcher{authLimit: burst, spendLimit: limit}
}

// authFailure records one rejected request and reports the count when this one
// crossed the burst threshold.
func (a *anomalyWatcher) authFailure(now time.Time) (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.authWindowStart.IsZero() || now.Sub(a.authWindowStart) >= authFailWindow {
		a.authWindowStart, a.authFails = now, 0
	}
	a.authFails++
	if a.authFails < a.authLimit {
		return a.authFails, false
	}
	if !a.lastAuthAlert.IsZero() && now.Sub(a.lastAuthAlert) < anomalyAlertCooldown {
		return a.authFails, false
	}
	a.lastAuthAlert = now
	return a.authFails, true
}

// addSpend records one charge and reports the hourly total when it crossed the
// alert line.
func (a *anomalyWatcher) addSpend(now time.Time, cost ledger.Amount) (ledger.Amount, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.spendWindowStart.IsZero() || now.Sub(a.spendWindowStart) >= time.Hour {
		a.spendWindowStart, a.spend = now, 0
	}
	if cost > 0 {
		if int64(cost) > math.MaxInt64-a.spend {
			a.spend = math.MaxInt64
		} else {
			a.spend += int64(cost)
		}
	}
	if a.spend < a.spendLimit {
		return ledger.Amount(a.spend), false
	}
	if !a.lastSpendAlert.IsZero() && now.Sub(a.lastSpendAlert) < anomalyAlertCooldown {
		return ledger.Amount(a.spend), false
	}
	a.lastSpendAlert = now
	return ledger.Amount(a.spend), true
}

// alertAuthBurst sends the burst page, if the watcher says this request crossed
// the line.
func (s *Server) alertAuthBurst() {
	if s.anomalies == nil {
		return
	}
	n, fire := s.anomalies.authFailure(time.Now())
	if !fire {
		return
	}
	s.alert(fmt.Sprintf("🚨 <b>Auth failure burst</b>\n\n%d requests were rejected (401/403) in the last %s. Check for a leaked key or a client hammering a wrong one.",
		n, authFailWindow))
}

// alertSpend records a delivered charge and pages when the hour crossed the line.
func (s *Server) alertSpend(cost ledger.Amount) {
	if s.anomalies == nil {
		return
	}
	total, fire := s.anomalies.addSpend(time.Now(), cost)
	if !fire {
		return
	}
	s.alert(fmt.Sprintf("⚠️ <b>Spend spike</b>\n\n%s charged in the last hour, above the %s alert line.",
		ledger.FormatMYR(total), ledger.FormatMYR(ledger.Amount(s.anomalies.spendLimit))))
}
