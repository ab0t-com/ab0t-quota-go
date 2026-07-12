package engine

// D-48 — Acquire inherited NEITHER guard that Check has: an unknown bundle /
// typo'd resource_key was ADMITTED with zero enforcement, and global_kill_switch
// was a NO-OP on the primary admission gate. Both are the forbidden fail-OPEN
// direction (D-31): a config that means "deny/halt" silently widened to "allow".
// This is the same class as Python's D-48 — the new primitive shipped without
// the old primitive's guards.
//
// RED before the fix (Acquire admits in all three cases).

import (
	"context"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/activations"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/messages"
	"github.com/ab0t-com/ab0t-quota-go/providers"
	"github.com/ab0t-com/ab0t-quota-go/registry"
)

func acquireGuardEngine(t *testing.T, enf config.EnforcementConfig) *Engine {
	t.Helper()
	limit := 5.0
	cfg := &config.Config{
		Enforcement:     enf,
		TierProvider:    config.TierProviderConfig{Type: "static", DefaultTier: "pro"},
		Tiers:           []config.Tier{{TierID: "pro", Limits: map[string]config.TierLimit{"sandboxes": {Limit: &limit}}}},
		Resources:       []config.ResourceDef{{ResourceKey: "sandboxes", CounterType: config.CounterGauge}},
		ResourceBundles: map[string][]string{"sandbox_bundle": {"sandboxes"}},
	}
	prov, _ := providers.New(cfg.TierProvider)
	return &Engine{
		Cfg: cfg, Reg: registry.New(cfg), Provider: prov, Factory: counters.NewMemoryFactory("quota"),
		Messages:    messages.New(messages.Templates{}),
		Clock:       func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Activations: activations.NewInMemoryStore(),
	}
}

// Hole 1a: an unknown BUNDLE name must be denied in enforce mode, not admitted.
func TestAcquire_UnknownBundle_DeniedInEnforce_D48(t *testing.T) {
	e := acquireGuardEngine(t, config.EnforcementConfig{Enabled: true})
	res, err := e.Acquire(context.Background(), AcquireInput{OrgID: "o1", BundleName: "typo_not_a_bundle"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Admitted {
		t.Errorf("D-48 FAIL-OPEN: unknown bundle ADMITTED (reason=%q) — a typo disabled enforcement", res.Reason)
	}
	if res.Reason != "unknown_bundle" {
		t.Errorf("want reason unknown_bundle, got %q", res.Reason)
	}
}

// Hole 1b: an unregistered resource_key (typo) must be denied, not admitted.
func TestAcquire_UnknownResourceKey_DeniedInEnforce_D48(t *testing.T) {
	e := acquireGuardEngine(t, config.EnforcementConfig{Enabled: true})
	res, err := e.Acquire(context.Background(), AcquireInput{OrgID: "o1", ResourceKey: "sandboxez_typo"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Admitted {
		t.Errorf("D-48 FAIL-OPEN: unknown resource_key ADMITTED (reason=%q)", res.Reason)
	}
}

// Hole 2: global_kill_switch must halt Acquire, exactly as it halts Check.
func TestAcquire_GlobalKillSwitch_Denies_D48(t *testing.T) {
	e := acquireGuardEngine(t, config.EnforcementConfig{Enabled: true, GlobalKillSwitch: true})
	res, err := e.Acquire(context.Background(), AcquireInput{OrgID: "o1", ResourceKey: "sandboxes"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Admitted {
		t.Errorf("D-48 FAIL-OPEN: global_kill_switch was a NO-OP on Acquire (admitted=%v) — the emergency halt lever did nothing", res.Admitted)
	}
	if res.Reason != "global_kill_switch" {
		t.Errorf("want reason global_kill_switch, got %q", res.Reason)
	}
}

// Parity: under shadow_mode / enforcement-off, an unknown bundle is allowed +
// warned (not hard-denied) — mirrors Python's acquire and Check's knob
// semantics. (This distinguishes the enforce-deny from a blanket deny.)
func TestAcquire_UnknownBundle_AllowedUnderShadow_D48(t *testing.T) {
	e := acquireGuardEngine(t, config.EnforcementConfig{Enabled: true, ShadowMode: true})
	res, _ := e.Acquire(context.Background(), AcquireInput{OrgID: "o1", BundleName: "typo_not_a_bundle"})
	if !res.Admitted {
		t.Errorf("under shadow_mode an unknown bundle should be allowed+warned, got denied (reason=%q)", res.Reason)
	}
}
