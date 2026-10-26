// Package ledger holds the money rules as pure functions: no database, no
// HTTP, no Telegram. Every amount is an integer count of micro-sen (µsen),
// where 1 sen = 1,000,000 µsen and 1 MYR = 100,000,000 µsen.
package ledger

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Amount is money in micro-sen. It is an integer so sub-sen usage costs stay
// exact instead of rounding away.
type Amount int64

const (
	// UsecPerSen is one sen in micro-sen.
	UsecPerSen = 1_000_000
	// UsecPerMYR is one ringgit in micro-sen.
	UsecPerMYR = 100_000_000
	// MaxAmount bounds accepted values so arithmetic stays inside int64.
	MaxAmount Amount = math.MaxInt64 / 4
	// MinAmount is the most negative balance a debt may reach.
	MinAmount Amount = -MaxAmount
)

var (
	// ErrEmpty is returned for an empty amount string.
	ErrEmpty = errors.New("amount is empty")
	// ErrNegative is returned for a negative amount.
	ErrNegative = errors.New("amount must not be negative")
	// ErrTooLarge is returned when an amount exceeds MaxAmount.
	ErrTooLarge = errors.New("amount is too large")
	// ErrPrecision is returned for more than eight decimal places.
	ErrPrecision = errors.New("amount has more than 8 decimal places")
	// ErrFormat is returned for anything that is not a decimal number.
	ErrFormat = errors.New("amount is not a decimal number")
	// ErrOverflow is returned when arithmetic would leave the money range.
	ErrOverflow = errors.New("amount arithmetic overflow")
)

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ParseAmount reads a human decimal ("10", "25.50", "RM 0.000001") into µsen.
// It rejects negatives, non-decimals and anything beyond eight decimal places.
func ParseAmount(s string) (Amount, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrEmpty
	}
	if len(s) >= 2 && strings.EqualFold(s[:2], "rm") {
		s = strings.TrimSpace(s[2:])
		// "RM" on its own is not zero: stripping the prefix must not turn an
		// empty amount into a valid one, or a price of RM 0.00 is one typo away.
		if s == "" {
			return 0, ErrEmpty
		}
	}
	if strings.HasPrefix(s, "-") {
		return 0, ErrNegative
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if !allDigits(whole) {
		return 0, ErrFormat
	}
	if hasDot {
		if frac == "" || !allDigits(frac) {
			return 0, ErrFormat
		}
		if len(frac) > 8 {
			return 0, ErrPrecision
		}
	}
	for len(frac) < 8 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, ErrTooLarge
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, ErrTooLarge
	}
	if w > int64(MaxAmount)/UsecPerMYR {
		return 0, ErrTooLarge
	}
	a := Amount(w)*UsecPerMYR + Amount(f)
	if a < 0 || a > MaxAmount {
		return 0, ErrTooLarge
	}
	return a, nil
}

// Add returns a+b, refusing to leave the money range.
func (a Amount) Add(b Amount) (Amount, error) {
	if b > 0 && a > MaxAmount-b {
		return 0, ErrOverflow
	}
	if b < 0 && a < MinAmount-b {
		return 0, ErrOverflow
	}
	return a + b, nil
}

// Charge debits cost from balance, allowing a visible debt rather than
// dropping a served call. It refuses a negative cost.
func Charge(balance, cost Amount) (Amount, error) {
	if cost < 0 {
		return 0, ErrNegative
	}
	return balance.Add(-cost)
}

// FormatMYR renders an amount the way the dashboard shows it, keeping full
// precision below one sen.
func FormatMYR(a Amount) string {
	myr := int64(a) / UsecPerMYR
	rem := int64(a) % UsecPerMYR
	neg := a < 0
	if neg {
		// Negate the parts, never the total: -MinAmount is not representable,
		// but the quotient and remainder each are.
		myr, rem = -myr, -rem
	}
	// The fraction is µsen: eight decimal places, trailing zeros trimmed but
	// never below the two a customer expects on money (RM 9.999979, RM 1.00).
	digits := strings.TrimRight(fmt.Sprintf("%08d", rem), "0")
	if len(digits) < 2 {
		digits += strings.Repeat("0", 2-len(digits))
	}
	s := fmt.Sprintf("RM %d.%s", myr, digits)
	if neg {
		return "-" + s
	}
	return s
}
