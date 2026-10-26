package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"tgbot/internal/ledger"
)

// v1FixtureSchema is the v1 shape of the three tables the migration test needs,
// copied from the v1 build: the usage table has no `estimated` column. Building
// the fixture from this literal rather than from schemaV1 is the point — it
// exercises the v1 -> v2 migration instead of the fresh CREATE path.
const v1FixtureSchema = `
CREATE TABLE customers (
	telegram_id INTEGER PRIMARY KEY,
	created_at  INTEGER NOT NULL,
	balance     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE api_keys (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	telegram_id INTEGER NOT NULL REFERENCES customers(telegram_id) ON DELETE CASCADE,
	name        TEXT    NOT NULL,
	prefix      TEXT    NOT NULL,
	hash        BLOB    NOT NULL UNIQUE,
	active      INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
	created_at  INTEGER NOT NULL,
	revoked_at  INTEGER
);
CREATE TABLE usage (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	telegram_id       INTEGER NOT NULL REFERENCES customers(telegram_id) ON DELETE CASCADE,
	key_id            INTEGER REFERENCES api_keys(id) ON DELETE SET NULL,
	model             TEXT    NOT NULL,
	prompt_tokens     INTEGER NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
	completion_tokens INTEGER NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
	cost              INTEGER NOT NULL DEFAULT 0 CHECK (cost >= 0),
	created_at        INTEGER NOT NULL,
	first_ms          INTEGER NOT NULL DEFAULT 0
);`

// writeV1Fixture leaves a v1 database on disk with a customer and a priced
// usage row, so the migration has real values to preserve.
func writeV1Fixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open v1 fixture: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(v1FixtureSchema); err != nil {
		_ = db.Close()
		t.Fatalf("create v1 fixture: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO customers (telegram_id, created_at, balance) VALUES (900, 1000, 5000)`); err != nil {
		_ = db.Close()
		t.Fatalf("seed v1 customer: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO usage (telegram_id, key_id, model, prompt_tokens, completion_tokens, cost, created_at, first_ms)
		 VALUES (900, NULL, 'v1/model', 11, 7, 4321, 1234, 56)`); err != nil {
		_ = db.Close()
		t.Fatalf("seed v1 usage: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		_ = db.Close()
		t.Fatalf("record v1 version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v1 fixture: %v", err)
	}
}

// usageColumns returns the column names of the usage table in declaration order.
func usageColumns(t *testing.T, d *DB) []string {
	t.Helper()
	rows, err := d.db.Query(`SELECT name FROM pragma_table_info('usage') ORDER BY cid`)
	if err != nil {
		t.Fatalf("read usage columns: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan usage column: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read usage columns: %v", err)
	}
	return names
}

// A database written by the v1 build must be upgraded in place: the marker
// column appears, every row keeps its values, and the stored version moves to 2.
func TestApplySchemaMigratesV1ToV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	writeV1Fixture(t, path)

	d, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB on a v1 database: %v", err)
	}
	defer func() { _ = d.Close() }()
	ctx := context.Background()

	var version int
	if err := d.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version = %d after migrating a v1 database, want %d", version, schemaVersion)
	}

	var (
		model              string
		prompt, completion int64
		cost, created, fm  int64
		estimated          int64
	)
	if err := d.db.QueryRow(
		`SELECT model, prompt_tokens, completion_tokens, cost, created_at, first_ms, estimated
		 FROM usage WHERE telegram_id = 900`).
		Scan(&model, &prompt, &completion, &cost, &created, &fm, &estimated); err != nil {
		t.Fatalf("read the migrated row: %v", err)
	}
	if model != "v1/model" || prompt != 11 || completion != 7 || cost != 4321 || created != 1234 || fm != 56 {
		t.Fatalf("migrated row lost values: model=%q prompt=%d completion=%d cost=%d created=%d first_ms=%d",
			model, prompt, completion, cost, created, fm)
	}
	if estimated != 0 {
		t.Fatalf("estimated = %d on a pre-existing row, want 0: the old build only billed real usage", estimated)
	}

	var balance int64
	if err := d.db.QueryRow(`SELECT balance FROM customers WHERE telegram_id = 900`).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 5000 {
		t.Fatalf("customer balance = %d after migration, want 5000 untouched", balance)
	}

	// The migrated table must carry the same marker semantics as a fresh one:
	// the new flag is writable and its CHECK is enforced.
	if err := d.RecordUsage(ctx, UsageEntry{
		TelegramID: 900, Model: "v2/model", Cost: ledger.Amount(7), Estimated: true,
	}); err != nil {
		t.Fatalf("record an estimated row on the migrated table: %v", err)
	}
	var flag int64
	if err := d.db.QueryRow(
		`SELECT estimated FROM usage WHERE model = 'v2/model'`).Scan(&flag); err != nil {
		t.Fatal(err)
	}
	if flag != 1 {
		t.Fatalf("estimated = %d for a row recorded as an estimate, want 1", flag)
	}
	if _, err := d.db.Exec(`UPDATE usage SET estimated = 2 WHERE telegram_id = 900`); err == nil {
		t.Fatal("migrated usage table accepted estimated = 2: the CHECK did not survive the ALTER")
	}

	// Same columns in the same order as a database created at v2, so the two
	// paths cannot drift apart.
	fresh := openTestDB(t)
	got, want := usageColumns(t, d), usageColumns(t, fresh)
	if len(got) != len(want) {
		t.Fatalf("migrated usage columns = %v, fresh = %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("migrated usage columns = %v, fresh = %v", got, want)
		}
	}
}

// A v2 database predates the price audit table. Opening it must add the table
// and its index, leave the prices already stored alone, and record v3 — the
// gateway then reads the same prices it did before the upgrade.
func TestApplySchemaMigratesV2ToV3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	ctx := context.Background()

	d, err := OpenDB(path)
	if err != nil {
		t.Fatalf("first OpenDB: %v", err)
	}
	if err := d.SetPrice(ctx, ModelPrice{Model: "m", PromptPerMillion: 100, CompletionPerMillion: 200}); err != nil {
		t.Fatal(err)
	}
	// Rewind the file to what a v2 build wrote: no audit table, version 2.
	if _, err := d.db.Exec(`DROP TABLE price_changes`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d2, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB on a v2 database: %v", err)
	}
	defer func() { _ = d2.Close() }()

	var version int
	if err := d2.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version = %d after migrating a v2 database, want %d", version, schemaVersion)
	}

	var name string
	if err := d2.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'price_changes'`).Scan(&name); err != nil {
		t.Fatalf("price_changes table missing after migration: %v", err)
	}
	if err := d2.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_price_changes_model'`).Scan(&name); err != nil {
		t.Fatalf("price_changes index missing after migration: %v", err)
	}

	p, err := d2.Price(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if p.PromptPerMillion != 100 || p.CompletionPerMillion != 200 {
		t.Fatalf("migration changed the stored price: %+v", p)
	}
}

// Re-opening a current database must do nothing at all: no second ALTER, no
// error, and rows already marked estimated stay marked.
func TestApplySchemaReopenIsNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	ctx := context.Background()

	d, err := OpenDB(path)
	if err != nil {
		t.Fatalf("first OpenDB: %v", err)
	}
	if err := d.EnsureCustomer(ctx, 901); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordUsage(ctx, UsageEntry{TelegramID: 901, Model: "m", Cost: 1, Estimated: true}); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordUsage(ctx, UsageEntry{TelegramID: 901, Model: "m", Cost: 2}); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d2, err := OpenDB(path)
	if err != nil {
		t.Fatalf("re-opening a v2 database: %v", err)
	}
	defer func() { _ = d2.Close() }()

	var version int
	if err := d2.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version = %d after reopening, want %d", version, schemaVersion)
	}
	rows, err := d2.db.Query(`SELECT cost, estimated FROM usage WHERE telegram_id = 901 ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var cost, estimated int64
		if err := rows.Scan(&cost, &estimated); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmtUsage(cost, estimated))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "1/1" || got[1] != "2/0" {
		t.Fatalf("rows after reopen = %v, want [1/1 2/0]", got)
	}
}

// A second startup against a database whose migration has committed must skip
// the ALTER step instead of attempting to add estimated again.
func TestApplySchemaAlreadyMigratedSkipsSteps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	writeV1Fixture(t, path)

	d, err := OpenDB(path)
	if err != nil {
		t.Fatalf("first OpenDB: %v", err)
	}
	defer func() { _ = d.Close() }()

	if err := applySchema(context.Background(), d.db); err != nil {
		t.Fatalf("second migration of already-migrated database: %v", err)
	}
	var count int
	if err := d.db.QueryRow(`SELECT count(*) FROM pragma_table_info('usage') WHERE name = 'estimated'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("estimated column count = %d, want 1", count)
	}
}

func fmtUsage(cost, estimated int64) string {
	return fmt.Sprintf("%d/%d", cost, estimated)
}

// A database created from scratch lands on v2 directly, with the marker present
// and defaulted to 0.
func TestFreshSchemaHasEstimatedMarker(t *testing.T) {
	d := openTestDB(t)
	if err := d.EnsureCustomer(context.Background(), 902); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(
		`INSERT INTO usage (telegram_id, model, cost, created_at) VALUES (902, 'm', 5, 1)`); err != nil {
		t.Fatalf("insert a plain usage row: %v", err)
	}
	var estimated int64
	if err := d.db.QueryRow(`SELECT estimated FROM usage WHERE telegram_id = 902`).Scan(&estimated); err != nil {
		t.Fatalf("read estimated: %v", err)
	}
	if estimated != 0 {
		t.Fatalf("estimated = %d, want 0 by default", estimated)
	}
	if _, err := d.db.Exec(`INSERT INTO usage (telegram_id, model, cost, created_at, estimated)
		VALUES (902, 'm', 5, 1, 2)`); err == nil {
		t.Fatal("fresh usage table accepted estimated = 2")
	}
}

func TestUsageCreatedIndexOnFreshAndMigratedDatabases(t *testing.T) {
	indexExists := func(t *testing.T, d *DB) bool {
		t.Helper()
		var name string
		err := d.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_usage_created'`).Scan(&name)
		if err == sql.ErrNoRows {
			return false
		}
		if err != nil {
			t.Fatalf("query usage created index: %v", err)
		}
		return name == "idx_usage_created"
	}

	fresh := openTestDB(t)
	if !indexExists(t, fresh) {
		t.Fatal("fresh database is missing idx_usage_created")
	}

	path := filepath.Join(t.TempDir(), "v4.db")
	d, err := OpenDB(path)
	if err != nil {
		t.Fatalf("create database to rewind to v4: %v", err)
	}
	if _, err := d.db.Exec(`DROP INDEX idx_usage_created`); err != nil {
		t.Fatalf("remove v5 index: %v", err)
	}
	if _, err := d.db.Exec(`ALTER TABLE claims DROP COLUMN expires_at`); err != nil {
		t.Fatalf("remove v6 claim expiry column: %v", err)
	}
	if _, err := d.db.Exec(`PRAGMA user_version = 4`); err != nil {
		t.Fatalf("record v4 version: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB on a v4 database: %v", err)
	}
	defer func() { _ = migrated.Close() }()
	if !indexExists(t, migrated) {
		t.Fatal("migrated database is missing idx_usage_created")
	}
}

func TestApplySchemaMigratesLedgerToAppendOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v6.db")
	d, err := OpenDB(path)
	if err != nil {
		t.Fatalf("create v6 fixture: %v", err)
	}
	if err := d.EnsureCustomer(ctx, 1200); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Credit(ctx, 1200, 100, "topup", "audit-topup"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ChargeUsage(ctx, UsageEntry{TelegramID: 1200, Model: "m", Cost: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`PRAGMA user_version = 6`); err != nil {
		t.Fatal(err)
	}
	if err := applySchema(ctx, d.db); err != nil {
		t.Fatalf("migrate v6 to v7: %v", err)
	}

	var version int
	if err := d.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version = %d after migration, want %d", version, schemaVersion)
	}
	var rows int
	if err := d.db.QueryRow(`SELECT count(*) FROM ledger WHERE telegram_id = 1200`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("migrated ledger rows = %d, want 2", rows)
	}
	var balance, ledgerTotal int64
	if err := d.db.QueryRow(`SELECT balance FROM customers WHERE telegram_id = 1200`).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if err := d.db.QueryRow(`SELECT sum(delta) FROM ledger WHERE telegram_id = 1200`).Scan(&ledgerTotal); err != nil {
		t.Fatal(err)
	}
	if balance != 80 || ledgerTotal != balance {
		t.Fatalf("migrated balance=%d ledger total=%d, want both 80", balance, ledgerTotal)
	}

	// Both reconciliation comparisons used by scripts/reconcile.sh remain zero
	// on this migrated fixture.
	var usageCost, usageLedger int64
	if err := d.db.QueryRow(`SELECT coalesce(sum(cost), 0) FROM usage WHERE estimated = 0`).Scan(&usageCost); err != nil {
		t.Fatal(err)
	}
	if err := d.db.QueryRow(`SELECT coalesce(sum(-delta), 0) FROM ledger WHERE reason = 'usage'`).Scan(&usageLedger); err != nil {
		t.Fatal(err)
	}
	if usageCost != usageLedger || balance != ledgerTotal {
		t.Fatalf("reconciliation mismatch: usage=%d ledger usage=%d balance=%d ledger total=%d", usageCost, usageLedger, balance, ledgerTotal)
	}

	if _, err := d.Credit(ctx, 1200, 5, "topup", "audit-after-migration"); err != nil {
		t.Fatalf("insert into migrated ledger: %v", err)
	}
	if _, err := d.db.Exec(`DELETE FROM customers WHERE telegram_id = 1200`); err == nil {
		t.Fatal("customer delete with ledger rows succeeded")
	}
	if _, err := d.db.Exec(`DELETE FROM ledger WHERE telegram_id = 1200`); err == nil {
		t.Fatal("ledger delete succeeded")
	}
	if _, err := d.db.Exec(`UPDATE ledger SET reason = 'changed' WHERE ref = 'audit-topup'`); err == nil {
		t.Fatal("ledger update succeeded")
	}

	if err := applySchema(ctx, d.db); err != nil {
		t.Fatalf("re-run schema migration: %v", err)
	}
	if err := d.db.QueryRow(`SELECT count(*) FROM ledger WHERE telegram_id = 1200`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Fatalf("ledger rows after idempotent re-run = %d, want 3", rows)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFreshLedgerIsAppendOnly(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 1201); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Credit(ctx, 1201, 10, "topup", "fresh-audit"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`DELETE FROM ledger WHERE ref = 'fresh-audit'`); err == nil {
		t.Fatal("fresh ledger delete succeeded")
	}
	if _, err := d.db.Exec(`UPDATE ledger SET reason = 'changed' WHERE ref = 'fresh-audit'`); err == nil {
		t.Fatal("fresh ledger update succeeded")
	}
}
