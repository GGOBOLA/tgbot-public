// Package config loads and validates runtime settings from the environment.
package config

import (
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register JPEG for the config-time image check
	_ "image/png"  // register PNG for the config-time image check
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"tgbot/internal/callback"
)

// Config is the fully validated bot configuration.
type Config struct {
	Token            string         // Telegram bot token
	DBPath           string         // SQLite file path
	AdminIDs         map[int64]bool // Telegram IDs allowed to /topup
	CallbackSecret   string         // HMAC key for signed button payloads
	NinerouterURL    string         // 9router base URL (gateway + price import); required with a key
	PaymentQRPath    string         // DuitNow QR image shown on Reload Balance
	WelcomeImagePath string         // brand image posted above the dashboard on every /start
	NinerouterKey    string         // internal 9router API key (gateway only)
	GatewayAddr      string         // gateway listen address; empty = disabled

	// Both are per customer, not per API key: the gateway's limiter buckets on
	// auth.TelegramID, so every key a customer holds shares one budget.
	RateLimitPerSec float64 // sustained requests per second
	RateLimitBurst  float64 // how many may arrive back-to-back
}

// Load reads the environment and fails fast on bad input.
func Load() (*Config, error) {
	cfg := &Config{
		Token:         strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		DBPath:        strings.TrimSpace(os.Getenv("DB_PATH")),
		NinerouterURL: strings.TrimRight(strings.TrimSpace(os.Getenv("NINEROUTER_URL")), "/"),
	}
	if cfg.Token == "" {
		return nil, errors.New("TELEGRAM_BOT_TOKEN is required")
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "./data/bot.db"
	}

	admins, err := parseAdminIDs(os.Getenv("ADMIN_IDS"))
	if err != nil {
		return nil, err
	}
	// Without an admin nobody can approve a DuitNow claim, so the bot would take
	// customers' money and never credit it. Refuse to start instead.
	if len(admins) == 0 {
		return nil, errors.New("ADMIN_IDS must list at least one Telegram user ID")
	}
	cfg.AdminIDs = admins

	// Every button payload is HMAC-signed with this key; without it a client
	// could forge an amount or an action id. The floor is the codec's own
	// MinSecret so boot and signing cannot disagree; 32 hex characters stays
	// the recommendation, not the gate.
	cfg.CallbackSecret = strings.TrimSpace(os.Getenv("CALLBACK_SECRET"))
	if len(cfg.CallbackSecret) < callback.MinSecret {
		return nil, fmt.Errorf("CALLBACK_SECRET must be at least %d characters (32 recommended: openssl rand -hex 32)", callback.MinSecret)
	}

	// Optional: without a QR the bot still takes claims, it just tells the
	// customer to pay the admin directly. If given, fail fast at boot.
	cfg.PaymentQRPath = strings.TrimSpace(os.Getenv("PAYMENT_QR_PATH"))
	if cfg.PaymentQRPath != "" {
		if err := checkReadableFile("PAYMENT_QR_PATH", cfg.PaymentQRPath); err != nil {
			return nil, err
		}
	}
	cfg.WelcomeImagePath = strings.TrimSpace(os.Getenv("WELCOME_IMAGE_PATH"))
	if cfg.WelcomeImagePath != "" {
		if err := checkReadableFile("WELCOME_IMAGE_PATH", cfg.WelcomeImagePath); err != nil {
			return nil, err
		}
	}
	// Gateway is opt-in: set GATEWAY_ADDR to listen. Without a 9router key it cannot
	// forward anything, so that combination is refused at boot.
	cfg.GatewayAddr = strings.TrimSpace(os.Getenv("GATEWAY_ADDR"))
	cfg.NinerouterKey = strings.TrimSpace(os.Getenv("NINEROUTER_KEY"))
	if cfg.GatewayAddr != "" {
		if err := validateListenAddr(cfg.GatewayAddr); err != nil {
			return nil, err
		}
		if cfg.NinerouterKey == "" {
			return nil, errors.New("GATEWAY_ADDR is set but NINEROUTER_KEY is empty")
		}
	}
	// No implicit upstream. A key without a URL used to point every prompt and
	// the operator's bearer at the vendor cloud silently: one removed line in an
	// .env was enough to send customer traffic off the host. Naming the URL is
	// now required, so the destination is always a decision someone wrote down.
	if cfg.NinerouterURL == "" && cfg.NinerouterKey != "" {
		return nil, errors.New("NINEROUTER_KEY is set but NINEROUTER_URL is empty: name the upstream explicitly (for the hosted service use https://9router.com)")
	}

	// Rate limit defaults: 20 requests back-to-back, 5/s sustained per customer.
	// The gateway buckets on auth.TelegramID, so a customer's keys share them.
	cfg.RateLimitPerSec, err = parseFloatDefault("RATE_LIMIT_PER_SEC", 5)
	if err != nil {
		return nil, err
	}
	cfg.RateLimitBurst, err = parseFloatDefault("RATE_LIMIT_BURST", 20)
	if err != nil {
		return nil, err
	}
	// A rate of 1e-12 makes time.Duration overflow into a negative Retry-After,
	// and a burst of 1e308 makes the bucket never empty. Bound both.
	if cfg.RateLimitPerSec < 0.1 || cfg.RateLimitPerSec > 1000 {
		return nil, fmt.Errorf("RATE_LIMIT_PER_SEC must be between 0.1 and 1000, got %v", cfg.RateLimitPerSec)
	}
	if cfg.RateLimitBurst < 1 || cfg.RateLimitBurst > 10000 {
		return nil, fmt.Errorf("RATE_LIMIT_BURST must be between 1 and 10000, got %v", cfg.RateLimitBurst)
	}

	// Only the gateway and the admin price import call 9router, so the URL is
	// optional in a bot-only deployment, but it must be sane when present.
	if cfg.NinerouterURL != "" {
		if err := validateHTTPURL("NINEROUTER_URL", cfg.NinerouterURL); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// validateHTTPURL checks scheme, host and port without echoing the value. A
// malformed URL can still carry credentials in its text, and this message goes
// to the journal through log.Fatalf.
func validateHTTPURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s is not a valid URL", name)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s scheme must be http or https", name)
	}
	// Plain http over a network puts the upstream bearer key and every customer
	// prompt on the wire; loopback is the one place it cannot.
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("%s must use https unless it points at loopback", name)
	}
	// Credentials or a query would be logged by an error and would break the
	// forwarded path (s.upstream + "/v1/...").
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must not contain credentials, a query or a fragment", name)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%s has an invalid port", name)
		}
	}
	return nil
}

// isLoopbackHost reports whether host names this machine.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseFloatDefault reads an optional positive float, falling back to def.
func parseFloatDefault(name string, def float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive number, got %q", name, raw)
	}
	return v, nil
}

// checkReadableFile proves the path is a regular file this process can open, not
// merely that it exists: a directory or a root-only file passes os.Stat and then
// fails at the first customer upload.
func checkReadableFile(name, path string) error {
	// Open through a Root scoped to the file's own directory so the final
	// component cannot traverse or follow a symlink out of it (G304/G703).
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%s is not a valid path: %w", name, err)
	}
	root, err := os.OpenRoot(filepath.Dir(abs))
	if err != nil {
		return fmt.Errorf("%s is not readable: %w", name, err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(filepath.Base(abs))
	if err != nil {
		return fmt.Errorf("%s is not readable: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%s is not readable: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file, got %s", name, info.Mode().Type())
	}
	// Openable is not decodable: a text file named qr.png passed boot and failed
	// at the first customer tap.
	if _, _, err := image.DecodeConfig(f); err != nil {
		return fmt.Errorf("%s is not a readable PNG or JPEG image", name)
	}
	return nil
}

// validateListenAddr catches a typo at boot instead of inside the gateway
// goroutine, after the dashboard is already serving customers.
func validateListenAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("GATEWAY_ADDR must be host:port, got %q: %w", addr, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("GATEWAY_ADDR port must be 1-65535, got %q", port)
	}
	if !isLoopbackHost(host) && os.Getenv("GATEWAY_ALLOW_NON_LOOPBACK") != "1" {
		return fmt.Errorf("GATEWAY_ADDR %q is non-loopback; TLS termination is required in front of a non-loopback bind (set GATEWAY_ALLOW_NON_LOOPBACK=1 to allow it)", addr)
	}
	return nil
}

// parseAdminIDs turns "111,222" into a set. Blank entries are skipped.
func parseAdminIDs(raw string) (map[int64]bool, error) {
	ids := make(map[int64]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ADMIN_IDS has a non-numeric entry: %q", part)
		}
		// Telegram user IDs are positive; negative IDs are groups/channels, which
		// can never match a sender and would leave claims unapprovable.
		if id <= 0 {
			return nil, fmt.Errorf("ADMIN_IDS entries must be positive user IDs, got %q", part)
		}
		ids[id] = true
	}
	return ids, nil
}
