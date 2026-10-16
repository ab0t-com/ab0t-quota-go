package config

// Ticket 20261016 review A-08/A-12/C-03/C-04/C-05: configs that load and boot but
// silently fail open must be reported. RED on v0.1.9 by compile (Warnings is new).

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

func TestWarnings_FlagSilentFailOpenShapes_20261016(t *testing.T) {
	lim := 10.0
	c := &Config{
		Storage:     StorageConfig{RedisURL: Declare("memory://")},
		Enforcement: EnforcementConfig{Enabled: false},
		Resources: []ResourceDef{
			{ResourceKey: "spend", CounterType: CounterAccumulator, ResetPeriod: "monthy"},
			{ResourceKey: "req", CounterType: CounterRate, WindowSeconds: 60},
		},
		Tiers: []Tier{{TierID: "free", Limits: map[string]TierLimit{"spend": {Limit: &lim}, "reqs": {Limit: &lim}},
			CreditGrant: &CreditGrant{Trigger: "sign_up", AmountPerPeriod: Decimal{Decimal: decimal.NewFromInt(5)},
				Dedup: "per_org", Lifecycle: CreditLifecyclePersistent, Destination: CreditDestCreditBalance}}},
	}
	got := strings.Join(c.Warnings(), "\n")
	for _, want := range []string{"enforcement.enabled", `reset_period "monthy"`, `"reqs" names no resource`,
		`credit_grant.trigger "sign_up"`, `credit_grant.dedup "per_org"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing warning containing %q; got:\n%s", want, got)
		}
	}
	c.Enforcement.Enabled = true
	c.Resources[0].ResetPeriod = ResetMonthly
	c.Tiers[0].Limits = map[string]TierLimit{"spend": {Limit: &lim}}
	c.Tiers[0].CreditGrant.Trigger, c.Tiers[0].CreditGrant.Dedup = CreditTriggerSignup, DedupPerOrgPerTier
	if w := c.Warnings(); len(w) != 0 {
		t.Errorf("clean config warned: %v", w)
	}
}
