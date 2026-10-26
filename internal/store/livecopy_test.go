package store

import (
	"os"
	"testing"
)

// Run only with LIVE_DB set to a COPY of a real database: proves the current
// migration keeps every row and every balance on real data, not fixtures.
func TestLiveCopyMigrates(t *testing.T) {
	path := os.Getenv("LIVE_DB")
	if path == "" {
		t.Skip("LIVE_DB not set")
	}
	d, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB on live copy: %v", err)
	}
	defer func() { _ = d.Close() }()

	var version int
	if err := d.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("schema version after migrate: %d", version)

	for _, table := range []string{"customers", "ledger", "usage", "claims", "api_keys", "prices"} {
		var n int
		if err := d.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		t.Logf("%s rows: %d", table, n)
	}

	// Every customer balance must still equal the sum of its ledger deltas.
	rows, err := d.db.Query(`SELECT c.telegram_id, c.balance, COALESCE((SELECT SUM(delta) FROM ledger l WHERE l.telegram_id = c.telegram_id), 0) FROM customers c`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	bad := 0
	for rows.Next() {
		var id, balance, sum int64
		if err := rows.Scan(&id, &balance, &sum); err != nil {
			t.Fatal(err)
		}
		if balance != sum {
			bad++
			t.Logf("DRIFT customer %d: balance=%d ledger_sum=%d", id, balance, sum)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("%d customers drifted during migration", bad)
	}

	// The append-only guard must be live on the migrated file.
	if _, err := d.db.Exec(`DELETE FROM ledger WHERE id = (SELECT min(id) FROM ledger)`); err == nil {
		t.Fatal("DELETE on ledger was allowed after migration")
	} else {
		t.Logf("delete refused as expected: %v", err)
	}
	if _, err := d.db.Exec(`UPDATE ledger SET reason = 'x' WHERE id = (SELECT min(id) FROM ledger)`); err == nil {
		t.Fatal("UPDATE on ledger was allowed after migration")
	} else {
		t.Logf("update refused as expected: %v", err)
	}
}
