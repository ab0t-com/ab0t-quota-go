package engine

// TASK P5.4 (finding QG-04) — per-user scoping enforcement. The golden test
// covers the per-user PARTITION being written; this covers ENFORCEMENT: a
// user over their derived per-user share is denied even while the org is
// under its cap, and the org-level cap still applies independently.

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

func perUserEngine(t *testing.T, tier config.Tier) (*Engine, *counters.Factory) {
	t.Helper()
	cfg := &config.Config{
		Enforcement: config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{
			Type:    "static",
			Mapping: map[string]string{"alice": tier.TierID, "bob": tier.TierID},
		},
		Tiers:     []config.Tier{tier},
		Resources: []config.ResourceDef{{ResourceKey: "sandboxes", CounterType: config.CounterGauge}},
	}
	reg := registry.New(cfg)
	factory := counters.NewMemoryFactory("quota")
	prov, err := providers.New(cfg.TierProvider)
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{
		Cfg: cfg, Reg: reg, Provider: prov, Factory: factory,
		Messages: messages.New(messages.Templates{}),
		Clock:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}, factory
}

// Explicit per_user_limit: org cap 10, per-user cap 2.
func TestPerUser_ExplicitLimit_DeniesUserUnderOrgCap_QG04(t *testing.T) {
	pul := 2.0
	cap := 10.0
	e, _ := perUserEngine(t, config.Tier{
		TierID: "pro",
		Limits: map[string]config.TierLimit{
			"sandboxes": {Limit: &cap, PerUserLimit: &pul},
		},
	})
	ctx := context.Background()
	in := func(u string) CheckInput { return CheckInput{UserID: u, OrgID: "o1", ResourceKey: "sandboxes"} }

	// alice spends up to her per-user cap.
	for i := 0; i < 2; i++ {
		if _, err := e.Spend(ctx, in("alice")); err != nil {
			t.Fatal(err)
		}
	}
	// alice's 3rd is denied on per-user even though org used=2 << cap=10.
	res, err := e.Check(ctx, in("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != Deny || res.Reason != "per_user_exceeded" {
		t.Fatalf("alice over per-user share should be denied per_user_exceeded, got %+v", res)
	}
	// bob (fresh user) is still allowed — the cap is per-user, not org.
	res, err = e.Check(ctx, in("bob"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != Allow {
		t.Fatalf("bob under his own per-user share should be allowed, got %+v", res)
	}
}

// Derived per-user limit via default_per_user_fraction: cap 10 × 0.2 = 2.
func TestPerUser_DerivedFraction_Enforced_QG04(t *testing.T) {
	cap := 10.0
	e, _ := perUserEngine(t, config.Tier{
		TierID:                 "pro",
		DefaultPerUserFraction: 0.2,
		Limits:                 map[string]config.TierLimit{"sandboxes": {Limit: &cap}},
	})
	ctx := context.Background()
	in := CheckInput{UserID: "alice", OrgID: "o1", ResourceKey: "sandboxes"}
	for i := 0; i < 2; i++ {
		if _, err := e.Spend(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != Deny || res.Reason != "per_user_exceeded" {
		t.Fatalf("derived per-user cap (ceil(10*0.2)=2) should deny 3rd, got %+v", res)
	}
}

// Org cap still binds when no per-user limit is configured.
func TestPerUser_NoPerUserLimit_OrgCapStillApplies(t *testing.T) {
	cap := 2.0
	e, _ := perUserEngine(t, config.Tier{
		TierID: "pro",
		Limits: map[string]config.TierLimit{"sandboxes": {Limit: &cap}},
	})
	ctx := context.Background()
	in := CheckInput{UserID: "alice", OrgID: "o1", ResourceKey: "sandboxes"}
	for i := 0; i < 2; i++ {
		if _, err := e.Spend(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != Deny || res.Reason != "exceeded" {
		t.Fatalf("org cap should deny with reason 'exceeded' (not per_user), got %+v", res)
	}
}

// Release decrements both partitions, restoring per-user headroom.
func TestPerUser_ReleaseRestoresUserHeadroom_QG04(t *testing.T) {
	pul := 1.0
	cap := 10.0
	e, factory := perUserEngine(t, config.Tier{
		TierID: "pro",
		Limits: map[string]config.TierLimit{"sandboxes": {Limit: &cap, PerUserLimit: &pul}},
	})
	ctx := context.Background()
	in := CheckInput{UserID: "alice", OrgID: "o1", ResourceKey: "sandboxes"}

	if _, err := e.Spend(ctx, in); err != nil {
		t.Fatal(err)
	}
	// At per-user cap → next denied.
	if res, _ := e.Check(ctx, in); res.Decision != Deny {
		t.Fatalf("expected deny at per-user cap, got %+v", res)
	}
	// Release frees the user partition.
	if err := e.Release(ctx, in); err != nil {
		t.Fatal(err)
	}
	userVal, _, _ := factory.Floats.GetFloat(ctx, factory.Gauge("sandboxes").UserKey("o1", "alice"))
	if userVal != 0 {
		t.Errorf("user partition should be 0 after release, got %v", userVal)
	}
	if res, _ := e.Check(ctx, in); res.Decision != Allow {
		t.Fatalf("expected allow after release, got %+v", res)
	}
}
