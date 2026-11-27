package config

// Prices are exact at config load (v0.2.0): more than 6 decimal places, or out of range, is refused
// naming the price; exact prices (strings or JSON numbers, trailing zeros allowed) load. Same rule as
// the Python runtime's config schema and billing.MoneyFromDecimal.

import (
	"strings"
	"testing"
)

func pricingDoc(price string) []byte {
	return []byte(`{"products":{"vm":{"variants":{"s":{"price_per_hour":` + price + `,"allocation_price":"0"}}}}}`)
}

func TestValidatePricingPrices_RefusesInexact_20261123(t *testing.T) {
	for _, p := range []string{`"0.1234567"`, `0.1234567`, `0.30000000000000004`, `"9007199254.740992"`} {
		err := validatePricingPrices(pricingDoc(p))
		if err == nil || !strings.Contains(err.Error(), "pricing.products.vm.variants.s.price_per_hour") {
			t.Errorf("price %s: err = %v; want a refusal naming the price", p, err)
		}
	}
}

func TestValidatePricingPrices_Control_ExactLoads_20261123(t *testing.T) {
	for _, p := range []string{`"0.123456"`, `0.123456`, `"0.1234560"`, `2`, `"9007199254.740991"`, `"not-a-number"`, `null`} {
		if err := validatePricingPrices(pricingDoc(p)); err != nil {
			t.Errorf("price %s refused: %v", p, err)
		}
	}
	if err := validatePricingPrices(nil); err != nil {
		t.Errorf("absent pricing: %v", err)
	}
}
