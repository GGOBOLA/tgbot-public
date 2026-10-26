package gateway

import (
	"database/sql"
	"io"
	"net/http"
	"testing"

	_ "modernc.org/sqlite" // the same driver the store links
)

// usageRow is one raw usage row. The store exposes aggregates only, so these
// tests read the table through a second connection to the same file: the store
// runs WAL, which allows a reader while its single writer is idle.
type usageRow struct {
	model     string
	cost      int64
	estimated int64
}

func (h *harness) usageRows(t *testing.T) []usageRow {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open usage rows: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT model, cost, estimated FROM usage ORDER BY id`)
	if err != nil {
		t.Fatalf("read usage rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []usageRow
	for rows.Next() {
		var r usageRow
		if err := rows.Scan(&r.model, &r.cost, &r.estimated); err != nil {
			t.Fatalf("scan usage row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read usage rows: %v", err)
	}
	return out
}

// A served call that reported no usage is billed at the pre-flight hold, which
// is a guess about tokens nobody reported. The row must say so: reconcile drops
// marked rows from both sides of its check, so a known gap cannot be counted as
// real drift (finding M3).
func TestMissingUsageMarksTheChargeEstimated(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})

	rec := h.do(t, `{"model":"test/model","messages":[{"role":"user","content":"hi"}]}`, testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	rows := h.usageRows(t)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	if rows[0].cost == 0 {
		t.Fatal("usage cost = 0: the pre-flight hold was not charged, so this test proves nothing")
	}
	if rows[0].estimated != 1 {
		t.Fatalf("estimated = %d for a hold charge, want 1: an estimate is being recorded as a real price",
			rows[0].estimated)
	}
}

// A call that reports its own usage is priced from the response, so its row is
// not an estimate and reconcile must keep counting it.
func TestReportedUsageIsNotEstimated(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1000,"completion_tokens":500}}`)
	})

	rec := h.do(t, `{"model":"test/model","messages":[{"role":"user","content":"hi"}]}`, testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	rows := h.usageRows(t)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	if rows[0].cost == 0 {
		t.Fatal("usage cost = 0: the call was not priced, so this test proves nothing")
	}
	if rows[0].estimated != 0 {
		t.Fatalf("estimated = %d for priced usage, want 0", rows[0].estimated)
	}
}
