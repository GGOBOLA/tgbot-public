package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no CGO

	"tgbot/internal/ledger"
)

// nowUnix is the single clock read for the rebuilt store.
func nowUnix() int64 { return time.Now().Unix() }

// DB is the rebuilt store: one connection, WAL, and the v1 schema. It grows
// alongside Store until the callers move over, then Store is deleted.
type DB struct {
	db *sql.DB

	// hookMu guards lowBalanceHook: it is set once at startup and read from
	// every gateway goroutine that commits a debit.
	hookMu         sync.RWMutex
	lowBalanceHook func(ctx context.Context, telegramID int64, balance ledger.Amount)
}

// SetLowBalanceHook registers a callback fired once whenever a debit crosses
// below ledger.LowBalanceThreshold. It runs in its own goroutine after commit,
// so the caller's latency is untouched. Pass nil to disable.
func (d *DB) SetLowBalanceHook(hook func(ctx context.Context, telegramID int64, balance ledger.Amount)) {
	d.hookMu.Lock()
	d.lowBalanceHook = hook
	d.hookMu.Unlock()
}

// notifyLowBalance fires the hook off the request path.
func (d *DB) notifyLowBalance(telegramID int64, balance ledger.Amount) {
	d.hookMu.RLock()
	hook := d.lowBalanceHook
	d.hookMu.RUnlock()
	if hook == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hook(ctx, telegramID, balance)
	}()
}

// OpenDB opens (or creates) the database and applies the v1 schema.
func OpenDB(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		// Only narrow a dir this call created; a pre-existing parent is not ours
		// to chmod (DB_PATH=/tmp/x.db means /tmp) and is left alone on purpose,
		// even when world-readable. One Stat before MkdirAll decides that once.
		_, statErr := os.Stat(dir)
		created := errors.Is(statErr, os.ErrNotExist)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
		if created {
			// #nosec G302 -- this is a directory; 0700 needs the execute bit to be
			// traversable and grants nothing to group or other.
			if err := os.Chmod(dir, 0o700); err != nil {
				return nil, fmt.Errorf("restrict db dir: %w", err)
			}
		}
	}
	// _txlock=immediate takes the write lock at BeginTx instead of upgrading on
	// the first write: a deferred upgrade against a second process (seedprices,
	// telegramcheck, a copy tool) returns SQLITE_BUSY_SNAPSHOT, which the busy
	// timeout does not retry.
	dsn := "file:" + path + "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// One writer keeps SQLite serialized and makes the balance guard safe.
	db.SetMaxOpenConns(1)

	d := &DB{db: db}
	// Restrict the database and any WAL siblings before migrations can read or
	// write sensitive balances. Repeat after migrations to catch newly created
	// siblings.
	if err := restrictDBFiles(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := applySchema(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	// The file holds key hashes, balances and the ledger. sqlite creates it
	// world-readable, so narrow it (and the WAL/SHM siblings) to the owner.
	if err := restrictDBFiles(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

// restrictDBFiles narrows the database and its WAL siblings to mode 0600.
// Missing siblings are not an error: SQLite creates them lazily.
func restrictDBFiles(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		err := os.Chmod(p, 0o600)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("restrict %s: %w", p, err)
		}
	}
	return nil
}

// Close releases the handle.
func (d *DB) Close() error { return d.db.Close() }

// EnsureCustomer creates the customer row on first contact and is a no-op
// afterwards.
func (d *DB) EnsureCustomer(ctx context.Context, telegramID int64) error {
	if telegramID <= 0 {
		return fmt.Errorf("telegram id must be positive, got %d", telegramID)
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO customers (telegram_id, created_at, balance) VALUES (?, ?, 0)
		 ON CONFLICT(telegram_id) DO NOTHING`,
		telegramID, nowUnix())
	return err
}

// Balance reads one customer's balance.
func (d *DB) Balance(ctx context.Context, telegramID int64) (ledger.Amount, error) {
	var bal int64
	err := d.db.QueryRowContext(ctx,
		`SELECT balance FROM customers WHERE telegram_id = ?`, telegramID).Scan(&bal)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return ledger.Amount(bal), nil
}

// LastUpdateID returns the highest Telegram update id handled by the bot.
func (d *DB) LastUpdateID(ctx context.Context) (int, error) {
	var id int
	err := d.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'telegram_last_update_id'`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// SetLastUpdateID records a handled Telegram update. The upsert is serialized
// by the store's single-writer connection.
func (d *DB) SetLastUpdateID(ctx context.Context, id int) error {
	_, err := d.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('telegram_last_update_id', ?)
		ON CONFLICT(key) DO UPDATE SET value = MAX(value, excluded.value)`, id)
	return err
}
