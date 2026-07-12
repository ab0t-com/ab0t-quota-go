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

// TestEngine_ConcurrentReleaseDoesntUnderflow runs paired Spend+Release on a
// gauge; each pair's lifecycle nets to 0, so the fully-drained gauge is 0.
//
// REFRAMED 2026-07-12 (W-D20) — cite DECISIONS.md D-20.
//
// The original assertion fired N *unpaired* concurrent Spend(+1) and N unpaired
// Release(-1) and demanded an EXACT net of 0. That net is only reachable if the
// gauge is allowed to go transiently NEGATIVE — a Release that wins the race
// before its paired Spend would have to record a -1. But a gauge is the *level*
// of currently-active resources and floors at 0 by design (QG-06 / D-24): a
// release at level 0 is an over-release and is clamped, so that decrement is
// lost and a later Spend leaves positive residue. The old assertion therefore
// demanded the very phantom-headroom bug the floor exists to kill; it is a
// non-guarantee (it fails against Python too, which also floors). Determined
// empirically: the engine is atomic and race-clean (Spend→IncrByFloat,
// Release→DecrByFloorZero, both mutex-guarded RMW under -race); the residue is
// the floor, not a lost update.
//
// The LEGITIMATE intent behind the name — "a paired spend/release lifecycle
// nets to zero, and a duplicate/racing release never underflows" — is what this
// reframed test now asserts, and it does so more strongly than the D-20
// fallback of `Used >= 0`: each goroutine pairs its own Spend→Release with a
// happens-before, and the pairs run concurrently. This is deterministic
// (net == 0 exactly) AND still has teeth against a real engine race — a
// non-atomic read-modify-write in Spend/Release would drop increments or
// decrements and break the exact-0. The unordered floor invariant (Used >= 0,
// never exceeds N) is owned by TestGaugeConcurrent_NeverUnderflows_Correct_QG06
// (gauge_floor_concurrent_test.go) and TestGaugeRelease_FloorsAtZero_...QG06.
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
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			// Happens-before within the pair: the Spend completes before its
			// paired Release runs, so no Release ever races ahead of level 0.
			_, _ = e.Spend(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "x", Cost: 1})
			_ = e.Release(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "x", Cost: 1})
		}()
	}
	wg.Wait()

	res, _ := e.Check(ctx, CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "x"})
	// Every paired lifecycle nets to 0 → the drained gauge is exactly 0, and
	// never negative (the anti-underflow guarantee the test name promises).
	if res.Used != 0 {
		t.Errorf("paired (happens-before) spend/release should net to 0, got %v", res.Used)
	}
}
