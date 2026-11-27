package config

// Edge cases (v0.2.0): a price with an absurd exponent in quota-config.json is refused at load, QUICKLY.
// Before, the range compare made shopspring rescale by 10^exponent ("1e999999999" — effectively a
// hang at startup). Control: ordinary prices still load.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestValidatePricingPrices_AbsurdExponents_EQ1(t *testing.T) {
	for price, want := range map[string]string{
		`"1e999999999"`:       "out of range",   // RED: hang
		`1e999999999`:         "out of range",   // RED: hang (a JSON number's exact text)
		`"1e-999999999"`:      "decimal places", // RED: hang
		`"0e999999999"`:       "",               // a zero (positivity is meter.ParsePricing's to judge)
		`"1e3"`:               "",               // control: 1000 is a price
		`"0.000001"`:          "",               // control
		`"0.0000001"`:         "decimal places", // control
		`"9007199254.740992"`: "out of range",   // control
	} {
		raw := json.RawMessage(`{"products":{"vm":{"variants":{"s":{"price_per_hour":` + price + `}}}}}`)
		var err error
		done := make(chan struct{})
		go func() { err = validatePricingPrices(raw); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("price %s: validation did not return within 2s", price)
		}
		switch {
		case want == "" && err != nil:
			t.Errorf("price %s refused: %v", price, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("price %s: %v; want an error containing %q", price, err, want)
		}
	}
}
