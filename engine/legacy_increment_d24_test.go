package engine

// Claim 4 / D-24 — the legacy Spend path must NEVER refuse by default; it
// counts at the fact and (when it crosses the limit) makes over-admission an
// OBSERVABLE event, not a silent undercount. Refusing after provisioning
// would leave a resource existing-and-uncounted = phantom headroom.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/messages"
	"github.com/ab0t-com/ab0t-quota-go/providers"
	"github.com/ab0t-com/ab0t-quota-go/registry"
)

func d24Engine(t *testing.T, mode string) (*Engine, *counters.Factory) {
	t.Helper()
	limit := 1.0
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true, LegacyIncrement: mode},
		TierProvider: config.TierProviderConfig{Type: "static", DefaultTier: "pro"},
		Tiers:        []config.Tier{{TierID: "pro", Limits: map[string]config.TierLimit{"sandboxes": {Limit: &limit}}}},
		Resources:    []config.ResourceDef{{ResourceKey: "sandboxes", CounterType: config.CounterGauge}},
	}
	prov, _ := providers.New(cfg.TierProvider)
	fs := counters.NewMemoryFactory("quota")
	e := &Engine{
		Cfg: cfg, Reg: registry.New(cfg), Provider: prov, Factory: fs,
		Messages: messages.New(messages.Templates{}),
		Clock:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
	return e, fs
}

// Default (count_and_alert): Spend past the limit COUNTS (never refuses).
func TestSpend_DefaultNeverRefuses_CountsPastLimit_D24(t *testing.T) {
	e, fs := d24Engine(t, "") // "" ⇒ count_and_alert
	ctx := context.Background()
	in := CheckInput{OrgID: "o1", ResourceKey: "sandboxes", Cost: 1}

	if v, err := e.Spend(ctx, in); err != nil || v != 1 {
		t.Fatalf("first spend: v=%v err=%v", v, err)
	}
	// Second spend crosses the limit — it must STILL count (no refusal), so
	// the counter reflects the resource that actually exists.
	v, err := e.Spend(ctx, in)
	if err != nil {
		t.Fatalf("D-24: legacy Spend refused past the limit (err=%v) — must count at the fact", err)
	}
	if v != 2 {
		t.Errorf("D-24: gauge = %v after over-limit spend (want 2 — counted, not refused)", v)
	}
	got, _, _ := fs.Floats.GetFloat(ctx, counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}.OrgKey("o1"))
	if got != 2 {
		t.Errorf("stored gauge = %v (want 2)", got)
	}
}

// Opt-in enforce: Spend past the limit refuses atomically (ErrOverLimit) and
// does NOT count.
func TestSpend_EnforceMode_RefusesPastLimit_D24(t *testing.T) {
	e, fs := d24Engine(t, "enforce")
	ctx := context.Background()
	in := CheckInput{OrgID: "o1", ResourceKey: "sandboxes", Cost: 1}

	if _, err := e.Spend(ctx, in); err != nil {
		t.Fatalf("first spend: %v", err)
	}
	_, err := e.Spend(ctx, in)
	if !errors.Is(err, ErrOverLimit) {
		t.Fatalf("enforce mode: expected ErrOverLimit past the limit, got %v", err)
	}
	got, _, _ := fs.Floats.GetFloat(ctx, counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}.OrgKey("o1"))
	if got != 1 {
		t.Errorf("enforce mode: gauge = %v after refused spend (want 1 — not spent)", got)
	}
}
