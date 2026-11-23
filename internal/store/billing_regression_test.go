package store

import (
	"context"
	"fmt"
	"testing"

	"tgbot/internal/ledger"
)

// The reference is what lets the reconciliation join an estimated charge to
// exactly one ledger row instead of guessing from equal cost/timestamp values.
func TestChargeUsageLinksEachUsageRowToItsLedgerRow(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 42); err != nil {
		t.Fatal(err)
	}
	for _, cost := range []int64{100, 200} {
		if _, err := d.ChargeUsage(ctx, UsageEntry{TelegramID: 42, Model: "m", Cost: ledger.Amount(cost)}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT u.id, l.ref FROM usage u JOIN ledger l ON l.ref = 'usage:' || u.id
		 WHERE l.reason = 'usage' ORDER BY u.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var count int
	for rows.Next() {
		var id int64
		var ref string
		if err := rows.Scan(&id, &ref); err != nil {
			t.Fatal(err)
		}
		if ref != fmt.Sprintf("usage:%d", id) {
			t.Fatalf("ledger ref = %q for usage %d", ref, id)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("linked usage rows = %d, want 2", count)
	}
}
