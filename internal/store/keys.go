package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
)

// MaxActiveKeys is the most active keys one customer may hold.
const MaxActiveKeys = 5

// ErrKeyCap is returned when a customer already holds MaxActiveKeys.
var ErrKeyCap = errors.New("active key limit reached")

// Key is one customer key. The plaintext key is shown once, at creation.
type Key struct {
	ID         int64
	TelegramID int64
	Name       string
	Prefix     string
	Active     bool
	CreatedAt  int64
}

// CreateKey mints sk- + 64 hex chars, stores only the SHA-256 hash and an
// 8-char prefix, and returns the plaintext exactly once. The insert is
// conditional, so the 5-key cap holds under concurrent requests.
func (d *DB) CreateKey(ctx context.Context, telegramID int64, name string) (string, Key, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return "", Key{}, errors.New("key name must be 1-64 characters")
	}
	if telegramID <= 0 {
		return "", Key{}, errors.New("telegram id must be positive")
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", Key{}, err
	}
	plain := "sk-" + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plain))
	prefix := plain[:11]
	created := nowUnix()

	res, err := d.db.ExecContext(ctx,
		`INSERT INTO api_keys (telegram_id, name, prefix, hash, active, created_at)
		 SELECT ?, ?, ?, ?, 1, ?
		 WHERE (SELECT COUNT(*) FROM api_keys WHERE telegram_id = ? AND active = 1) < ?`,
		telegramID, name, prefix, sum[:], created, telegramID, MaxActiveKeys)
	if err != nil {
		return "", Key{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", Key{}, err
	}
	if n == 0 {
		return "", Key{}, ErrKeyCap
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", Key{}, err
	}
	return plain, Key{
		ID: id, TelegramID: telegramID, Name: name, Prefix: prefix, Active: true, CreatedAt: created,
	}, nil
}

// ListKeys returns a customer's keys, oldest first.
func (d *DB) ListKeys(ctx context.Context, telegramID int64) ([]Key, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, telegram_id, name, prefix, active, created_at
		 FROM api_keys WHERE telegram_id = ? ORDER BY id`, telegramID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var keys []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.ID, &k.TelegramID, &k.Name, &k.Prefix, &k.Active, &k.CreatedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// RevokeKey deactivates one key owned by the customer. It refuses a key that
// is already inactive or owned by someone else.
func (d *DB) RevokeKey(ctx context.Context, telegramID, keyID int64) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE api_keys SET active = 0, revoked_at = ?
		 WHERE id = ? AND telegram_id = ? AND active = 1`, nowUnix(), keyID, telegramID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// KeyByHash resolves an active key from its SHA-256 hash for gateway auth.
func (d *DB) KeyByHash(ctx context.Context, hash []byte) (Key, error) {
	var k Key
	err := d.db.QueryRowContext(ctx,
		`SELECT id, telegram_id, name, prefix, active, created_at
		 FROM api_keys WHERE hash = ? AND active = 1`, hash).
		Scan(&k.ID, &k.TelegramID, &k.Name, &k.Prefix, &k.Active, &k.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	if err != nil {
		return Key{}, err
	}
	return k, nil
}
