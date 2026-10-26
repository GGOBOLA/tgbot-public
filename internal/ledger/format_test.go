package ledger

import "testing"

func TestFormatMYR(t *testing.T) {
	cases := []struct {
		in   Amount
		want string
	}{
		{10 * UsecPerMYR, "RM 10.00"},
		{81, "RM 0.00000081"},
		{-UsecPerMYR, "-RM 1.00"},
		// Trailing zeros are noise in a wallet: 999,997,900 µsen is RM 9.999979,
		// not RM 9.99997900. Two decimals stay even when the fraction is exact.
		{999_997_900, "RM 9.999979"},
		{9_999_979_000, "RM 99.99979"},
		{UsecPerMYR / 2, "RM 0.50"},
		{UsecPerSen / 2, "RM 0.005"},
		{0, "RM 0.00"},
		{-81, "-RM 0.00000081"},
		{MaxAmount, "RM 23058430092.13693951"},
	}
	for _, c := range cases {
		if got := FormatMYR(c.in); got != c.want {
			t.Fatalf("%d = %q, want %q", c.in, got, c.want)
		}
	}
}

// Parsing and formatting must round-trip, or a displayed balance cannot be
// typed back in.
func TestFormatParseRoundTrip(t *testing.T) {
	for _, v := range []Amount{1, 81, UsecPerSen, UsecPerMYR, 999_997_900, 9_999_979_000} {
		s := FormatMYR(v)
		back, err := ParseAmount(s[len("RM "):])
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if back != v {
			t.Fatalf("round trip %d -> %q -> %d", v, s, back)
		}
	}
}
