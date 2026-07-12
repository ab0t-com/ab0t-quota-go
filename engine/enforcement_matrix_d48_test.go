package engine

// D-48 / D-43 conformance scenario 1 — the enforcement matrix. Every guard
// knob × every ADMISSION GATE entry point (Check, Acquire, Acquire-bundle) →
// consistent outcome. This is the CI tripwire that stops "the new primitive
// forgot the old primitive's guards" from recurring: a gate that ignores a
// deny/halt knob is a fail-OPEN and fails here.
//
// Spend/Release are NOT gates (D-24: legacy Spend counts at the fact and never
// refuses), so they are excluded — a matrix that demanded Spend "deny" would
// contradict D-24.

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

// admittedByCheck runs Check and reports whether the request was admitted
// (any non-Deny, non-error decision — Allow/Warn/Critical/ShadowAllow).
func admittedByCheck(t *testing.T, e *Engine, rk string) bool {
	res, err := e.Check(context.Background(), CheckInput{OrgID: "o1", ResourceKey: rk})
	if err != nil {
		return false // Check errors on unknown resource → not admitted
	}
	return res.Decision != Deny
}

func admittedByAcquire(t *testing.T, e *Engine, in AcquireInput) bool {
	res, err := e.Acquire(context.Background(), in)
	if err != nil {
		return false
	}
	return res.Admitted
}

func matrixEngine(t *testing.T, enf config.EnforcementConfig, orgLimit float64) *Engine {
	t.Helper()
	cfg := &config.Config{
		Enforcement:     enf,
		TierProvider:    config.TierProviderConfig{Type: "static", DefaultTier: "pro"},
		Tiers:           []config.Tier{{TierID: "pro", Limits: map[string]config.TierLimit{"sandboxes": {Limit: &orgLimit}}}},
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

// HARD invariants — the two D-48 fail-opens. A gate that ignores these fails CI.
func TestEnforcementMatrix_KillSwitchAndUnknown_AllGatesDeny_D48(t *testing.T) {
	// global_kill_switch → NO gate admits.
	t.Run("global_kill_switch", func(t *testing.T) {
		e := matrixEngine(t, config.EnforcementConfig{Enabled: true, GlobalKillSwitch: true}, 100)
		if admittedByCheck(t, e, "sandboxes") {
			t.Error("Check admitted under global_kill_switch")
		}
		if admittedByAcquire(t, e, AcquireInput{OrgID: "o1", ResourceKey: "sandboxes"}) {
			t.Error("Acquire admitted under global_kill_switch (fail-OPEN)")
		}
		if admittedByAcquire(t, e, AcquireInput{OrgID: "o1", BundleName: "sandbox_bundle"}) {
			t.Error("Acquire(bundle) admitted under global_kill_switch (fail-OPEN)")
		}
	})

	// unknown bundle / resource in ENFORCE → NO gate admits.
	t.Run("unknown_enforce", func(t *testing.T) {
		e := matrixEngine(t, config.EnforcementConfig{Enabled: true}, 100)
		if admittedByCheck(t, e, "typo_resource") {
			t.Error("Check admitted an unknown resource")
		}
		if admittedByAcquire(t, e, AcquireInput{OrgID: "o1", ResourceKey: "typo_resource"}) {
			t.Error("Acquire admitted an unknown resource (fail-OPEN)")
		}
		if admittedByAcquire(t, e, AcquireInput{OrgID: "o1", BundleName: "typo_bundle"}) {
			t.Error("Acquire admitted an unknown bundle (fail-OPEN)")
		}
	})

	// Sanity: a known, under-limit resource is admitted by every gate (the
	// matrix is not vacuously denying everything).
	t.Run("known_under_limit_all_admit", func(t *testing.T) {
		e := matrixEngine(t, config.EnforcementConfig{Enabled: true}, 100)
		if !admittedByCheck(t, e, "sandboxes") {
			t.Error("Check denied a known under-limit resource")
		}
		if !admittedByAcquire(t, e, AcquireInput{OrgID: "o1", ResourceKey: "sandboxes"}) {
			t.Error("Acquire denied a known under-limit resource")
		}
	})
}

// D-55 (was the D-49 divergence, now RESOLVED) — enabled=false and shadow_mode
// on a KNOWN, OVER-limit resource: Check and Acquire must now give the SAME
// outcome (both admit). Python adopted this and so has Go: shadow observes,
// never refuses; enabled=false bypasses. HARD-asserted parity — a regression
// where Acquire hard-denies under shadow (blocking a real customer during a
// rollout) fails here.
func TestEnforcementMatrix_EnabledShadow_KnownResource_Parity_D55(t *testing.T) {
	seed := func(e *Engine) {
		g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
		_, _ = e.Factory.Floats.IncrByFloat(context.Background(), g.OrgKey("o1"), 1) // limit=1 → at cap
	}
	t.Run("enforcement_disabled_both_admit", func(t *testing.T) {
		e := matrixEngine(t, config.EnforcementConfig{Enabled: false}, 1)
		seed(e)
		if !admittedByCheck(t, e, "sandboxes") || !admittedByAcquire(t, e, AcquireInput{OrgID: "o1", ResourceKey: "sandboxes"}) {
			t.Error("D-55: enforcement.enabled=false — both Check and Acquire must admit an over-limit resource")
		}
	})
	t.Run("shadow_mode_both_admit", func(t *testing.T) {
		e := matrixEngine(t, config.EnforcementConfig{Enabled: true, ShadowMode: true}, 1)
		seed(e)
		if !admittedByCheck(t, e, "sandboxes") || !admittedByAcquire(t, e, AcquireInput{OrgID: "o1", ResourceKey: "sandboxes"}) {
			t.Error("D-55: shadow_mode — both Check and Acquire must ADMIT (shadow observes, never refuses)")
		}
	})
}
