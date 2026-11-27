package config

// pricing.products.<product>.funding_sources (v0.2.0) — refused at config load when invalid, like
// the Python runtime's config schema: a typo must never silently widen what may pay for a hold.
// The same rule is applied again by meter.ParsePricing / billing.ValidateFundingSources (this
// package cannot import billing); keep the bucket list identical to billing.Funding*.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// fundingBuckets are billing's money buckets (billing.FundingBalance / FundingSubscriptionCredit /
// FundingCreditBalance).
var fundingBuckets = []string{"balance", "subscription_credit", "credit_balance"}

func validatePricingFundingSources(raw json.RawMessage) error {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return nil
	}
	var doc struct {
		Products map[string]map[string]json.RawMessage `json:"products"`
	}
	if err := json.Unmarshal(t, &doc); err != nil {
		return nil // the shape is meter.ParsePricing's to judge; only funding_sources is checked here
	}
	ids := make([]string, 0, len(doc.Products))
	for id := range doc.Products {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fs, ok := doc.Products[id]["funding_sources"]
		if !ok || string(bytes.TrimSpace(fs)) == "null" {
			continue
		}
		where := "pricing.products." + id + ".funding_sources"
		var list []string
		if err := json.Unmarshal(fs, &list); err != nil {
			return fmt.Errorf("config: %s must be a list of %v", where, fundingBuckets)
		}
		if len(list) == 0 {
			return fmt.Errorf("config: %s is empty — nothing could pay; omit it to allow every bucket %v", where, fundingBuckets)
		}
		seen := map[string]bool{}
		for _, s := range list {
			known := false
			for _, b := range fundingBuckets {
				known = known || s == b
			}
			if !known {
				return fmt.Errorf("config: %s contains %q; allowed: %v", where, s, fundingBuckets)
			}
			if seen[s] {
				return fmt.Errorf("config: %s lists %q twice", where, s)
			}
			seen[s] = true
		}
	}
	return nil
}
