// Command bot runs the customer dashboard Telegram bot.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/bot"
	"tgbot/internal/callback"
	"tgbot/internal/config"
	"tgbot/internal/gateway"
	"tgbot/internal/obs"
	"tgbot/internal/store"
	"tgbot/internal/upstream"
)

func main() {
	if err := run(); err != nil {
		// A signal-driven shutdown is not a failure; otherwise systemd records
		// every restart as a crash, which hides the real ones.
		if errors.Is(err, context.Canceled) {
			return
		}
		log.Fatalf("fatal: %v", err)
	}
}

// runComponents runs every fn concurrently on one shared context and returns
// after all of them have stopped. The first non-nil result cancels the rest and
// is returned: a gateway that cannot bind must take the bot down with it rather
// than leave systemd a live unit with no gateway behind it. Waiting for every
// fn also keeps the store open until both components have stopped, so a charge
// that is still committing is not cut off by the closing DB.
func runComponents(ctx context.Context, fns ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
	)
	for _, fn := range fns {
		wg.Add(1)
		go func(fn func(context.Context) error) {
			defer wg.Done()
			if err := fn(ctx); err != nil {
				once.Do(func() {
					first = err
					cancel()
				})
			}
		}(fn)
	}
	wg.Wait()
	return first
}

// redactingLogOutput wraps w so the standard library's log package scrubs
// credentials. Gateway and cmd/bot log through log.Printf, which has no
// redaction of its own; wrapping the process output once at startup covers
// every one of those call sites without touching any of them.
func redactingLogOutput(w io.Writer) io.Writer { return obs.NewRedactingWriter(w) }

// setupLogging installs the process-wide redacting writer on the standard
// library logger and returns it. It is a function rather than two lines inline
// in run() so the wiring itself is testable: a test can call this, log through
// the global logger, and fail if the SetOutput call disappears. Testing only
// redactingLogOutput leaves that call unpinned, and deleting it would put
// credentials back in the journal with every test still green.
func setupLogging(w io.Writer) io.Writer {
	out := redactingLogOutput(w)
	log.SetOutput(out)
	return out
}

// run wires config, storage and the bot, then blocks until SIGINT/SIGTERM.
func run() error {
	// Before anything can log: from here on every line written through the
	// standard library logger is redacted, including main's log.Fatalf.
	setupLogging(os.Stderr)

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.OpenDB(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			log.Printf("close store: %v", cerr)
		}
	}()

	api, err := tgbotapi.NewBotAPIWithClient(cfg.Token, tgbotapi.APIEndpoint,
		// getUpdates long-polls for 60s, so the client must outlive that; every
		// other call is bounded by the same value, which is why it is not 90s.
		&http.Client{Timeout: 70 * time.Second})
	if err != nil {
		// The library returns the raw *url.Error, whose text contains
		// https://api.telegram.org/bot<TOKEN>/getMe. Never log it: a boot-time
		// DNS or TLS failure would write the bot token to the journal.
		return errors.New("telegram auth failed: check TELEGRAM_BOT_TOKEN and network access")
	}
	log.Printf("authorized as @%s", api.Self.UserName)

	codec, err := callback.New([]byte(cfg.CallbackSecret))
	if err != nil {
		return err
	}
	// The admin price screen lists what the upstream serves, so a model can be
	// priced without editing the file and re-running seedprices. Without a key
	// the upstream would refuse the call, so the catalogue stays off and the
	// screen shows priced models only.
	var lister *upstream.ModelLister
	if cfg.NinerouterKey != "" {
		lister = upstream.NewModelLister(cfg.NinerouterURL, cfg.NinerouterKey, 3*time.Second)
	}
	opts := bot.Options{
		Admins:           cfg.AdminIDs,
		PaymentQRPath:    cfg.PaymentQRPath,
		WelcomeImagePath: cfg.WelcomeImagePath,
	}
	// Assign only when non-nil: a typed nil pointer in an interface is not nil,
	// and the catalogue would then report a dead upstream forever.
	if lister != nil {
		opts.ModelLister = lister
	}
	b, err := bot.New(api, st, codec, obs.New(os.Stderr), opts)
	if err != nil {
		return err
	}
	// One warning per downward crossing, sent off the billing path.
	st.SetLowBalanceHook(b.NotifyLowBalance)

	// Gateway and bot share one process: the gateway bills, the bot shows the bill.
	// Both must stop before the store closes: a charge that is still committing
	// when the DB goes away is served usage that can never be booked.
	fns := make([]func(context.Context) error, 0, 2)

	if cfg.GatewayAddr != "" {
		gw := gateway.New(cfg.GatewayAddr, cfg.NinerouterURL, cfg.NinerouterKey, st,
			cfg.DBPath, cfg.RateLimitPerSec, cfg.RateLimitBurst)
		gw.SetAdminAlert(func(msg string) {
			for id := range cfg.AdminIDs {
				b.SendAdminNotice(context.Background(), id, msg)
			}
		})
		addr, upstream := cfg.GatewayAddr, cfg.NinerouterURL
		fns = append(fns, func(ctx context.Context) error {
			// gw.Run owns its listener and reports no readiness, so the address
			// is tried once here first: a port that is already taken becomes a
			// startup failure instead of a "listening" line for a gateway that
			// never came up. The probe listener is closed immediately; if
			// another process claims the port in the instant before gw.Run
			// binds, that error still stops the process through runComponents.
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return fmt.Errorf("gateway listen %s: %w", addr, err)
			}
			_ = ln.Close()
			log.Printf("gateway listening on %s -> %s", addr, upstream)
			return gw.Run(ctx)
		})
	}

	log.Printf("dashboard up, db=%s", cfg.DBPath)
	fns = append(fns, b.Run)

	return runComponents(ctx, fns...)
}
