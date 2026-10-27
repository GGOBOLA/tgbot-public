package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"tgbot/internal/ledger"
)

// v3 tables are the two the v4 step rebuilds, in the shape version 3 wrote them:
// money columns bounded below, but with no upper bound.
const v3Tables = `
CREATE TABLE customers (
	telegram_id INTEGER PRIMARY KEY,
	created_at  INTEGER NOT NULL,
	balance     INTEGER NOT NULL DEFAULT 0
	            CHECK (balance >= -2305843009213693951 AND balance <= 2305843009213693951)
);
CREATE TABLE claims (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	telegram_id INTEGER NOT NULL REFERENCES customers(telegram_id) ON DELETE CASCADE,
	amount      INTEGER NOT NULL CHECK (amount > 0),
	status      TEXT    NOT NULL DEFAULT 'pending'
	            CHECK (status IN ('pending', 'approved', 'rejected')),
	created_at  INTEGER NOT NULL,
	decided_at  INTEGER,
	decided_by  INTEGER
);
CREATE UNIQUE INDEX idx_claims_one_pending ON claims(telegram_id) WHERE status = 'pending';
CREATE INDEX idx_claims_status ON claims(status, id);
CREATE TABLE usage (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	telegram_id       INTEGER NOT NULL REFERENCES customers(telegram_id) ON DELETE CASCADE,
	key_id            INTEGER,
	model             TEXT    NOT NULL,
	prompt_tokens     INTEGER NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
	completion_tokens INTEGER NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
	cost              INTEGER NOT NULL DEFAULT 0 CHECK (cost >= 0),
	created_at        INTEGER NOT NULL,
	first_ms          INTEGER NOT NULL DEFAULT 0,
	estimated         INTEGER NOT NULL DEFAULT 0 CHECK (estimated IN (0, 1))
);
CREATE INDEX idx_usage_owner ON usage(telegram_id, created_at);
PRAGMA user_version = 3;
`

// A version-3 database must upgrade in place: the money rows survive, the new
// upper bounds apply, and the indexes the rebuild dropped come back.
func TestSchemaV4RebuildsMoneyTablesInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")

	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(v3Tables); err != nil {
		t.Fatalf("create v3 schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO customers (telegram_id, created_at, balance) VALUES (400, 1, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO claims (telegram_id, amount, status, created_at) VALUES (400, 7000000, 'approved', 2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO usage (telegram_id, key_id, model, prompt_tokens, completion_tokens, cost, created_at, estimated)
		VALUES (400, NULL, 'm', 10, 20, 123456, 3, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB on a v3 file: %v", err)
	}
	defer func() { _ = d.Close() }()
	ctx := context.Background()

	var version int
	if err := d.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version = %d, want %d", version, schemaVersion)
	}

	// The rows are still there, with their values.
	var amount, cost, estimated int64
	if err := d.db.QueryRowContext(ctx, `SELECT amount FROM claims WHERE telegram_id = 400`).Scan(&amount); err != nil {
		t.Fatalf("claim row lost: %v", err)
	}
	if amount != 7000000 {
		t.Fatalf("claim amount = %d, want 7000000", amount)
	}
	if err := d.db.QueryRowContext(ctx, `SELECT cost, estimated FROM usage WHERE telegram_id = 400`).Scan(&cost, &estimated); err != nil {
		t.Fatalf("usage row lost: %v", err)
	}
	if cost != 123456 || estimated != 1 {
		t.Fatalf("usage row = cost %d estimated %d, want 123456/1", cost, estimated)
	}

	// The new bounds are enforced in both directions.
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO claims (telegram_id, amount, status, created_at) VALUES (400, ?, 'approved', 4)`, maxMoney+1); err == nil {
		t.Fatal("a claim above maxMoney was accepted")
	}
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO usage (telegram_id, model, cost, created_at) VALUES (400, 'm', ?, 5)`, maxMoney+1); err == nil {
		t.Fatal("a cost above maxMoney was accepted")
	}
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO usage (telegram_id, model, cost, created_at) VALUES (400, 'm', -1, 6)`); err == nil {
		t.Fatal("a negative cost was accepted")
	}

	// The rebuild recreated the one-pending index: a second open claim fails.
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO claims (telegram_id, amount, status, created_at) VALUES (400, 1, 'pending', 7)`); err != nil {
		t.Fatalf("first pending claim: %v", err)
	}
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO claims (telegram_id, amount, status, created_at) VALUES (400, 2, 'pending', 8)`); err == nil ||
		!strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("second pending claim = %v, want a UNIQUE violation", err)
	}

	// And the file is still structurally sound.
	var integrity string
	if err := d.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q", integrity)
	}
}

func TestSchemaV9BoundsPricesAndPreservesAudit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v8.db")
	ctx := context.Background()
	d, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`INSERT INTO prices (model, prompt_per_million, completion_per_million, updated_at) VALUES ('legacy', 10, 20, 30)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`INSERT INTO price_changes (model, old_prompt, old_completion, new_prompt, new_completion, changed_by, created_at) VALUES ('legacy', 1, 2, 10, 20, 7, 30)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`PRAGMA user_version = 8`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d, err = OpenDB(path)
	if err != nil {
		t.Fatalf("migrate v8 database: %v", err)
	}
	defer func() { _ = d.Close() }()
	var version int
	if err := d.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version = %d, want %d", version, schemaVersion)
	}
	var prompt, completion int64
	if err := d.db.QueryRowContext(ctx, `SELECT prompt_per_million, completion_per_million FROM prices WHERE model = 'legacy'`).Scan(&prompt, &completion); err != nil {
		t.Fatalf("read migrated price: %v", err)
	}
	if prompt != 10 || completion != 20 {
		t.Fatalf("migrated price = %d/%d, want 10/20", prompt, completion)
	}
	var audits int
	if err := d.db.QueryRowContext(ctx, `SELECT count(*) FROM price_changes WHERE model = 'legacy' AND old_prompt = 1 AND old_completion = 2 AND new_prompt = 10 AND new_completion = 20 AND changed_by = 7 AND created_at = 30`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("preserved audit rows = %d, want 1", audits)
	}

	cap := int64(ledger.MaxRatePerMillion)
	if _, err := d.db.ExecContext(ctx, `INSERT INTO prices (model, prompt_per_million, completion_per_million, updated_at) VALUES ('cap', ?, ?, 1)`, cap, cap); err != nil {
		t.Fatalf("price at rate cap rejected: %v", err)
	}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO prices (model, prompt_per_million, completion_per_million, updated_at) VALUES ('over', ?, 0, 1)`, cap+1); err == nil {
		t.Fatal("price above rate cap was accepted")
	}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO price_changes (model, old_prompt, old_completion, new_prompt, new_completion, changed_by, created_at) VALUES ('cap', ?, ?, ?, ?, 1, 1)`, cap, cap, cap, cap); err != nil {
		t.Fatalf("audit at rate cap rejected: %v", err)
	}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO price_changes (model, old_prompt, old_completion, new_prompt, new_completion, changed_by, created_at) VALUES ('over', 0, 0, ?, 0, 1, 1)`, cap+1); err == nil {
		t.Fatal("audit value above rate cap was accepted")
	}

	if _, err := d.SetPriceAudited(ctx, ModelPrice{Model: "audited", PromptPerMillion: ledger.MaxRatePerMillion, CompletionPerMillion: ledger.MaxRatePerMillion}, 9); err != nil {
		t.Fatalf("audited price at rate cap rejected: %v", err)
	}
}
