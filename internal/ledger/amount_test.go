package ledger

import (
	"errors"
	"testing"
)

func TestParseAmount(t *testing.T) {
	ok := []struct {
		in   string
		want Amount
	}{
		{"10", 10 * UsecPerMYR},
		{"25.50", 25*UsecPerMYR + 50*UsecPerSen},
		{"0.000001", 100},
		{"0.00000001", 1},
		{"rm 10", 10 * UsecPerMYR},
		{" 7 ", 7 * UsecPerMYR},
		{"0", 0},
	}
	for _, c := range ok {
		got, err := ParseAmount(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q = %d, want %d", c.in, got, c.want)
		}
	}

	// A bare currency prefix must not parse as zero: stripping "RM" from "RM"
	// leaves nothing, and the empty string is an error, not RM 0.00.
	for _, in := range []string{"RM", "RM ", "rm"} {
		if got, err := ParseAmount(in); !errors.Is(err, ErrEmpty) {
			t.Fatalf("ParseAmount(%q) = %v, %v; want ErrEmpty", in, got, err)
		}
	}

	bad := []struct {
		in   string
		want error
	}{
		{"", ErrEmpty},
		{"-1", ErrNegative},
		{"abc", ErrFormat},
		{"1.2.3", ErrFormat},
		{"0.123456789", ErrPrecision},
		{"1e9", ErrFormat},
	}
	for _, c := range bad {
		if _, err := ParseAmount(c.in); !errors.Is(err, c.want) {
			t.Fatalf("%q err = %v, want %v", c.in, err, c.want)
		}
	}
}

func TestChargeKeepsDebt(t *testing.T) {
	bal, _ := ParseAmount("1")
	cost, _ := ParseAmount("2")
	got, err := Charge(bal, cost)
	if err != nil {
		t.Fatal(err)
	}
	if got != -UsecPerMYR {
		t.Fatalf("got %d, want %d", got, -UsecPerMYR)
	}
}

func TestAddOverflow(t *testing.T) {
	if _, err := MaxAmount.Add(1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("MaxAmount.Add(1) err = %v", err)
	}
	if _, err := MinAmount.Add(-1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("MinAmount.Add(-1) err = %v", err)
	}
}

func FuzzParseAmount(f *testing.F) {
	for _, s := range []string{"0", "10", "25.50", "-1", "abc", "1.2.3"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a, err := ParseAmount(s)
		if err != nil {
			return
		}
		if a < 0 || a > MaxAmount {
			t.Fatalf("out of range: %d from %q", a, s)
		}
	})
}
