package config

// Known-bug tests (2026-10-16) — FIXED in v0.1.11, so they always run now (the
// QUOTA_RUN_KNOWN_BUGS gate is gone). Each FAILED on v0.1.10.
// Ticket: .private/tickets/20261016_test_hardening_findings/TICKET.md (Q-4)

import (
	"encoding/json"
	"testing"
)

// Q-4: config.Decimal parsed a JSON NUMBER through strconv.ParseFloat →
// decimal.NewFromFloat, so a money value written as a number in quota-config.json
// (credit_grant.amount_per_period, a price) lost precision beyond float64.
// A JSON STRING was exact; v0.1.11 makes the number form exact too.
func TestKnownBug_ConfigDecimalNumberGoesThroughFloat(t *testing.T) {
	for _, lit := range []string{"9007199254740993", "0.123456789012345678901", "12345678901234567890.5", "1e-30"} {
		var fromNum, fromStr Decimal
		if err := json.Unmarshal([]byte(lit), &fromNum); err != nil {
			t.Fatalf("%s: %v", lit, err)
		}
		if err := json.Unmarshal([]byte(`"`+lit+`"`), &fromStr); err != nil {
			t.Fatalf("%q: %v", lit, err)
		}
		if !fromNum.Decimal.Equal(fromStr.Decimal) {
			t.Errorf("JSON number %s parsed as %s; the same value as a string is %s", lit, fromNum.Decimal, fromStr.Decimal)
		}
	}
	// 2^53 + 1 — the first integer float64 cannot hold — exactly, digit for digit.
	var d Decimal
	if err := json.Unmarshal([]byte(`9007199254740993`), &d); err != nil {
		t.Fatal(err)
	}
	if d.Decimal.String() != "9007199254740993" {
		t.Fatalf("9007199254740993 decoded as %s", d.Decimal.String())
	}
}

// The input forms that worked before v0.1.11 still decode to the same value, inside
// a real config field; non-numbers are still refused.
func TestConfigDecimalInputForms_20261016(t *testing.T) {
	ok := map[string]string{
		`10`: "10", `10.5`: "10.5", `0.5`: "0.5", `-1.25`: "-1.25", `0`: "0", `-0`: "0",
		`1e2`: "100", `1E+2`: "100", `2.5e-3`: "0.0025",
		`"10.00"`: "10", `"10"`: "10", `"0.5"`: "0.5", `"9007199254740993"`: "9007199254740993",
		`null`: "0",
	}
	for in, want := range ok {
		var g struct {
			Amount Decimal `json:"amount_per_period"`
		}
		if err := json.Unmarshal([]byte(`{"amount_per_period":`+in+`}`), &g); err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if !g.Amount.Decimal.Equal(NewDecimal(want).Decimal) {
			t.Errorf("%s decoded as %s, want %s", in, g.Amount.Decimal, want)
		}
	}
	for _, in := range []string{`true`, `"abc"`, `[]`, `{}`, `"1.2.3"`} {
		var g struct {
			Amount Decimal `json:"amount_per_period"`
		}
		if err := json.Unmarshal([]byte(`{"amount_per_period":`+in+`}`), &g); err == nil {
			t.Errorf("%s was accepted as %s, want an error", in, g.Amount.Decimal)
		}
	}
	// A direct call with something that is not a JSON number is an error, not a panic
	// (v0.1.10 accepted "NaN" here and panicked in decimal.NewFromFloat).
	for _, in := range []string{"NaN", "Inf", "0x10", "1_000"} {
		var d Decimal
		if err := d.UnmarshalJSON([]byte(in)); err == nil {
			t.Errorf("UnmarshalJSON(%s) accepted %s, want an error", in, d.Decimal)
		}
	}
}
