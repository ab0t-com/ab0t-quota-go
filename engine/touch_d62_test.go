package engine

// D-62 — the engine tracks recent activity so the reconciler's guard is
// zero-config (a correctness feature a client must remember to supply is a
// correctness feature that is absent). Acquire/Spend record a touch; the guard
// window is a knob.

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

func touchEngine(t *testing.T, clock func() time.Time) *Engine {
	t.Helper()
	limit := 10.0
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", DefaultTier: "pro"},
		Tiers:        []config.Tier{{TierID: "pro", Limits: map[string]config.TierLimit{"sandboxes": {Limit: &limit}}}},
		Resources:    []config.ResourceDef{{ResourceKey: "sandboxes", CounterType: config.CounterGauge}},
	}
	prov, _ := providers.New(cfg.TierProvider)
	return &Engine{Cfg: cfg, Reg: registry.New(cfg), Provider: prov, Factory: counters.NewMemoryFactory("quota"), Messages: messages.New(messages.Templates{}), Clock: clock}
}

func TestEngine_AcquireRecordsTouch_D62(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	e := touchEngine(t, func() time.Time { return now })
	if e.RecentlyTouched("o1", "sandboxes", time.Minute) {
		t.Fatal("nothing touched yet")
	}
	if _, err := e.Acquire(context.Background(), AcquireInput{OrgID: "o1", ResourceKey: "sandboxes"}); err != nil {
		t.Fatal(err)
	}
	if !e.RecentlyTouched("o1", "sandboxes", time.Minute) {
		t.Error("Acquire must record a touch within the window")
	}
	// Advance the clock past the window → no longer recent.
	now = now.Add(2 * time.Minute)
	if e.RecentlyTouched("o1", "sandboxes", time.Minute) {
		t.Error("touch should have aged out past the window")
	}
}

func TestEngine_SpendRecordsTouch_D62(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	e := touchEngine(t, func() time.Time { return now })
	if _, err := e.Spend(context.Background(), CheckInput{OrgID: "o1", ResourceKey: "sandboxes"}); err != nil {
		t.Fatal(err)
	}
	if !e.RecentlyTouched("o1", "sandboxes", time.Minute) {
		t.Error("Spend must record a touch")
	}
	// The TouchGuard closure reflects it.
	if !e.TouchGuard(time.Minute)("o1", "sandboxes") {
		t.Error("TouchGuard should report the recent touch")
	}
}
