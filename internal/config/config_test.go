package config

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"tgbot/internal/callback"
)

// baseEnv sets a minimal valid environment, clearing every variable Load reads so
// the tests do not inherit whatever the developer's shell has.
func baseEnv(t *testing.T) {
	t.Helper()
	for _, kv := range [][2]string{
		{"TELEGRAM_BOT_TOKEN", "1:x"},
		{"DB_PATH", ""},
		{"ADMIN_IDS", "111"},
		{"CALLBACK_SECRET", "0123456789abcdef0123456789abcdef"},
		{"PAYMENT_QR_PATH", ""},
		{"WELCOME_IMAGE_PATH", ""},
		{"NINEROUTER_URL", ""},
		{"NINEROUTER_KEY", ""},
		{"GATEWAY_ADDR", ""},
		{"GATEWAY_ALLOW_NON_LOOPBACK", ""},
		{"RATE_LIMIT_PER_SEC", ""},
		{"RATE_LIMIT_BURST", ""},
	} {
		t.Setenv(kv[0], kv[1])
	}
}

func TestValidateListenAddr(t *testing.T) {
	cases := []struct {
		addr      string
		allowWide bool
		wantErr   bool
	}{
		{"127.0.0.1:8787", false, false},
		{"localhost:8787", false, false},
		{"[::1]:8787", false, false},
		{"0.0.0.0:8787", false, true},
		{"192.0.2.5:8787", false, true},
		{"0.0.0.0:8787", true, false},
		{"192.0.2.5:8787", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.addr+"/allow="+strconv.FormatBool(tc.allowWide), func(t *testing.T) {
			t.Setenv("GATEWAY_ALLOW_NON_LOOPBACK", "")
			if tc.allowWide {
				t.Setenv("GATEWAY_ALLOW_NON_LOOPBACK", "1")
			}
			err := validateListenAddr(tc.addr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateListenAddr(%q) error = %v, wantErr %v", tc.addr, err, tc.wantErr)
			}
			if tc.wantErr && (!strings.Contains(err.Error(), tc.addr) || !strings.Contains(err.Error(), "TLS termination") || !strings.Contains(err.Error(), "GATEWAY_ALLOW_NON_LOOPBACK=1")) {
				t.Fatalf("error does not explain the non-loopback requirement and escape hatch: %v", err)
			}
		})
	}
}

func TestLoadValid(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.AdminIDs[111] || cfg.DBPath != "./data/bot.db" {
		t.Fatalf("defaults wrong: admins=%v db=%q", cfg.AdminIDs, cfg.DBPath)
	}
}

// TestBrandNameIsIgnored pins the removal of BRAND_NAME. Nothing consumed the
// value — bot.Options has no brand field and no screen carries brand copy — so
// Load must neither read nor validate it. A length that used to fail boot is the
// sharpest probe: if a stray read comes back, this goes red.
func TestBrandNameIsIgnored(t *testing.T) {
	baseEnv(t)
	t.Setenv("BRAND_NAME", strings.Repeat("x", 200))
	if _, err := Load(); err != nil {
		t.Fatalf("BRAND_NAME must not be read: %v", err)
	}
}

// TestCallbackSecretFloorMatchesCodec pins the single floor for the HMAC key.
// The config gate and callback.New must agree: a key shorter than the
// codec's minimum either fails boot for no reason or boots and then refuses
// every button. The enforced floor is callback.MinSecret; the 32-character
// value stays a recommendation in the message.
func TestCallbackSecretFloorMatchesCodec(t *testing.T) {
	baseEnv(t)

	t.Setenv("CALLBACK_SECRET", strings.Repeat("a", callback.MinSecret))
	if _, err := Load(); err != nil {
		t.Fatalf("a %d-byte secret is the codec minimum and must pass boot: %v", callback.MinSecret, err)
	}

	t.Setenv("CALLBACK_SECRET", strings.Repeat("a", callback.MinSecret-1))
	_, err := Load()
	if err == nil {
		t.Fatalf("a %d-byte secret is below the codec minimum and must fail boot", callback.MinSecret-1)
	}
	if !strings.Contains(err.Error(), "openssl rand -hex 32") {
		t.Fatalf("the error must keep the 32-char recommendation, got %v", err)
	}
}

func TestPaymentQRPath(t *testing.T) {
	baseEnv(t)

	cfg, err := Load()
	if err != nil || cfg.PaymentQRPath != "" {
		t.Fatalf("qr optional: %v", err)
	}

	// A configured but missing file fails fast, not on first customer tap.
	t.Setenv("PAYMENT_QR_PATH", filepath.Join(t.TempDir(), "nope.png"))
	if _, err := Load(); err == nil {
		t.Fatal("missing QR file must fail fast")
	}

	// A directory exists but is not a usable image: os.Stat alone let this pass.
	t.Setenv("PAYMENT_QR_PATH", t.TempDir())
	if _, err := Load(); err == nil {
		t.Fatal("directory must not pass as a QR file")
	}

	ok := filepath.Join(t.TempDir(), "qr.png")
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ok, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAYMENT_QR_PATH", ok)
	if cfg, err := Load(); err != nil || cfg.PaymentQRPath != ok {
		t.Fatalf("qr set: %v", err)
	}

	// A text file named .png is not an image: the bot would fail on the first
	// customer tap, so boot must refuse it.
	fake := filepath.Join(t.TempDir(), "fake.png")
	if err := os.WriteFile(fake, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAYMENT_QR_PATH", fake)
	if _, err := Load(); err == nil {
		t.Fatal("a non-image .png must not pass boot")
	}
}

// TestEnvExampleShipsNoQrPath pins the template contract. The real QR is
// git-ignored, so a template that points at assets/duitnow.jpg makes every clean
// checkout fail boot on a file that is not in the repo. The template ships the
// variable empty and the operator opts in on the host.
func TestEnvExampleShipsNoQrPath(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key != "PAYMENT_QR_PATH" {
			continue
		}
		found = true
		if value != "" {
			t.Fatalf(".env.example sets PAYMENT_QR_PATH=%q, want it empty: the QR is git-ignored", value)
		}
	}
	if !found {
		t.Fatal(".env.example has no PAYMENT_QR_PATH line")
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		env  [2]string
		want string
	}{
		{"missing token", [2]string{"TELEGRAM_BOT_TOKEN", ""}, "TELEGRAM_BOT_TOKEN is required"},
		{"no admins", [2]string{"ADMIN_IDS", ""}, "at least one"},
		{"bad admin id", [2]string{"ADMIN_IDS", "111,abc"}, "non-numeric"},
		{"group admin id", [2]string{"ADMIN_IDS", "-100123"}, "positive user IDs"},
		{"nan rate", [2]string{"RATE_LIMIT_PER_SEC", "NaN"}, "positive number"},
		{"inf rate", [2]string{"RATE_LIMIT_BURST", "Inf"}, "positive number"},
		{"zero rate", [2]string{"RATE_LIMIT_PER_SEC", "0"}, "positive number"},
		{"tiny rate", [2]string{"RATE_LIMIT_PER_SEC", "1e-12"}, "between 0.1 and 1000"},
		{"huge burst", [2]string{"RATE_LIMIT_BURST", "1e9"}, "between 1 and 10000"},
		{"gateway without key", [2]string{"GATEWAY_ADDR", "127.0.0.1:8787"}, "NINEROUTER_KEY is empty"},
		{"gateway addr no port", [2]string{"GATEWAY_ADDR", "8787"}, "host:port"},
		{"gateway port range", [2]string{"GATEWAY_ADDR", ":99999"}, "1-65535"},
		{"bad upstream scheme", [2]string{"NINEROUTER_URL", "ftp://x"}, "scheme must be http or https"},
		{"upstream credentials", [2]string{"NINEROUTER_URL", "https://user:pw@x"}, "must not contain credentials"},
		{"upstream bad port", [2]string{"NINEROUTER_URL", "https://host:99999"}, "invalid port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv(tc.env[0], tc.env[1])
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestCloudDefaultOnlyWithKey(t *testing.T) {
	baseEnv(t)
	// No key: the price import has nothing to authenticate with, so the URL stays
	// empty and the bot reports "no provider configured" instead of dialling out.
	if cfg, err := Load(); err != nil || cfg.NinerouterURL != "" {
		t.Fatalf("no key: url %q (%v), want empty", cfg.NinerouterURL, err)
	}
	// A key with no URL must refuse to boot rather than silently dial the vendor
	// cloud with the operator's key and every customer prompt.
	t.Setenv("NINEROUTER_KEY", "sk-x")
	if _, err := Load(); err == nil {
		t.Fatal("key set without a URL: want a boot error, got none")
	}
	t.Setenv("NINEROUTER_URL", "https://9router.com")
	cfg, err := Load()
	if err != nil || cfg.NinerouterURL != "https://9router.com" {
		t.Fatalf("explicit url: url %q (%v), want the named upstream", cfg.NinerouterURL, err)
	}
}
