package engine

// D-55 (adjudicated) — Acquire must honour enabled=false and shadow_mode like
// Check does. Python resolved this in a concurrent leg: acquire honours
// enabled=False, and under shadow_mode it ADMITS + SPENDS + logs
// shadow_would_deny. Shadow observes; it never refuses — a shadow mode that
// hard-denies blocks the very customers the safe rollout protects.
//
// RED before the fix (Acquire hard-denies over-limit under shadow/disabled).

import (
	"context"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
)

func seedAtLimit(t *testing.T, e *Engine) {
	t.Helper()
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	if _, err := e.Factory.Floats.IncrByFloat(context.Background(), g.OrgKey("o1"), 1); err != nil {
		t.Fatal(err)
	}
}

func TestAcquire_ShadowMode_AdmitsAndSpendsOverLimit_D55(t *testing.T) {
	e := matrixEngine(t, config.EnforcementConfig{Enabled: true, ShadowMode: true}, 1) // limit 1
	seedAtLimit(t, e)                                                                  // gauge = 1 = cap
	res, err := e.Acquire(context.Background(), AcquireInput{OrgID: "o1", ResourceKey: "sandboxes"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Admitted {
		t.Errorf("D-55: shadow_mode must ADMIT an over-limit acquire (shadow observes, never refuses), got denied reason=%q", res.Reason)
	}
	// It also SPENDS (admits + spends): gauge goes to 2.
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	if v, _, _ := e.Factory.Floats.GetFloat(context.Background(), g.OrgKey("o1")); v != 2 {
		t.Errorf("D-55: shadow acquire must spend (gauge want 2, got %v)", v)
	}
}

func TestAcquire_EnforcementDisabled_AdmitsOverLimit_D55(t *testing.T) {
	e := matrixEngine(t, config.EnforcementConfig{Enabled: false}, 1)
	seedAtLimit(t, e)
	res, err := e.Acquire(context.Background(), AcquireInput{OrgID: "o1", ResourceKey: "sandboxes"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Admitted {
		t.Errorf("D-55: enforcement.enabled=false must ADMIT (like Check), got denied reason=%q", res.Reason)
	}
}

// Enforce mode is unchanged: over-limit still denies.
func TestAcquire_EnforceMode_StillDeniesOverLimit_D55(t *testing.T) {
	e := matrixEngine(t, config.EnforcementConfig{Enabled: true}, 1)
	seedAtLimit(t, e)
	res, _ := e.Acquire(context.Background(), AcquireInput{OrgID: "o1", ResourceKey: "sandboxes"})
	if res.Admitted {
		t.Error("enforce mode must still deny an over-limit acquire")
	}
}
