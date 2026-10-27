package gateway

import (
	"context"
	"fmt"
	"log"
	"time"
)

// Housekeeping intervals and limits.
const (
	maintenanceTick  = time.Hour
	limiterIdleFor   = 10 * time.Minute
	usageKeepMonths  = 12 // usage rows survive a year
	upstreamKeepDays = 30 // upstream error rows are only read day by day
	// Provider-disaster alert: at most one ping per window while retries keep
	// piling up. 10 failed requests in an hour is a real outage, not a blip.
	upstreamAlertThreshold = 10
	upstreamAlertWindow    = time.Hour
)

// maintenance runs the periodic jobs: idle rate-limit buckets, usage rollup and
// daily-bucket pruning. It never blocks request handling and stops with ctx.
func (s *Server) maintenance(ctx context.Context) {
	// One pass at startup: a service restarted more often than the tick would
	// otherwise never prune, and the retention promise would silently stop
	// applying. Alerting is skipped on this pass so a restart cannot page an
	// admin about failures the previous process already reported.
	s.maintenancePass(ctx, false)
	s.probeOfflineModels(ctx)

	ticker := time.NewTicker(maintenanceTick)
	defer ticker.Stop()
	modelTicker := time.NewTicker(modelAvailabilityTick)
	defer modelTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.maintenancePass(ctx, true)
		case <-modelTicker.C:
			s.probeOfflineModels(ctx)
		case <-s.availabilityWake:
			s.probeOfflineModels(ctx)
		}
	}
}

// maintenancePass is one round of the periodic jobs.
func (s *Server) maintenancePass(ctx context.Context, alert bool) {
	if n := s.limiter.Sweep(time.Now(), limiterIdleFor); n > 0 {
		log.Printf("gateway: dropped %d idle rate-limit buckets", n)
	}
	// Expired failure windows are dropped here as well as on access, so a flood
	// from many source addresses cannot leave the map pinned at its cap.
	if n := s.authFails.sweep(time.Now()); n > 0 {
		log.Printf("gateway: dropped %d expired auth-failure windows", n)
	}
	usageCutoff := time.Now().AddDate(0, -usageKeepMonths, 0).Unix()
	if pruned, err := s.db.PruneUsage(ctx, usageCutoff); err != nil {
		log.Printf("gateway: usage prune: %v", err)
	} else if pruned > 0 {
		log.Printf("gateway: pruned %d usage rows older than %d months", pruned, usageKeepMonths)
	}
	upstreamCutoff := time.Now().AddDate(0, 0, -upstreamKeepDays).Unix()
	if pruned, err := s.db.PruneUpstreamErrors(ctx, upstreamCutoff); err != nil {
		log.Printf("gateway: upstream error prune: %v", err)
	} else if pruned > 0 {
		log.Printf("gateway: pruned %d upstream error rows older than %d days", pruned, upstreamKeepDays)
	}
	if alert {
		s.maybeAlertUpstream(ctx, time.Now())
	}
}

// SetAdminAlert registers the sink for provider-disaster pings. main wires it
// with the bot's sender; safe to call before or during Run. The sink is called
// from the alert dispatcher once Run is up, never from a request goroutine.
func (s *Server) SetAdminAlert(fn func(string)) {
	s.alertMu.Lock()
	s.adminAlert = fn
	s.alertMu.Unlock()
}

// maybeAlertUpstream pings the admins once per window while upstream retries
// keep piling up, so an outage is noticed by Telegram instead of by a customer
// complaint. Runs on the maintenance goroutine only; no lock needed.
func (s *Server) maybeAlertUpstream(ctx context.Context, now time.Time) {
	if s.adminAlertSink() == nil {
		return
	}
	if now.Sub(s.upstreamAlertedAt) < upstreamAlertWindow {
		return
	}
	// The lookback is anchored to the wall clock: failures are written with real
	// timestamps, while now only decides when the next alert may fire.
	sum, err := s.db.UpstreamSummary(ctx, time.Now().Add(-upstreamAlertWindow).Unix())
	if err != nil {
		log.Printf("gateway: upstream alert check: %v", err)
		return
	}
	if sum.Failed < upstreamAlertThreshold {
		return
	}
	s.upstreamAlertedAt = now
	label := "connection error"
	if sum.LastStatus > 0 {
		label = fmt.Sprintf("HTTP %d", sum.LastStatus)
	}
	msg := fmt.Sprintf("⚠️ <b>Provider trouble</b>\n\n"+
		"%d request(s) needed upstream retries in the last hour (%d recovered, %d failed for the customer).\n"+
		"Last: %s at %s. The 📈 Today screen has the full picture.",
		sum.Failed, sum.Recovered, sum.GaveUp, label, sum.LastAt)
	s.alert(msg)
	log.Printf("gateway: provider alert sent (%d failures in window)", sum.Failed)
}
