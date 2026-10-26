package ledger

import "testing"

func TestCost(t *testing.T) {
	r := Rates{PromptPerMillion: 100 * UsecPerMYR, CompletionPerMillion: 200 * UsecPerMYR}

	got := Cost(r, Usage{Prompt: 1000, Completion: 500})
	// 1000 × 100 RM/1M = 0.1 RM = 10_000_000 µsen
	// 500  × 200 RM/1M = 0.1 RM = 10_000_000 µsen
	want := Amount(20_000_000)
	if got != want {
		t.Fatalf("cost = %d, want %d", got, want)
	}

	if zero := Cost(r, Usage{}); zero != 0 {
		t.Fatalf("empty usage cost = %d, want 0", zero)
	}
}

// Ceiling vs truncation is only visible when a component does not divide
// exactly: every other vector in this file lands on a whole µsen, so a
// truncating cost would pass them all.
func TestCostCeilsNonDivisibleComponents(t *testing.T) {
	r := Rates{
		PromptPerMillion:     7_500_000,      // RM 0.075 / 1M tokens = 7.5 µsen per token
		CompletionPerMillion: 2 * UsecPerMYR, // RM 2.00 / 1M tokens = 200 µsen per token
	}

	// Prompt: 3 × 7_500_000 = 22_500_000, /1_000_000 = 22.5 µsen
	//   → ceiling 23 (truncation would bill 22).
	// Completion: 1 × 200_000_000 = 200_000_000, /1_000_000 = 200 µsen
	//   → exact, 200 either way.
	// Total: 23 + 200 = 223 µsen (truncation would total 222).
	got := Cost(r, Usage{Prompt: 3, Completion: 1})
	if want := Amount(223); got != want {
		t.Fatalf("cost = %d, want %d: a 22.5 µsen component must round up, not truncate", got, want)
	}
}

func TestCostCacheMultipliers(t *testing.T) {
	r := Rates{PromptPerMillion: 100 * UsecPerMYR}

	read := Cost(r, Usage{CacheRead: 1000})
	write5m := Cost(r, Usage{CacheWrite5m: 1000})
	write1h := Cost(r, Usage{CacheWrite1h: 1000})

	if read != 1_000_000 {
		t.Fatalf("cache read = %d, want 1_000_000 (0.1x of 10_000_000)", read)
	}
	if write5m != 12_500_000 {
		t.Fatalf("cache write 5m = %d, want 12_500_000 (1.25x)", write5m)
	}
	if write1h != 20_000_000 {
		t.Fatalf("cache write 1h = %d, want 20_000_000 (2x)", write1h)
	}
}

func TestCostSaturatesInsteadOfGoingNegative(t *testing.T) {
	r := Rates{PromptPerMillion: MaxRatePerMillion, CompletionPerMillion: MaxRatePerMillion}
	got := Cost(r, Usage{
		Prompt:       maxInt64,
		Completion:   maxInt64,
		CacheRead:    maxInt64,
		CacheWrite5m: maxInt64,
		CacheWrite1h: maxInt64,
	})
	if got <= 0 {
		t.Fatalf("saturated cost = %d, must stay positive", got)
	}
}

func TestCostIgnoredForNonPositiveInputs(t *testing.T) {
	r := Rates{PromptPerMillion: 100 * UsecPerMYR, CompletionPerMillion: 100 * UsecPerMYR}
	if got := Cost(r, Usage{Prompt: -5, Completion: -1}); got != 0 {
		t.Fatalf("negative tokens cost = %d, want 0", got)
	}
	if got := Cost(Rates{}, Usage{Prompt: 1000}); got != 0 {
		t.Fatalf("zero rate cost = %d, want 0", got)
	}
}

func FuzzCost(f *testing.F) {
	f.Add(int64(1000), int64(500), int64(0), int64(0))
	f.Add(int64(maxInt64), int64(maxInt64), int64(maxInt64), int64(maxInt64))
	f.Fuzz(func(t *testing.T, p, c, cr, cw int64) {
		got := Cost(Rates{PromptPerMillion: MaxRatePerMillion, CompletionPerMillion: MaxRatePerMillion},
			Usage{Prompt: p, Completion: c, CacheRead: cr, CacheWrite5m: cw})
		if got < 0 {
			t.Fatalf("negative cost %d from p=%d c=%d cr=%d cw=%d", got, p, c, cr, cw)
		}
	})
}
