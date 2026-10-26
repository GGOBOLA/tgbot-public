// Command telegramcheck sends every screen to a real Telegram chat and deletes
// it again, so Telegram itself validates what unit tests can only approximate.
//
//	TELEGRAM_CONTRACT_TOKEN="$TOKEN" go run ./cmd/telegramcheck -chat 123456789
//
// Use a throwaway bot and your own chat: it posts and deletes one message per
// screen. Never point it at a production token in CI.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tgbot/internal/bot"
	"tgbot/internal/callback"
	"tgbot/internal/obs"
	"tgbot/internal/store"
)

func main() {
	chatID := flag.Int64("chat", 0, "chat to post the screens to (your own user id)")
	keep := flag.Bool("keep", false, "leave the messages in place instead of deleting them")
	flag.Parse()

	token, err := contractToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if *chatID == 0 {
		fmt.Fprintln(os.Stderr, "usage: TELEGRAM_CONTRACT_TOKEN=<bot token> telegramcheck -chat <chat id>")
		os.Exit(2)
	}
	if err := run(token, *chatID, *keep); err != nil {
		fmt.Fprintln(os.Stderr, "telegramcheck:", err)
		os.Exit(1)
	}
}

func run(token string, chatID int64, keep bool) error {
	return runWithClient(token, chatID, keep, &http.Client{Timeout: 70 * time.Second})
}

func runWithClient(token string, chatID int64, keep bool, client tgbotapi.HTTPClient) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	api, err := tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, client)
	if err != nil {
		return fmt.Errorf("telegram auth failed: check the token and network access")
	}

	dir, err := os.MkdirTemp("", "telegramcheck-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	db, err := store.OpenDB(filepath.Join(dir, "check.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	codec, err := callback.New([]byte("telegramcheck-secret-0123456789"))
	if err != nil {
		return err
	}

	b, err := bot.New(api, db, codec, obs.New(os.Stderr), bot.Options{
		Admins:           map[int64]bool{chatID: true},
		PaymentQRPath:    envOr("PAYMENT_QR_PATH", ""),
		WelcomeImagePath: envOr("WELCOME_IMAGE_PATH", ""),
	})
	if err != nil {
		return err
	}

	screens, err := b.ContractScreens(ctx, chatID)
	if err != nil {
		return err
	}

	failures := 0
	for _, s := range screens {
		msg, err := api.Send(s.Chat)
		if err != nil {
			failures++
			// A *url.Error embeds the request URL, and this one carries the bot
			// token; the bot's own logger redacts for the same reason.
			fmt.Printf("FAIL  %-16s %s\n", s.Name, obs.Redact(err.Error()))
			continue
		}
		fmt.Printf("ok    %-16s message %d\n", s.Name, msg.MessageID)
		if keep {
			continue
		}
		// DeleteMessage answers with a bare `true`, not a Message, so it goes
		// through Request rather than Send.
		if _, err := api.Request(tgbotapi.NewDeleteMessage(chatID, msg.MessageID)); err != nil {
			fmt.Printf("      (could not delete message %d: %s)\n", msg.MessageID, obs.Redact(err.Error()))
		}
		// Telegram rate-limits a burst of edits/sends; a short gap keeps the run
		// readable instead of tripping 429.
		time.Sleep(300 * time.Millisecond)
	}

	if failures > 0 {
		return fmt.Errorf("%d of %d screens were rejected by Telegram", failures, len(screens))
	}
	fmt.Printf("\nall %d screens accepted by Telegram\n", len(screens))
	return nil
}

func contractToken() (string, error) {
	token := os.Getenv("TELEGRAM_CONTRACT_TOKEN")
	if token == "" {
		return "", fmt.Errorf("TELEGRAM_CONTRACT_TOKEN is required")
	}
	return token, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
