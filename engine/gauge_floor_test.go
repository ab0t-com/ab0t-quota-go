package engine

// Claim 1 (finding QG-06, the correctness bug inside it) — Go's gauge
// Release has NO floor-at-zero. Python floors (gauge.py:79-81); Go does a
// raw negative INCRBYFLOAT (engine.go Release). An over-release (more
// decrements than increments — a double-fired stop, a retried teardown)
// drives the gauge NEGATIVE, which silently manufactures free quota
// headroom: subsequent Check reads a negative `used`, so the org can exceed
// its real limit. This is a live limit-integrity defect, not a parity nit.
//
// RED before the fix: over-released gauge reads negative; a later Check
// grants more than the cap allows.

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

func floorEngine(t *testing.T) (*Engine, *counters.Factory) {
	t.Helper()
	cap := 3.0
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", Mapping: map[string]string{"alice": "pro"}},
		Tiers: []config.Tier{{TierID: "pro", Limits: map[string]config.TierLimit{
			"sandboxes": {Limit: &cap},
		}}},
		Resources: []config.ResourceDef{{ResourceKey: "sandboxes", CounterType: config.CounterGauge}},
	}
	reg := registry.New(cfg)
	factory := counters.NewMemoryFactory("quota")
	prov, _ := providers.New(cfg.TierProvider)
	return &Engine{
		Cfg: cfg, Reg: reg, Provider: prov, Factory: factory,
		Messages: messages.New(messages.Templates{}),
		Clock:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}, factory
}

func TestGaugeRelease_FloorsAtZero_NoNegativeHeadroom_QG06(t *testing.T) {
	e, factory := floorEngine(t)
	ctx := context.Background()
	in := CheckInput{UserID: "alice", OrgID: "o1", ResourceKey: "sandboxes"}

	// One acquire, then THREE releases (over-release by 2).
	if _, err := e.Spend(ctx, in); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := e.Release(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	org := partitionOrg(in.UserID, in.OrgID)
	g := factory.Gauge("sandboxes")

	orgVal, _, _ := factory.Floats.GetFloat(ctx, g.OrgKey(org))
	if orgVal < 0 {
		t.Errorf("QG-06: org gauge went NEGATIVE (%v) — floor-at-zero missing; this manufactures free headroom", orgVal)
	}
	userVal, _, _ := factory.Floats.GetFloat(ctx, g.UserKey(org, in.UserID))
	if userVal < 0 {
		t.Errorf("QG-06: per-user gauge went NEGATIVE (%v) — floor-at-zero missing", userVal)
	}

	// The integrity consequence: with a floored gauge, `used` is 0 and the
	// org gets exactly its cap. With a negative gauge it would get cap+2.
	res, err := e.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Used != 0 {
		t.Errorf("QG-06: Check.Used=%v after over-release (want 0) — negative usage grants phantom quota", res.Used)
	}
}
