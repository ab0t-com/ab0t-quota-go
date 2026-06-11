package engine

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

func ptrFloat(f float64) *float64 { return &f }

func newEngine(t *testing.T, cfg *config.Config) *Engine {
	t.Helper()
	reg := registry.New(cfg)
	factory := counters.NewMemoryFactory("quota")
	prov, err := providers.New(cfg.TierProvider)
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{
		Cfg:      cfg,
		Reg:      reg,
		Provider: prov,
		Factory:  factory,
		Messages: messages.New(messages.Templates{}),
		Clock:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
}

func TestEngine_AllowUnderLimit(t *testing.T) {
	cfg := &config.Config{
		Enforcement: config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static",
			Mapping: map[string]string{"alice": "pro"}},
		Tiers: []config.Tier{
			{TierID: "pro", Limits: map[string]config.TierLimit{
				"sandbox.concurrent": {Limit: ptrFloat(10), WarningThreshold: 0.8},
			}},
		},
		Resources: []config.ResourceDef{
			{ResourceKey: "sandbox.concurrent", CounterType: config.CounterGauge},
		},
	}
	e := newEngine(t, cfg)
	res, err := e.Check(context.Background(), CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "sandbox.concurrent"})
	if err != nil || res.Decision != Allow {
		t.Fatalf("got %+v err=%v", res, err)
	}
}

func TestEngine_DenyOverLimit(t *testing.T) {
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", Mapping: map[string]string{"alice": "pro"}},
		Tiers: []config.Tier{
			{TierID: "pro", Limits: map[string]config.TierLimit{
				"sandbox.concurrent": {Limit: ptrFloat(2)},
			}},
		},
		Resources: []config.ResourceDef{
			{ResourceKey: "sandbox.concurrent", CounterType: config.CounterGauge},
		},
	}
	e := newEngine(t, cfg)
	ctx := context.Background()
	// Pre-fill to 2 (limit). New request → deny.
	_, _ = e.Spend(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "sandbox.concurrent", Cost: 2})
	res, err := e.Check(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "sandbox.concurrent"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != Deny {
		t.Errorf("got %s", res.Decision)
	}
}

func TestEngine_ShadowModeFlipsDenyToShadowAllow(t *testing.T) {
	cfg := &config.Config{
		Enforcement: config.EnforcementConfig{Enabled: true, ShadowMode: true},
		TierProvider: config.TierProviderConfig{Type: "static", Mapping: map[string]string{"alice": "pro"}},
		Tiers: []config.Tier{
			{TierID: "pro", Limits: map[string]config.TierLimit{
				"sandbox.concurrent": {Limit: ptrFloat(1)},
			}},
		},
		Resources: []config.ResourceDef{
			{ResourceKey: "sandbox.concurrent", CounterType: config.CounterGauge},
		},
	}
	e := newEngine(t, cfg)
	ctx := context.Background()
	_, _ = e.Spend(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "sandbox.concurrent", Cost: 1})
	res, _ := e.Check(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "sandbox.concurrent"})
	if res.Decision != ShadowAllow {
		t.Errorf("got %s", res.Decision)
	}
}

func TestEngine_KillSwitchDenies(t *testing.T) {
	cfg := &config.Config{
		Enforcement: config.EnforcementConfig{Enabled: true, GlobalKillSwitch: true},
		TierProvider: config.TierProviderConfig{Type: "static", DefaultTier: "free"},
		Tiers:        []config.Tier{{TierID: "free"}},
		Resources:    []config.ResourceDef{{ResourceKey: "x", CounterType: config.CounterGauge}},
	}
	e := newEngine(t, cfg)
	res, _ := e.Check(context.Background(), CheckInput{UserID: "a", ResourceKey: "x"})
	if res.Decision != Deny {
		t.Errorf("got %s", res.Decision)
	}
}

func TestEngine_EnforcementDisabledAllowsAll(t *testing.T) {
	cfg := &config.Config{
		Enforcement: config.EnforcementConfig{Enabled: false},
		TierProvider: config.TierProviderConfig{Type: "static", DefaultTier: "free"},
		Tiers:        []config.Tier{{TierID: "free"}},
		Resources:    []config.ResourceDef{{ResourceKey: "x", CounterType: config.CounterGauge}},
	}
	e := newEngine(t, cfg)
	res, _ := e.Check(context.Background(), CheckInput{UserID: "a", ResourceKey: "x"})
	if res.Decision != Allow {
		t.Errorf("got %s", res.Decision)
	}
}

func TestEngine_UnknownResource(t *testing.T) {
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", DefaultTier: "free"},
		Tiers:        []config.Tier{{TierID: "free"}},
	}
	e := newEngine(t, cfg)
	_, err := e.Check(context.Background(), CheckInput{UserID: "a", ResourceKey: "missing"})
	if err == nil {
		t.Error("expected error for unknown resource")
	}
}

func TestEngine_BurstAllowsAboveLimit(t *testing.T) {
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", Mapping: map[string]string{"alice": "pro"}},
		Tiers: []config.Tier{
			{TierID: "pro", Limits: map[string]config.TierLimit{
				"sandbox.concurrent": {Limit: ptrFloat(5), BurstAllowance: 2},
			}},
		},
		Resources: []config.ResourceDef{
			{ResourceKey: "sandbox.concurrent", CounterType: config.CounterGauge},
		},
	}
	e := newEngine(t, cfg)
	ctx := context.Background()
	_, _ = e.Spend(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "sandbox.concurrent", Cost: 5})
	res, _ := e.Check(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "sandbox.concurrent"})
	if res.Decision != Allow || res.Reason != "burst_consumed" {
		t.Errorf("expected burst-allow, got %+v", res)
	}
	// Past burst → deny
	_, _ = e.Spend(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "sandbox.concurrent", Cost: 2})
	res, _ = e.Check(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "sandbox.concurrent"})
	if res.Decision != Deny {
		t.Errorf("expected deny past burst, got %s", res.Decision)
	}
}

func TestEngine_AccumulatorSpend(t *testing.T) {
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", Mapping: map[string]string{"alice": "pro"}},
		Tiers: []config.Tier{
			{TierID: "pro", Limits: map[string]config.TierLimit{
				"spend.usd": {Limit: ptrFloat(100)},
			}},
		},
		Resources: []config.ResourceDef{
			{ResourceKey: "spend.usd", CounterType: config.CounterAccumulator, ResetPeriod: config.ResetMonthly},
		},
	}
	e := newEngine(t, cfg)
	ctx := context.Background()
	if v, err := e.Spend(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "spend.usd", Cost: 25.50}); err != nil || v != 25.50 {
		t.Fatalf("got %v err=%v", v, err)
	}
	if v, _ := e.Spend(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "spend.usd", Cost: 75}); v != 100.50 {
		t.Errorf("got %v", v)
	}
}

func TestEngine_GaugeReleaseDecrements(t *testing.T) {
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", Mapping: map[string]string{"alice": "pro"}},
		Tiers: []config.Tier{
			{TierID: "pro", Limits: map[string]config.TierLimit{
				"sandbox.concurrent": {Limit: ptrFloat(10)},
			}},
		},
		Resources: []config.ResourceDef{
			{ResourceKey: "sandbox.concurrent", CounterType: config.CounterGauge},
		},
	}
	e := newEngine(t, cfg)
	ctx := context.Background()
	_, _ = e.Spend(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "sandbox.concurrent", Cost: 3})
	if err := e.Release(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "sandbox.concurrent", Cost: 1}); err != nil {
		t.Fatal(err)
	}
	res, _ := e.Check(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "sandbox.concurrent"})
	if res.Used != 2 {
		t.Errorf("used = %v", res.Used)
	}
}
