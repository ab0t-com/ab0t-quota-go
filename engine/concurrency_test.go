package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/messages"
	"github.com/ab0t-com/ab0t-quota-go/providers"
	"github.com/ab0t-com/ab0t-quota-go/registry"
)

// TestEngine_ConcurrentSpendsAreAccurate races 100 goroutines doing Spend(1)
// on a gauge. Final value must equal exactly 100 — no race losses.
func TestEngine_ConcurrentSpendsAreAccurate(t *testing.T) {
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", Mapping: map[string]string{"alice": "pro"}},
		Tiers: []config.Tier{
			{TierID: "pro", Limits: map[string]config.TierLimit{
				"sandbox.concurrent": {Limit: ptrFloat(1_000_000)}, // effectively unbounded
			}},
		},
		Resources: []config.ResourceDef{
			{ResourceKey: "sandbox.concurrent", CounterType: config.CounterGauge},
		},
	}
	prov, _ := providers.New(cfg.TierProvider)
	e := &Engine{
		Cfg: cfg, Reg: registry.New(cfg), Provider: prov,
		Factory: counters.NewMemoryFactory("quota"), Messages: messages.New(messages.Templates{}),
	}

	const N = 100
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = e.Spend(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "sandbox.concurrent", Cost: 1})
		}()
	}
	wg.Wait()

	res, _ := e.Check(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "sandbox.concurrent"})
	if res.Used != float64(N) {
		t.Errorf("after %d concurrent spends, used = %v (want %d)", N, res.Used, N)
	}
}

// TestEngine_AccumulatorNoDoubleSpend races 50 spends of 0.5 USD on a
// monthly cap; final value must be 25.0 exactly.
func TestEngine_AccumulatorNoDoubleSpend(t *testing.T) {
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", Mapping: map[string]string{"alice": "pro"}},
		Tiers: []config.Tier{
			{TierID: "pro", Limits: map[string]config.TierLimit{
				"spend.usd": {Limit: ptrFloat(1000)},
			}},
		},
		Resources: []config.ResourceDef{
			{ResourceKey: "spend.usd", CounterType: config.CounterAccumulator, ResetPeriod: config.ResetMonthly},
		},
	}
	prov, _ := providers.New(cfg.TierProvider)
	e := &Engine{
		Cfg: cfg, Reg: registry.New(cfg), Provider: prov,
		Factory: counters.NewMemoryFactory("quota"), Messages: messages.New(messages.Templates{}),
		Clock: func() time.Time { return time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC) },
	}

	const N = 50
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = e.Spend(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "spend.usd", Cost: 0.5})
		}()
	}
	wg.Wait()

	res, _ := e.Check(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "spend.usd"})
	// 50 × 0.5 = 25.0 — small float tolerance.
	want := float64(N) * 0.5
	if res.Used < want-0.001 || res.Used > want+0.001 {
		t.Errorf("after %d concurrent spends of 0.5, used = %v (want ≈ %v)", N, res.Used, want)
	}
}

// TestEngine_ConcurrentReleaseDoesntUnderflow runs paired Spend+Release
// on a gauge; net counter must return to 0.
func TestEngine_ConcurrentReleaseDoesntUnderflow(t *testing.T) {
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", Mapping: map[string]string{"alice": "pro"}},
		Tiers: []config.Tier{
			{TierID: "pro", Limits: map[string]config.TierLimit{
				"x": {Limit: ptrFloat(1_000_000)},
			}},
		},
		Resources: []config.ResourceDef{{ResourceKey: "x", CounterType: config.CounterGauge}},
	}
	prov, _ := providers.New(cfg.TierProvider)
	e := &Engine{
		Cfg: cfg, Reg: registry.New(cfg), Provider: prov,
		Factory: counters.NewMemoryFactory("quota"), Messages: messages.New(messages.Templates{}),
	}

	const N = 50
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(N * 2)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = e.Spend(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "x", Cost: 1})
		}()
		go func() {
			defer wg.Done()
			_ = e.Release(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "x", Cost: 1})
		}()
	}
	wg.Wait()

	res, _ := e.Check(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "x"})
	if res.Used != 0 {
		t.Errorf("paired spend/release should net to 0, got %v", res.Used)
	}
}
