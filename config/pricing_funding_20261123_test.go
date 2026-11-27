package config

// v0.2.0: pricing.products.<product>.funding_sources is refused at config load when invalid (the
// Python config schema's rule); absent / null / a valid list load as before.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func pricingCfg(pricing string) *Config {
	lim := 1.0
	return &Config{
		Tiers:     []Tier{{TierID: "pro", Limits: map[string]TierLimit{"x": {Limit: &lim}}}},
		Resources: []ResourceDef{{ResourceKey: "x", CounterType: CounterGauge}},
		Pricing:   json.RawMessage(pricing),
	}
}

func TestValidate_PricingFundingSources_20261123(t *testing.T) {
	ok := []string{
		``, `null`,
		`{"products":{"vm":{"variants":{"s":{"price_per_hour":"1","allocation_price":"0"}}}}}`,
		`{"products":{"vm":{"funding_sources":null,"variants":{}}}}`,
		`{"products":{"vm":{"funding_sources":["subscription_credit","balance","credit_balance"],"variants":{}}}}`,
	}
	for _, p := range ok {
		if err := pricingCfg(p).Validate(); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	bad := map[string]string{
		`{"products":{"vm":{"funding_sources":["cash"]}}}`:              "contains",
		`{"products":{"vm":{"funding_sources":[]}}}`:                    "empty",
		`{"products":{"vm":{"funding_sources":["balance","balance"]}}}`: "twice",
		`{"products":{"vm":{"funding_sources":"balance"}}}`:             "must be a list",
	}
	for p, want := range bad {
		err := pricingCfg(p).Validate()
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "pricing.products.vm.funding_sources") {
			t.Errorf("%s: got %v, want an error naming the product and %q", p, err, want)
		}
	}
}

// The bucket list must stay billing's (billing.FundingBalance / FundingSubscriptionCredit /
// FundingCreditBalance; the billing package pins the same literal list).
func TestFundingBuckets_AreBillings_20261123(t *testing.T) {
	if !reflect.DeepEqual(fundingBuckets, []string{"balance", "subscription_credit", "credit_balance"}) {
		t.Fatalf("fundingBuckets = %v", fundingBuckets)
	}
}
