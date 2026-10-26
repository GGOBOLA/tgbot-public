package ledger

// Rates is a model's resale price in µsen per million tokens.
type Rates struct {
	PromptPerMillion     Amount
	CompletionPerMillion Amount
}

// MaxRatePerMillion caps a price at RM 10,000 per million tokens. Above that
// the cost arithmetic is in overflow territory and no real model is close.
const MaxRatePerMillion Amount = 10_000 * UsecPerMYR

// LowBalanceThreshold is the balance (RM 2.00) that triggers a warning.
const LowBalanceThreshold Amount = 2 * UsecPerMYR

// Usage is the token classes one call reports.
type Usage struct {
	Prompt       int64
	Completion   int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
}

const maxInt64 = int64(^uint64(0) >> 1)

// Anthropic prompt-caching multipliers applied to the input price.
const (
	cacheReadNum, cacheReadDen       = 1, 10 // 0.1x
	cacheWrite5mNum, cacheWrite5mDen = 5, 4  // 1.25x, 5-minute TTL
	cacheWrite1hNum, cacheWrite1hDen = 2, 1  // 2x, 1-hour TTL
)

// Cost prices one call in µsen, rounding each component up so a billable call
// is never free. An absurd input saturates instead of wrapping negative, which
// would credit the customer instead of charging them.
func Cost(r Rates, u Usage) Amount {
	return ceilPerMillion(u.Prompt, r.PromptPerMillion) +
		ceilPerMillion(u.Completion, r.CompletionPerMillion) +
		ceilScaled(u.CacheRead, r.PromptPerMillion, cacheReadNum, cacheReadDen) +
		ceilScaled(u.CacheWrite5m, r.PromptPerMillion, cacheWrite5mNum, cacheWrite5mDen) +
		ceilScaled(u.CacheWrite1h, r.PromptPerMillion, cacheWrite1hNum, cacheWrite1hDen)
}

// ceilPerMillion is tokens × µsen_per_1M / 1e6, rounded up.
func ceilPerMillion(tokens int64, rate Amount) Amount {
	if tokens <= 0 || rate <= 0 {
		return 0
	}
	if tokens > (maxInt64-999_999)/int64(rate) {
		return Amount(maxInt64 / 1_000_000)
	}
	return Amount((tokens*int64(rate) + 999_999) / 1_000_000)
}

// ceilScaled is tokens × rate × num / (den × 1e6), rounded up, saturating.
func ceilScaled(tokens int64, rate Amount, num, den int64) Amount {
	if tokens <= 0 || rate <= 0 || num <= 0 || den <= 0 {
		return 0
	}
	n := mulSat(tokens, int64(rate))
	n = mulSat(n, num)
	d := 1_000_000 * den
	// n + d - 1 overflows for a saturated n; the naive expression then wraps
	// negative and the customer is credited.
	if n > maxInt64-(d-1) {
		return Amount(maxInt64 / d)
	}
	return Amount((n + d - 1) / d)
}

// mulSat multiplies, saturating at MaxInt64.
func mulSat(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > maxInt64/b {
		return maxInt64
	}
	return a * b
}
