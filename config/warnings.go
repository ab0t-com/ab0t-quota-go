package config

import (
	"encoding/json"
	"fmt"
	"sort"
)

// v0.3.0 — the two defaults the Go library now SHARES with the Python library (both true). A config
// FILE that omits the key gets true; before v0.3.0 the Go library used false for both. Both libraries
// print EXACTLY these texts when a config file omits the key (shared vectors
// testdata/config_default_warnings_golden_20261127.json).
const EnforcementEnabledUnsetWarning = `enforcement.enabled is not set: the default is now true (limits ` +
	`enforced); it was false (EVERY request allowed) in the Go library before v0.3.0 — set it explicitly to ` +
	`keep the old Go behaviour ("enforcement": {"enabled": false}) or to silence this warning ` +
	`("enforcement": {"enabled": true})`

// ResetOnDowngradeUnsetWarning is the text for a tier whose credit_grant omits reset_on_downgrade.
func ResetOnDowngradeUnsetWarning(tierID string) string {
	return `tier "` + tierID + `": credit_grant.reset_on_downgrade is not set: the default is now true (the ` +
		`grant's balance is reset when the org moves to a lower tier); it was false in the Go library before ` +
		`v0.3.0 — set it explicitly to keep the old Go behaviour ("reset_on_downgrade": false) or to silence ` +
		`this warning`
}

// markUnsetDefaults (v0.3.0) applies the two defaults a config FILE leaves unset — enforcement.enabled
// and each credit_grant's reset_on_downgrade become TRUE (Python parity; they were Go's zero value,
// false, before v0.3.0) — and records which were unset, so Warnings names them. It runs only in
// LoadConfig: a Config built in Go code keeps exactly the values it was given.
func markUnsetDefaults(cfg *Config, loose map[string]json.RawMessage) {
	var enf map[string]json.RawMessage
	if raw, ok := loose["enforcement"]; ok {
		_ = json.Unmarshal(raw, &enf)
	}
	if _, ok := enf["enabled"]; !ok {
		cfg.Enforcement.enabledUnset = true
		cfg.Enforcement.Enabled = true
	}
	var tiers []map[string]json.RawMessage
	if raw, ok := loose["tiers"]; ok {
		_ = json.Unmarshal(raw, &tiers)
	}
	for i := range tiers {
		if i >= len(cfg.Tiers) || cfg.Tiers[i].CreditGrant == nil {
			continue
		}
		var grant map[string]json.RawMessage
		raw, ok := tiers[i]["credit_grant"]
		if !ok || json.Unmarshal(raw, &grant) != nil || grant == nil {
			continue
		}
		if _, set := grant["reset_on_downgrade"]; !set {
			cfg.Tiers[i].CreditGrant.resetOnDowngradeUnset = true
			cfg.Tiers[i].CreditGrant.ResetOnDowngrade = true
		}
	}
}

// Warnings (v0.1.10, ticket 20261016 review A-08/A-12/C-03/C-04/C-05) lists the
// config shapes that load and boot but silently mean something the author
// almost certainly did not intend — each one a fail-OPEN on enforcement or on
// money. quota.Setup logs every entry at WARN and publishes them as
// Capabilities.ConfigWarnings.
//
// They are warnings, not refusals, on purpose: refusing a config that booted
// yesterday is a breaking change and belongs in a MINOR release. The values
// are the ones the code acts on (counters.CurrentPeriod, the default credit
// handler, config.Tier.Validate); everything else falls to a default.
func (c *Config) Warnings() []string {
	var w []string
	switch {
	case c.Enforcement.enabledUnset:
		// v0.3.0: the key is absent from the config file — the text both libraries print.
		w = append(w, EnforcementEnabledUnsetWarning)
	case !c.Enforcement.Enabled:
		w = append(w, `enforcement.enabled is false: EVERY request is allowed `+
			`(reason "enforcement_disabled"). Since v0.3.0 a config file that omits the key defaults to true, `+
			`as in the Python runtime — `+
			`write "enforcement": {"enabled": true} (with "shadow_mode": true to observe first)`)
	}
	resources := map[string]bool{}
	for _, r := range c.Resources {
		resources[r.ResourceKey] = true
		if r.CounterType == CounterAccumulator {
			switch r.ResetPeriod {
			case ResetHourly, ResetDaily, ResetWeekly, ResetMonthly:
			default:
				w = append(w, fmt.Sprintf(`resource %q: reset_period %q is not hourly|daily|weekly|monthly — `+
					`the counter NEVER resets (a monthly cap becomes a lifetime cap)`, r.ResourceKey, r.ResetPeriod))
			}
		}
	}
	for _, t := range c.Tiers {
		keys := make([]string, 0, len(t.Limits))
		for k := range t.Limits {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !resources[k] {
				w = append(w, fmt.Sprintf(`tier %q: limit for %q names no resource in resources[] — `+
					`it is never enforced (typo?)`, t.TierID, k))
			}
		}
		switch t.BillingModel {
		case "", BillingModelConsumptionOnly, BillingModelSubscriptionUnlockOnly, BillingModelSubscriptionWithCredits,
			BillingModelMeteredBilling, BillingModelOneTimePurchase, BillingModelFreeTier, BillingModelEnterprise:
		default:
			w = append(w, fmt.Sprintf(`tier %q: unknown billing_model %q — none of its cross-field rules apply`,
				t.TierID, t.BillingModel))
		}
		g := t.CreditGrant
		if g == nil {
			continue
		}
		if g.resetOnDowngradeUnset {
			w = append(w, ResetOnDowngradeUnsetWarning(t.TierID))
		}
		switch g.Trigger {
		case CreditTriggerSignup, CreditTriggerSubscriptionInvoicePaid, CreditTriggerScheduledPeriodStart,
			CreditTriggerManual, CreditTriggerWebhookAdmin:
		case "":
			w = append(w, fmt.Sprintf(`tier %q: credit_grant has no trigger — the Go runtime treats it as `+
				`"signup", the Python runtime grants nothing; declare it`, t.TierID))
		default:
			w = append(w, fmt.Sprintf(`tier %q: unknown credit_grant.trigger %q — it is never granted`, t.TierID, g.Trigger))
		}
		switch g.Dedup {
		case DedupPerUserPerTier, DedupPerOrgPerTier, DedupPerUserGlobal, DedupPerOrgGlobal:
		default:
			w = append(w, fmt.Sprintf(`tier %q: unknown credit_grant.dedup %q — treated as per_user_per_tier `+
				`(one grant per user, so every member of an org can claim it)`, t.TierID, g.Dedup))
		}
		switch g.Lifecycle {
		case CreditLifecyclePersistent, CreditLifecycleUseItOrLoseIt, CreditLifecycleRolloverCapped:
		default:
			w = append(w, fmt.Sprintf(`tier %q: unknown credit_grant.lifecycle %q — billing refuses the grant (422)`,
				t.TierID, g.Lifecycle))
		}
		switch g.Destination {
		case CreditDestCreditBalance, CreditDestSubscriptionCredit:
		default:
			w = append(w, fmt.Sprintf(`tier %q: unknown credit_grant.destination %q — billing refuses the grant (422)`,
				t.TierID, g.Destination))
		}
	}
	return w
}
