package store

import (
	"context"
	"database/sql"
	"fmt"
)

// schemaVersion is the on-disk shape of a fresh tgbot database. Version 2 adds
// usage.estimated, which marks a charge taken from the pre-flight hold because
// the upstream reported no usage: such a row holds an estimate, not a price.
// Version 3 adds price_changes, the audit trail for prices edited from the bot:
// a price that moves money for every later request must be traceable to a
// person. Version 4 rebuilds usage and claims so their money columns carry the
// same upper bound as customers.balance and ledger.balance_after: before it, a
// cost or a claim could be any int64 the code passed. A database written by an
// earlier version is upgraded in place by schemaSteps; there is still
// deliberately no path from the pre-rebuild schema. Version 7 makes the ledger
// append-only and prevents deleting a customer with financial history. Version
// 8 stores the last handled Telegram update id. Version 9 bounds stored prices
// and price audit values at the same ceiling used by the ledger.
const schemaVersion = 9

// maxMoney mirrors ledger.MaxAmount. It is duplicated as a literal because
// CHECK constraints cannot reference Go values at runtime.
const maxMoney = 2305843009213693951

// maxRatePerMillion mirrors ledger.MaxRatePerMillion.
const maxRatePerMillion = 1000000000000

// schemaV1 is applied to an empty database in one transaction. Money columns
// are integer µsen; the CHECK constraints are the second line of defence behind
// the ledger package, and the partial unique indexes are the idempotency guard.
var schemaV1 = []string{
	fmt.Sprintf(`CREATE TABLE IF NOT EXISTS customers (
		telegram_id INTEGER PRIMARY KEY,
		created_at  INTEGER NOT NULL,
		balance     INTEGER NOT NULL DEFAULT 0
		            CHECK (balance >= -%d AND balance <= %d)
	)`, maxMoney, maxMoney),

	`CREATE TABLE IF NOT EXISTS api_keys (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		telegram_id INTEGER NOT NULL REFERENCES customers(telegram_id) ON DELETE CASCADE,
		name        TEXT    NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
		prefix      TEXT    NOT NULL,
		hash        BLOB    NOT NULL UNIQUE,
		active      INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
		created_at  INTEGER NOT NULL,
		revoked_at  INTEGER
	)`,

	`CREATE INDEX IF NOT EXISTS idx_api_keys_owner ON api_keys(telegram_id, active)`,

	fmt.Sprintf(`CREATE TABLE IF NOT EXISTS ledger (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		telegram_id   INTEGER NOT NULL REFERENCES customers(telegram_id) ON DELETE CASCADE,
		delta         INTEGER NOT NULL CHECK (delta <> 0),
		balance_after INTEGER NOT NULL CHECK (balance_after >= -%d AND balance_after <= %d),
		reason        TEXT    NOT NULL CHECK (length(reason) > 0),
		ref           TEXT    NOT NULL DEFAULT '',
		created_at    INTEGER NOT NULL
	)`, maxMoney, maxMoney),

	// One ledger row per ref, so replaying a credit or a claim is a no-op
	// instead of a second payment.
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_ledger_ref ON ledger(ref) WHERE ref <> ''`,
	`CREATE INDEX IF NOT EXISTS idx_ledger_owner ON ledger(telegram_id, id)`,
	`CREATE TRIGGER IF NOT EXISTS ledger_no_update
		BEFORE UPDATE ON ledger BEGIN
			SELECT RAISE(ABORT, 'ledger is append-only: updates are refused');
		END`,
	`CREATE TRIGGER IF NOT EXISTS ledger_no_delete
		BEFORE DELETE ON ledger BEGIN
			SELECT RAISE(ABORT, 'ledger is append-only: deletes are refused');
		END`,

	fmt.Sprintf(`CREATE TABLE IF NOT EXISTS usage (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		telegram_id       INTEGER NOT NULL REFERENCES customers(telegram_id) ON DELETE CASCADE,
		key_id            INTEGER REFERENCES api_keys(id) ON DELETE SET NULL,
		model             TEXT    NOT NULL,
		prompt_tokens     INTEGER NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
		completion_tokens INTEGER NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
		cost              INTEGER NOT NULL DEFAULT 0 CHECK (cost >= 0 AND cost <= %d),
		created_at        INTEGER NOT NULL,
		first_ms          INTEGER NOT NULL DEFAULT 0,
		estimated         INTEGER NOT NULL DEFAULT 0 CHECK (estimated IN (0, 1))
	)`, maxMoney),
	`CREATE INDEX IF NOT EXISTS idx_usage_owner ON usage(telegram_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_created ON usage(created_at)`,

	fmt.Sprintf(`CREATE TABLE IF NOT EXISTS claims (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		telegram_id INTEGER NOT NULL REFERENCES customers(telegram_id) ON DELETE CASCADE,
		amount      INTEGER NOT NULL CHECK (amount > 0 AND amount <= %d),
		status      TEXT    NOT NULL DEFAULT 'pending'
		            CHECK (status IN ('pending', 'approved', 'rejected')),
		created_at  INTEGER NOT NULL,
		decided_at  INTEGER,
		decided_by  INTEGER,
		expires_at  INTEGER
	)`, maxMoney),

	// A customer may have at most one open claim, so a double tap cannot open
	// two approvable payments.
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_claims_one_pending ON claims(telegram_id) WHERE status = 'pending'`,
	`CREATE INDEX IF NOT EXISTS idx_claims_status ON claims(status, id)`,

	fmt.Sprintf(`CREATE TABLE IF NOT EXISTS prices (
		model                  TEXT    PRIMARY KEY,
		prompt_per_million     INTEGER NOT NULL DEFAULT 0 CHECK (prompt_per_million >= 0 AND prompt_per_million <= %d),
		completion_per_million INTEGER NOT NULL DEFAULT 0 CHECK (completion_per_million >= 0 AND completion_per_million <= %d),
		updated_at             INTEGER NOT NULL
	)`, maxRatePerMillion, maxRatePerMillion),

	// One row per price edit made from the dashboard. Written in the same
	// transaction as the prices upsert, so a price never moves without a record
	// of who moved it, and a record never exists for a price that did not land.
	fmt.Sprintf(`CREATE TABLE IF NOT EXISTS price_changes (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		model          TEXT    NOT NULL,
		old_prompt     INTEGER NOT NULL CHECK (old_prompt >= 0 AND old_prompt <= %d),
		old_completion INTEGER NOT NULL CHECK (old_completion >= 0 AND old_completion <= %d),
		new_prompt     INTEGER NOT NULL CHECK (new_prompt >= 0 AND new_prompt <= %d),
		new_completion INTEGER NOT NULL CHECK (new_completion >= 0 AND new_completion <= %d),
		changed_by     INTEGER NOT NULL,
		created_at     INTEGER NOT NULL
	)`, maxRatePerMillion, maxRatePerMillion, maxRatePerMillion, maxRatePerMillion),
	`CREATE INDEX IF NOT EXISTS idx_price_changes_model ON price_changes(model, id)`,

	// Upstream failures feed the disaster alert; they are pruned on a timer.
	`CREATE TABLE IF NOT EXISTS upstream_errors (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		path       TEXT    NOT NULL,
		status     INTEGER NOT NULL,
		failures   INTEGER NOT NULL DEFAULT 0 CHECK (failures >= 0),
		recovered  INTEGER NOT NULL DEFAULT 0 CHECK (recovered IN (0, 1)),
		created_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_upstream_errors_created ON upstream_errors(created_at)`,

	`CREATE TABLE IF NOT EXISTS settings (
		key   TEXT PRIMARY KEY,
		value INTEGER NOT NULL
	)`,
}

// schemaStep upgrades a database that was already at version `from` to the next
// version. The end state of every step is also baked into schemaV1, which is why
// a fresh file (user_version 0) runs none of them: it is created at
// schemaVersion, and applying a step would add a column the CREATE already has.
// Add a step whenever schemaVersion is bumped.
type schemaStep struct {
	from  int
	stmts []string
}

var schemaSteps = []schemaStep{
	{
		from: 1,
		stmts: []string{
			// Every row written before v2 came from reported usage, so the
			// default is the honest backfill: only new hold charges get 1.
			`ALTER TABLE usage ADD COLUMN estimated INTEGER NOT NULL DEFAULT 0 CHECK (estimated IN (0, 1))`,
		},
	},
	{
		from: 2,
		stmts: []string{
			// The CREATEs above already ran as no-ops on the existing tables;
			// this step only has to add the audit table a v2 file lacks.
			`CREATE TABLE IF NOT EXISTS price_changes (
				id             INTEGER PRIMARY KEY AUTOINCREMENT,
				model          TEXT    NOT NULL,
				old_prompt     INTEGER NOT NULL CHECK (old_prompt >= 0),
				old_completion INTEGER NOT NULL CHECK (old_completion >= 0),
				new_prompt     INTEGER NOT NULL CHECK (new_prompt >= 0),
				new_completion INTEGER NOT NULL CHECK (new_completion >= 0),
				changed_by     INTEGER NOT NULL,
				created_at     INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_price_changes_model ON price_changes(model, id)`,
		},
	},
	{
		from: 3,
		// SQLite cannot add a CHECK to an existing table, so both money-bearing
		// tables are rebuilt: create with the bound, copy, drop, rename, and
		// recreate the indexes DROP took with it. Nothing references either
		// table, so no foreign key is left pointing at the old name.
		stmts: []string{
			fmt.Sprintf(`CREATE TABLE claims_v4 (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				telegram_id INTEGER NOT NULL REFERENCES customers(telegram_id) ON DELETE CASCADE,
				amount      INTEGER NOT NULL CHECK (amount > 0 AND amount <= %d),
				status      TEXT    NOT NULL DEFAULT 'pending'
				            CHECK (status IN ('pending', 'approved', 'rejected')),
				created_at  INTEGER NOT NULL,
				decided_at  INTEGER,
				decided_by  INTEGER
			)`, maxMoney),
			`INSERT INTO claims_v4 (id, telegram_id, amount, status, created_at, decided_at, decided_by)
			 SELECT id, telegram_id, amount, status, created_at, decided_at, decided_by FROM claims`,
			`DROP TABLE claims`,
			`ALTER TABLE claims_v4 RENAME TO claims`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_claims_one_pending ON claims(telegram_id) WHERE status = 'pending'`,
			`CREATE INDEX IF NOT EXISTS idx_claims_status ON claims(status, id)`,

			fmt.Sprintf(`CREATE TABLE usage_v4 (
				id                INTEGER PRIMARY KEY AUTOINCREMENT,
				telegram_id       INTEGER NOT NULL REFERENCES customers(telegram_id) ON DELETE CASCADE,
				key_id            INTEGER REFERENCES api_keys(id) ON DELETE SET NULL,
				model             TEXT    NOT NULL,
				prompt_tokens     INTEGER NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
				completion_tokens INTEGER NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
				cost              INTEGER NOT NULL DEFAULT 0 CHECK (cost >= 0 AND cost <= %d),
				created_at        INTEGER NOT NULL,
				first_ms          INTEGER NOT NULL DEFAULT 0,
				estimated         INTEGER NOT NULL DEFAULT 0 CHECK (estimated IN (0, 1))
			)`, maxMoney),
			`INSERT INTO usage_v4 (id, telegram_id, key_id, model, prompt_tokens, completion_tokens,
			                      cost, created_at, first_ms, estimated)
			 SELECT id, telegram_id, key_id, model, prompt_tokens, completion_tokens,
			        cost, created_at, first_ms, estimated FROM usage`,
			`DROP TABLE usage`,
			`ALTER TABLE usage_v4 RENAME TO usage`,
			`CREATE INDEX IF NOT EXISTS idx_usage_owner ON usage(telegram_id, created_at)`,
		},
	},
	{
		from: 4,
		stmts: []string{
			`CREATE INDEX IF NOT EXISTS idx_usage_created ON usage(created_at)`,
		},
	},
	{
		from: 5,
		stmts: []string{
			`ALTER TABLE claims ADD COLUMN expires_at INTEGER`,
		},
	},
	{
		from: 6,
		// Rebuild ledger to change its customer FK without changing its stored
		// rows or constraints. Dropping the old table also drops its triggers.
		stmts: []string{
			fmt.Sprintf(`CREATE TABLE ledger_new (
				id            INTEGER PRIMARY KEY AUTOINCREMENT,
				telegram_id   INTEGER NOT NULL REFERENCES customers(telegram_id) ON DELETE RESTRICT,
				delta         INTEGER NOT NULL CHECK (delta <> 0),
				balance_after INTEGER NOT NULL CHECK (balance_after >= -%d AND balance_after <= %d),
				reason        TEXT    NOT NULL CHECK (length(reason) > 0),
				ref           TEXT    NOT NULL DEFAULT '',
				created_at    INTEGER NOT NULL
			)`, maxMoney, maxMoney),
			`INSERT INTO ledger_new (id, telegram_id, delta, balance_after, reason, ref, created_at)
			 SELECT id, telegram_id, delta, balance_after, reason, ref, created_at FROM ledger`,
			`DROP TABLE ledger`,
			`ALTER TABLE ledger_new RENAME TO ledger`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_ledger_ref ON ledger(ref) WHERE ref <> ''`,
			`CREATE INDEX IF NOT EXISTS idx_ledger_owner ON ledger(telegram_id, id)`,
			`CREATE TRIGGER IF NOT EXISTS ledger_no_update
				BEFORE UPDATE ON ledger BEGIN
					SELECT RAISE(ABORT, 'ledger is append-only: updates are refused');
				END`,
			`CREATE TRIGGER IF NOT EXISTS ledger_no_delete
				BEFORE DELETE ON ledger BEGIN
					SELECT RAISE(ABORT, 'ledger is append-only: deletes are refused');
				END`,
		},
	},
	{
		from: 7,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS settings (
				key   TEXT PRIMARY KEY,
				value INTEGER NOT NULL
			)`,
		},
	},
	{
		from: 8,
		// Rebuild both price tables to enforce the ledger's rate ceiling at the
		// database boundary. Keep all stored values and recreate the audit index.
		stmts: []string{
			fmt.Sprintf(`CREATE TABLE prices_v9 (
				model                  TEXT    PRIMARY KEY,
				prompt_per_million     INTEGER NOT NULL DEFAULT 0 CHECK (prompt_per_million >= 0 AND prompt_per_million <= %d),
				completion_per_million INTEGER NOT NULL DEFAULT 0 CHECK (completion_per_million >= 0 AND completion_per_million <= %d),
				updated_at             INTEGER NOT NULL
			)`, maxRatePerMillion, maxRatePerMillion),
			`INSERT INTO prices_v9 (model, prompt_per_million, completion_per_million, updated_at)
			 SELECT model, prompt_per_million, completion_per_million, updated_at FROM prices`,
			`DROP TABLE prices`,
			`ALTER TABLE prices_v9 RENAME TO prices`,
			fmt.Sprintf(`CREATE TABLE price_changes_v9 (
				id             INTEGER PRIMARY KEY AUTOINCREMENT,
				model          TEXT    NOT NULL,
				old_prompt     INTEGER NOT NULL CHECK (old_prompt >= 0 AND old_prompt <= %d),
				old_completion INTEGER NOT NULL CHECK (old_completion >= 0 AND old_completion <= %d),
				new_prompt     INTEGER NOT NULL CHECK (new_prompt >= 0 AND new_prompt <= %d),
				new_completion INTEGER NOT NULL CHECK (new_completion >= 0 AND new_completion <= %d),
				changed_by     INTEGER NOT NULL,
				created_at     INTEGER NOT NULL
			)`, maxRatePerMillion, maxRatePerMillion, maxRatePerMillion, maxRatePerMillion),
			`INSERT INTO price_changes_v9 (id, model, old_prompt, old_completion, new_prompt, new_completion, changed_by, created_at)
			 SELECT id, model, old_prompt, old_completion, new_prompt, new_completion, changed_by, created_at FROM price_changes`,
			`DROP TABLE price_changes`,
			`ALTER TABLE price_changes_v9 RENAME TO price_changes`,
			`CREATE INDEX IF NOT EXISTS idx_price_changes_model ON price_changes(model, id)`,
		},
	},
}

// applySchema brings the file up to schemaVersion. A fresh database gets the
// current shape from schemaV1; an older one gets the CREATEs (all IF NOT EXISTS,
// so they are no-ops on tables that exist) plus every migration step above its
// recorded version, and the new version is recorded in the same transaction. A
// failed step rolls back with the version, so a retry re-runs it cleanly.
// It refuses a database written by a newer build.
func applySchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema v%d is newer than this build (v%d)", version, schemaVersion)
	}
	if version == schemaVersion {
		return nil
	}

	for _, stmt := range schemaV1 {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
	}
	for _, step := range schemaSteps {
		// version == 0 is a fresh file, which already has every step's end
		// state; step.from < version means that step was applied by an earlier
		// start. Only the steps this file has not seen run.
		if version == 0 || step.from < version {
			continue
		}
		for _, stmt := range step.stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migrate schema v%d -> v%d: %w", step.from, step.from+1, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema: %w", err)
	}
	return nil
}
