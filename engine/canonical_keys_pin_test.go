package engine

// Claim 4 / D-18 (finding QG-03) — pin the engine to the CANONICAL Python
// parity key builders (Gauge.OrgKey / Accumulator.OrgPeriodKey / Rate.OrgKey
// in counters/keys_python.go). The pre-P5.3 scope-based builders
// (Gauge.Key / Accumulator.PeriodKey / Rate.Key) are Deprecated but retained
// because a peer's counters_test.go still asserts their shape (house rule:
// never rewrite a peer's tests). This test guards the seam D-18 protects: a
// refactor that silently switched the engine BACK to the deprecated builders
// would write the wrong keyspace again — this makes that impossible to do
// quietly.

import (
	"context"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/messages"
	"github.com/ab0t-com/ab0t-quota-go/providers"
	"github.com/ab0t-com/ab0t-quota-go/registry"
)

func TestEngine_WritesCanonicalKeys_NotDeprecatedShape_D18(t *testing.T) {
	cap := 100.0
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", Mapping: map[string]string{"u1": "pro"}},
		Tiers: []config.Tier{{TierID: "pro", Limits: map[string]config.TierLimit{
			"sandboxes":     {Limit: &cap},
			"api_spend_usd": {Limit: &cap},
			"api_calls":     {Limit: &cap},
		}}},
		Resources: []config.ResourceDef{
			{ResourceKey: "sandboxes", CounterType: config.CounterGauge},
			{ResourceKey: "api_spend_usd", CounterType: config.CounterAccumulator, ResetPeriod: config.ResetMonthly},
			{ResourceKey: "api_calls", CounterType: config.CounterRate, WindowSeconds: 3600},
		},
	}
	factory := counters.NewMemoryFactory("quota")
	at := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	e := &Engine{
		Cfg: cfg, Reg: registry.New(cfg), Factory: factory,
		Messages: messages.New(messages.Templates{}),
		Clock:    func() time.Time { return at },
	}
	prov, _ := providers.New(cfg.TierProvider)
	e.Provider = prov
	ctx := context.Background()
	in := func(rk string) CheckInput {
		return CheckInput{UserID: "u1", OrgID: "org-1", ResourceKey: rk, Cost: 1}
	}

	// Gauge: value lands under the canonical OrgKey, and NOT under the
	// deprecated scope-based Key shape.
	if _, err := e.Spend(ctx, in("sandboxes")); err != nil {
		t.Fatal(err)
	}
	g := factory.Gauge("sandboxes")
	if _, found, _ := factory.Floats.GetFloat(ctx, g.OrgKey("org-1")); !found {
		t.Errorf("D-18: gauge spend did not write canonical key %s", g.OrgKey("org-1"))
	}
	if _, found, _ := factory.Floats.GetFloat(ctx, g.Key("org:org-1")); found {
		t.Errorf("D-18: gauge spend wrote the DEPRECATED key %s — engine regressed to the old builder", g.Key("org:org-1"))
	}

	// Accumulator: canonical OrgPeriodKey written, deprecated PeriodKey not.
	if _, err := e.Spend(ctx, in("api_spend_usd")); err != nil {
		t.Fatal(err)
	}
	a := factory.Accumulator("api_spend_usd", config.ResetMonthly)
	if _, found, _ := factory.Floats.GetFloat(ctx, a.OrgPeriodKey("org-1", at)); !found {
		t.Errorf("D-18: accumulator spend did not write canonical key %s", a.OrgPeriodKey("org-1", at))
	}
	if _, found, _ := factory.Floats.GetFloat(ctx, a.PeriodKey("org:org-1", at)); found {
		t.Errorf("D-18: accumulator spend wrote the DEPRECATED key shape — engine regressed")
	}
}
