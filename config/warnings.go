package config

import (
	"fmt"
	"sort"
)

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
	if !c.Enforcement.Enabled {
		w = append(w, `enforcement.enabled is false or absent: EVERY request is allowed `+
			`(reason "enforcement_disabled"). The Python runtime defaults it to true — `+
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
