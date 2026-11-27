package billing

// Exact money (v0.2.0, additive): amounts are decimals, never binary floats.
//
// One rule for every amount, the same rule as the Python library (ab0t_quota.money) and the
// billing service:
//
//   - an amount is an exact decimal with at most 6 fractional digits (micro-units, 0.000001);
//   - rounding happens in exactly one kind of place — where a cost is COMPUTED (rate × time, a hold
//     estimate) — and the mode is ROUND_UP (away from zero) to 6 places: a genuine remainder
//     beyond the 6th place adds at most one micro-unit, an exact result is never changed;
//   - no amount is computed with or sent as a float64; JSON carries money as a string.
//
// Money is an int64 count of micro-units with |value| <= MaxMoneyMicros (2^53 - 1, i.e.
// $9,007,199,254.740991 — the range billing computes exactly). It is a plain value: the zero value
// is $0, == compares amounts, and it cannot hold a value that is not exact to the micro-unit.
//
// Choice recorded: an int64 micro-unit value type was chosen over (a) a decimal-string type —
// every operation would re-parse, and "1.5" / "1.500000" would compare unequal with == — and over
// (b) wrapping shopspring/decimal — a pointer-backed big.Int (not comparable with ==, allocates,
// unbounded precision) whose JSON form depends on the package-global MarshalJSONWithoutQuotes.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// MoneyPlaces is the number of fractional digits of every amount (micro-units).
const MoneyPlaces = 6

// MaxMoneyMicros is the largest |amount| in micro-units: 2^53 - 1 ($9,007,199,254.740991).
const MaxMoneyMicros int64 = 1<<53 - 1

const microsPerUnit = 1_000_000

// ErrInvalidMoney is wrapped by every *MoneyError (errors.Is(err, ErrInvalidMoney)).
var ErrInvalidMoney = errors.New("billing: invalid money amount")

// MoneyErrorKind says why an amount was refused.
type MoneyErrorKind string

const (
	// MoneySyntax — not a decimal amount (exponents, separators, NaN, … are refused).
	MoneySyntax MoneyErrorKind = "syntax"
	// MoneyPrecision — a nonzero digit beyond the 6th decimal place.
	MoneyPrecision MoneyErrorKind = "precision"
	// MoneyRange — |amount| above MaxMoneyMicros micro-units.
	MoneyRange MoneyErrorKind = "range"
)

// MoneyError is a refused amount.
type MoneyError struct {
	Kind  MoneyErrorKind
	Input string
	msg   string
}

func (e *MoneyError) Error() string { return "billing: money " + e.Input + ": " + e.msg }

// Unwrap makes errors.Is(err, ErrInvalidMoney) true.
func (e *MoneyError) Unwrap() error { return ErrInvalidMoney }

func moneyErr(kind MoneyErrorKind, input, msg string) error {
	return &MoneyError{Kind: kind, Input: strconv.Quote(input), msg: msg}
}

// Money is an exact amount in micro-units (see the file comment). The zero value is $0.
type Money struct {
	micros int64
}

func inRange(micros int64) bool { return micros >= -MaxMoneyMicros && micros <= MaxMoneyMicros }

// ParseMoney parses an exact amount: an optional sign, digits with an optional fraction (or a bare
// fraction, ".5"), surrounding whitespace ignored. More than 6 decimal places is refused unless
// the extra digits are zeros ("1.0000000" is 1). No exponent, no separators, no NaN / Infinity.
func ParseMoney(s string) (Money, error) {
	t := strings.TrimSpace(s)
	neg := false
	if t != "" && (t[0] == '+' || t[0] == '-') {
		neg = t[0] == '-'
		t = t[1:]
	}
	intPart, frac, _ := strings.Cut(t, ".")
	if (intPart == "" && frac == "") || !asciiDigits(intPart) || !asciiDigits(frac) {
		return Money{}, moneyErr(MoneySyntax, s, "not a decimal amount like 12.345678")
	}
	if len(frac) > MoneyPlaces {
		if strings.Trim(frac[MoneyPlaces:], "0") != "" {
			return Money{}, moneyErr(MoneyPrecision, s, "more than 6 decimal places (money is exact to 0.000001)")
		}
		frac = frac[:MoneyPlaces]
	}
	frac += strings.Repeat("0", MoneyPlaces-len(frac))
	intPart = strings.TrimLeft(intPart, "0")
	if len(intPart) > 10 { // MaxMoneyMicros has a 10-digit whole part; anything longer is out of range
		return Money{}, moneyErr(MoneyRange, s, "out of range (|amount| at most 9007199254.740991)")
	}
	var whole int64
	if intPart != "" {
		w, err := strconv.ParseInt(intPart, 10, 64)
		if err != nil {
			return Money{}, moneyErr(MoneySyntax, s, "not a decimal amount")
		}
		whole = w
	}
	f, _ := strconv.ParseInt(frac, 10, 64) // 6 ASCII digits: cannot fail
	micros := whole*microsPerUnit + f
	if micros > MaxMoneyMicros {
		return Money{}, moneyErr(MoneyRange, s, "out of range (|amount| at most 9007199254.740991)")
	}
	if neg {
		micros = -micros
	}
	return Money{micros: micros}, nil
}

// MustParseMoney is ParseMoney that panics on error — for constants and tests.
func MustParseMoney(s string) Money {
	m, err := ParseMoney(s)
	if err != nil {
		panic(err)
	}
	return m
}

func asciiDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// MoneyFromMicros is the amount of n micro-units; an error (MoneyRange) when |n| > MaxMoneyMicros.
func MoneyFromMicros(n int64) (Money, error) {
	if !inRange(n) {
		return Money{}, moneyErr(MoneyRange, strconv.FormatInt(n, 10)+" micro-units", "out of range")
	}
	return Money{micros: n}, nil
}

// MoneyFromDecimal converts a decimal amount exactly; an error when it has a nonzero digit beyond
// the 6th decimal place (MoneyPrecision) or is out of range (MoneyRange). Never rounds.
func MoneyFromDecimal(d decimal.Decimal) (Money, error) {
	// Edge guards (v0.2.0): decide an absurd representation from the coefficient's digit count and
	// the exponent, never by rescaling — comparing decimal.New(1, 1<<30) with an int64 makes
	// shopspring build 10^(2^30) (effectively a hang). Same rule as the Python library's _micros_of.
	if d.IsZero() {
		return Money{}, nil
	}
	shift := int64(d.Exponent()) + MoneyPlaces
	digits := int64(len(new(big.Int).Abs(d.Coefficient()).String()))
	if shift >= 0 && digits+shift > 17 { // >= 1e17 micro-units > MaxMoneyMicros
		return Money{}, moneyErr(MoneyRange, "(a value with exponent "+strconv.FormatInt(int64(d.Exponent()), 10)+")",
			"out of range (|amount| at most 9007199254.740991)")
	}
	if shift < 0 && -shift > digits { // nonzero and entirely below one micro-unit
		return Money{}, moneyErr(MoneyPrecision, "(a value with exponent "+strconv.FormatInt(int64(d.Exponent()), 10)+")",
			"more than 6 decimal places (money is exact to 0.000001)")
	}
	scaled := d.Shift(MoneyPlaces)
	if !scaled.Equal(scaled.Truncate(0)) {
		return Money{}, moneyErr(MoneyPrecision, d.String(), "more than 6 decimal places (money is exact to 0.000001)")
	}
	if scaled.Abs().GreaterThan(decimal.NewFromInt(MaxMoneyMicros)) {
		return Money{}, moneyErr(MoneyRange, d.String(), "out of range (|amount| at most 9007199254.740991)")
	}
	return Money{micros: scaled.IntPart()}, nil
}

// Micros is the amount in micro-units.
func (m Money) Micros() int64 { return m.micros }

// String is the canonical text: no exponent, no trailing fractional zeros, no "-0" ("0.3",
// "-1.666667", "5"). The same text as the Python library's format_money.
func (m Money) String() string {
	n := m.micros
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n/microsPerUnit, 10)
	if frac := n % microsPerUnit; frac != 0 {
		s += "." + strings.TrimRight(fmt.Sprintf("%06d", frac), "0")
	}
	if neg {
		s = "-" + s
	}
	return s
}

// Decimal is the amount as a shopspring decimal (exact).
func (m Money) Decimal() decimal.Decimal { return decimal.New(m.micros, -MoneyPlaces) }

// IsZero reports whether the amount is $0.
func (m Money) IsZero() bool { return m.micros == 0 }

// Sign is -1, 0 or +1.
func (m Money) Sign() int {
	switch {
	case m.micros < 0:
		return -1
	case m.micros > 0:
		return 1
	}
	return 0
}

// Cmp is -1, 0 or +1 as m is less than, equal to or greater than o.
func (m Money) Cmp(o Money) int {
	switch {
	case m.micros < o.micros:
		return -1
	case m.micros > o.micros:
		return 1
	}
	return 0
}

// Neg is -m (always in range: the range is symmetric).
func (m Money) Neg() Money { return Money{micros: -m.micros} }

// Add is m + o, exact; an error (MoneyRange) when the sum is out of range.
func (m Money) Add(o Money) (Money, error) { return MoneyFromMicros(m.micros + o.micros) }

// Sub is m - o, exact; an error (MoneyRange) when the difference is out of range.
func (m Money) Sub(o Money) (Money, error) { return MoneyFromMicros(m.micros - o.micros) }

// CostForDuration is the cost of running at hourlyRate for d: rate × seconds / 3600 (multiply,
// then divide), rounded ROUND_UP — away from zero — to 6 places, computed in integers (exact; no
// float, no intermediate rounding). A genuine remainder beyond the 6th place adds at most one
// micro-unit; an exact result is never changed. The same rule as the billing service and the
// Python library's cost_for. A negative duration is an error.
//
// Derive, do not accumulate: the running cost at a heartbeat is CostForDuration(rate, now-start),
// not the sum of per-beat costs (each per-beat cost is rounded on its own, so a sum can exceed the
// lifetime cost by up to one micro-unit per beat).
func CostForDuration(hourlyRate Money, d time.Duration) (Money, error) {
	if d < 0 {
		return Money{}, moneyErr(MoneyRange, d.String(), "duration must not be negative")
	}
	num := new(big.Int).Mul(big.NewInt(hourlyRate.micros), big.NewInt(int64(d)))
	q := divRoundUp(num, big.NewInt(int64(time.Hour)))
	if !q.IsInt64() || !inRange(q.Int64()) {
		return Money{}, moneyErr(MoneyRange, hourlyRate.String()+" × "+d.String(), "cost out of range")
	}
	return Money{micros: q.Int64()}, nil
}

// divRoundUp is num / den rounded ROUND_UP (den > 0): any nonzero remainder moves the quotient one
// AWAY from zero (7/2 → 4, -7/2 → -4, 6/2 → 3) — the integer form of Python's
// Decimal.quantize(..., rounding=ROUND_UP).
func divRoundUp(num, den *big.Int) *big.Int {
	abs := new(big.Int).Abs(num)
	q, r := new(big.Int).QuoRem(abs, den, new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if num.Sign() < 0 {
		q.Neg(q)
	}
	return q
}

// MarshalJSON writes the canonical text as a JSON string (never a number).
func (m Money) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(m.String())), nil }

// UnmarshalJSON reads a JSON string or number with ParseMoney's rules (a number's exact text, never
// through float64). null leaves the value unchanged.
func (m *Money) UnmarshalJSON(b []byte) error {
	t := strings.TrimSpace(string(b))
	if t == "null" {
		return nil
	}
	if strings.HasPrefix(t, `"`) {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return moneyErr(MoneySyntax, t, "not a JSON string")
		}
		t = s
	}
	v, err := ParseMoney(t)
	if err != nil {
		return err
	}
	*m = v
	return nil
}
