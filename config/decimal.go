// Package config implements the JSON schema for quota-config.json.
//
// The loader (LoadConfig) accepts unknown top-level keys (forward-compat
// with the Python lib's evolving schema) and expands ${QUOTA_*} env refs
// in string values. See PRODUCT_SPEC.md §13.6 for the policy.
package config

import (
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"
)

// Decimal wraps shopspring/decimal so JSON values can be string ("10.00"),
// number (10 or 10.5), or null. Matches the Python lib's pydantic Decimal
// acceptance pattern.
type Decimal struct {
	decimal.Decimal
}

// NewDecimal returns a Decimal parsed from a string. Panics on bad input —
// for runtime/JSON values use UnmarshalJSON.
func NewDecimal(s string) Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(fmt.Sprintf("config.NewDecimal(%q): %v", s, err))
	}
	return Decimal{d}
}

// MarshalJSON emits the decimal as a JSON string (matches Python's
// `str(Decimal(...))` round-trip behavior).
func (d Decimal) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.Decimal.String() + `"`), nil
}

// UnmarshalJSON accepts:
//   - JSON string: "10.00", "10", "0.5"
//   - JSON number: 10, 10.5, 0.5, 1e-3 — exact, never through float64 (v0.1.11)
//   - null: leaves the Decimal as zero
func (d *Decimal) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		d.Decimal = decimal.Zero
		return nil
	}
	// String form
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := decimal.NewFromString(s)
		if err != nil {
			return fmt.Errorf("config.Decimal: %w", err)
		}
		d.Decimal = v
		return nil
	}
	// Number form. v0.1.11 (ticket 20261016 test hardening, Q-4): parsed EXACTLY
	// from the literal, like the string form. v0.1.10 went through float64, so
	// 9007199254740993 read as 9007199254740992 and 0.123456789012345678901 as
	// 0.12345678901234568 — binary-float error in a money value.
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("config.Decimal: %w", err)
	}
	v, err := decimal.NewFromString(n.String())
	if err != nil {
		return fmt.Errorf("config.Decimal: %w", err)
	}
	d.Decimal = v
	return nil
}

// IsZero reports whether the Decimal is exactly zero.
func (d Decimal) IsZero() bool { return d.Decimal.IsZero() }

// Float64 returns the decimal as a float64; precision lost beyond ~15 digits.
func (d Decimal) Float64() float64 {
	f, _ := d.Decimal.Float64()
	return f
}
