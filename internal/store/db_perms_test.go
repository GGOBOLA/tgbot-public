package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenDBRestrictsFileBeforeFailedMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	seed, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDB(path); err == nil {
		t.Fatal("OpenDB succeeded with a newer schema version")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("db mode after failed migration = %o, want 600", got)
	}
}

// The database holds key hashes, balances and the ledger; sqlite honours the
// process umask, which on a default system leaves it world-readable.
func TestOpenDBRestrictsFileMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "bot.db")

	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("db mode = %o, want 600", got)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode = %o, want 700", got)
	}
}

// A WAL sibling that exists must be narrowed too, since it holds unflushed
// pages from the same ledger.
func TestRestrictDBFilesNarrowsSiblings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	if err := os.WriteFile(path+"-wal", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restrictDBFiles(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("wal mode = %o, want 600", got)
	}
}

func TestOpenDBStillWorks(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.EnsureCustomer(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
}

// A directory that already exists keeps its mode. OpenDB used to chmod the
// parent unconditionally, which is only safe when the parent is one this call
// made: with DB_PATH=/tmp/x.db the parent is /tmp, and narrowing it to 0700
// locks the whole host out of its own temp dir. Shared parents are not ours to
// narrow, so they are left exactly as found.
func TestOpenDBLeavesExistingDirModeAlone(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o755); err != nil { // defeat the test umask
		t.Fatal(err)
	}

	db, err := OpenDB(filepath.Join(shared, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	si, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}
	if got := si.Mode().Perm(); got != 0o755 {
		t.Fatalf("pre-existing dir mode = %o, want 755: not ours to narrow", got)
	}
}
