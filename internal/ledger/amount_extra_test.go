package ledger

import (
	"math"
	"testing"
)

// MinAmount has no positive counterpart, so negating it overflows. The
// formatter is exported and used for display, so it must still print the
// magnitude instead of a wrapped number.
func TestFormatMYRMinAmount(t *testing.T) {
	// MinAmount is the store's floor, not the int64 floor; both must format.
	if got, want := FormatMYR(MinAmount), "-RM 23058430092.13693951"; got != want {
		t.Fatalf("FormatMYR(MinAmount) = %q, want %q", got, want)
	}
	got := FormatMYR(Amount(math.MinInt64))
	want := "-RM 92233720368.54775808"
	if got != want {
		t.Fatalf("FormatMYR(math.MinInt64) = %q, want %q", got, want)
	}
	if got := FormatMYR(-150000000); got != "-RM 1.50" {
		t.Fatalf("negative amount = %q", got)
	}
}
