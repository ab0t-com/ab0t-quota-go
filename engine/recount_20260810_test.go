// Ticket 20260810 — Go parity tests for recount-before-deny, read-repair, and
// on-demand ReconcileOrg. The fast Redis counter is a cache of a type-specific
// derived truth; staleness must never harm the user.
package engine

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

func recountCfg() *config.Config {
	return &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", DefaultTier: "free", Mapping: map[string]string{"alice": "free"}},
		Tiers: []config.Tier{
			{TierID: "free", Limits: map[string]config.TierLimit{
				"compute.concurrent":    {Limit: ptrFloat(2)},
				"billing.monthly_spend": {Limit: ptrFloat(10)},
			}},
		},
		Resources: []config.ResourceDef{
			{ResourceKey: "compute.concurrent", CounterType: config.CounterGauge},
			{ResourceKey: "billing.monthly_spend", CounterType: config.CounterAccumulator, ResetPeriod: config.ResetMonthly},
			{ResourceKey: "api.requests", CounterType: config.CounterRate, WindowSeconds: 3600},
		},
	}
}

func newRecountEngine(t *testing.T) *Engine {
	t.Helper()
	cfg := recountCfg()
	prov, err := providers.New(cfg.TierProvider)
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{
		Cfg:      cfg,
		Reg:      registry.New(cfg),
		Provider: prov,
		Factory:  counters.NewMemoryFactory("quota"),
		Messages: messages.New(messages.Templates{}),
		Clock:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
}

func seedGauge(t *testing.T, e *Engine, org string, v float64) {
	t.Helper()
	if err := e.Factory.Floats.Set(context.Background(), e.Factory.Gauge("compute.concurrent").OrgKey(org), v, 0); err != nil {
		t.Fatal(err)
	}
}

func seedAcc(t *testing.T, e *Engine, org string, v float64) {
	t.Helper()
	a := e.Factory.Accumulator("billing.monthly_spend", config.ResetMonthly)
	if err := e.Factory.Floats.Set(context.Background(), a.OrgPeriodKey(org, e.now()), v, 0); err != nil {
		t.Fatal(err)
	}
}

func rcGaugeVal(t *testing.T, e *Engine, org string) float64 {
	t.Helper()
	v, _, err := e.Factory.Floats.GetFloat(context.Background(), e.Factory.Gauge("compute.concurrent").OrgKey(org))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// ---- Phase 1: recount-before-deny (gauge) --------------------------------

func TestRecount_StaleHighGauge_ZeroLive_Allows(t *testing.T) {
	e := newRecountEngine(t)
	seedGauge(t, e, "o", 5) // stuck-high cache
	var drift []DriftInfo
	e.OnDrift = func(_ context.Context, d DriftInfo) { drift = append(drift, d) }
	e.ObservedUsage = func(_ context.Context, _ string) (map[string]int, error) {
		return map[string]int{"compute.concurrent": 0}, nil // 0 live
	}
	res, err := e.Check(context.Background(), CheckInput{OrgID: "o", ResourceKey: "compute.concurrent"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision == Deny {
		t.Fatalf("stale-high with 0 live must ALLOW after recount, got %+v", res)
	}
	if got := rcGaugeVal(t, e, "o"); got != 0 {
		t.Fatalf("counter should be repaired to 0, got %v", got)
	}
	if len(drift) != 1 || drift[0].ResourceType != "gauge" || drift[0].Source != "provider" {
		t.Fatalf("expected one gauge/provider drift, got %+v", drift)
	}
}

func TestRecount_GenuinelyOverGauge_Denies(t *testing.T) {
	e := newRecountEngine(t)
	seedGauge(t, e, "o", 5)
	e.ObservedUsage = func(_ context.Context, _ string) (map[string]int, error) {
		return map[string]int{"compute.concurrent": 5}, nil // truly 5 live, limit 2
	}
	res, err := e.Check(context.Background(), CheckInput{OrgID: "o", ResourceKey: "compute.concurrent"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != Deny {
		t.Fatalf("genuinely over must DENY, got %+v", res)
	}
}

func TestRecount_NoProvider_KeepsDeny(t *testing.T) {
	e := newRecountEngine(t) // no ObservedUsage wired
	seedGauge(t, e, "o", 5)
	res, err := e.Check(context.Background(), CheckInput{OrgID: "o", ResourceKey: "compute.concurrent"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != Deny {
		t.Fatalf("no truth source → behaviour unchanged (deny), got %+v", res)
	}
}

// ---- Phase 1: recount-before-deny (accumulator) --------------------------

func TestRecount_AccumulatorOverCacheButLedgerUnder_Allows(t *testing.T) {
	e := newRecountEngine(t)
	seedAcc(t, e, "o", 15) // cache over the 10 limit
	var drift []DriftInfo
	e.OnDrift = func(_ context.Context, d DriftInfo) { drift = append(drift, d) }
	e.AccumulatorUsage = func(_ context.Context, _ string) (map[string]float64, error) {
		return map[string]float64{"billing.monthly_spend": 5}, nil // durable ledger sums to 5
	}
	res, err := e.Check(context.Background(), CheckInput{OrgID: "o", ResourceKey: "billing.monthly_spend", Cost: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision == Deny {
		t.Fatalf("accumulator ledger under limit must ALLOW, got %+v", res)
	}
	if len(drift) != 1 || drift[0].ResourceType != "accumulator" || drift[0].Source != "ledger" {
		t.Fatalf("expected one accumulator/ledger drift, got %+v", drift)
	}
}

// ---- Phase 1.2: fail-open on drift ---------------------------------------

func TestRecount_TruthDown_FailsOpen(t *testing.T) {
	e := newRecountEngine(t)
	seedGauge(t, e, "o", 5)
	var unreachable bool
	e.OnDrift = func(_ context.Context, d DriftInfo) {
		if d.Source == "provider_unreachable" {
			unreachable = true
		}
	}
	e.ObservedUsage = func(_ context.Context, _ string) (map[string]int, error) {
		return nil, errors.New("provider DB down")
	}
	res, err := e.Check(context.Background(), CheckInput{OrgID: "o", ResourceKey: "compute.concurrent"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision == Deny {
		t.Fatalf("truth-source down must fail OPEN, got %+v", res)
	}
	if res.Reason != "recount_fail_open" {
		t.Fatalf("expected recount_fail_open, got %q", res.Reason)
	}
	if !unreachable {
		t.Fatalf("fail-open must surface a provider_unreachable signal")
	}
}

func TestRecount_AccumulatorTruthDown_KeepsDeny(t *testing.T) {
	// Money-safety: an ACCUMULATOR cache is the durable ledger sum (no lost-
	// decrement drift). On a truth-source outage it must NOT fail open — keep the
	// deny (fail-to-last-known), with a distinct reason + a provider_unreachable alert.
	e := newRecountEngine(t)
	seedAcc(t, e, "o", 15) // over the 10 cap
	var unreachable bool
	e.OnDrift = func(_ context.Context, d DriftInfo) {
		if d.Source == "provider_unreachable" {
			unreachable = true
		}
	}
	e.AccumulatorUsage = func(_ context.Context, _ string) (map[string]float64, error) {
		return nil, errors.New("ledger source down")
	}
	res, err := e.Check(context.Background(), CheckInput{OrgID: "o", ResourceKey: "billing.monthly_spend", Cost: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != Deny {
		t.Fatalf("accumulator source down must KEEP the deny (no overspend), got %+v", res)
	}
	if res.Reason != "recount_source_unavailable_kept_deny" {
		t.Fatalf("expected recount_source_unavailable_kept_deny, got %q", res.Reason)
	}
	if !unreachable {
		t.Fatalf("kept-deny must still surface a provider_unreachable signal")
	}
}

// ---- Phase 2: ReconcileOrg -----------------------------------------------

func TestReconcileOrg_HealsGaugeAndAccumulator(t *testing.T) {
	e := newRecountEngine(t)
	seedGauge(t, e, "o", 5)
	seedAcc(t, e, "o", 15)
	e.ObservedUsage = func(_ context.Context, _ string) (map[string]int, error) {
		return map[string]int{"compute.concurrent": 1}, nil
	}
	e.AccumulatorUsage = func(_ context.Context, _ string) (map[string]float64, error) {
		return map[string]float64{"billing.monthly_spend": 3}, nil
	}
	report, err := e.ReconcileOrg(context.Background(), "o", "")
	if err != nil {
		t.Fatal(err)
	}
	g := report.Resources["compute.concurrent"]
	if g.Before != 5 || g.After != 1 || !g.Changed || g.Status != "repaired" || g.Source != "provider" {
		t.Fatalf("gauge entry wrong: %+v", g)
	}
	a := report.Resources["billing.monthly_spend"]
	if a.Before != 15 || a.After != 3 || !a.Changed || a.Status != "repaired" || a.Source != "ledger" {
		t.Fatalf("accumulator entry wrong: %+v", a)
	}
	if report.Resources["api.requests"].Status != "skipped_rate" {
		t.Fatalf("rate must be skipped: %+v", report.Resources["api.requests"])
	}
	if got := rcGaugeVal(t, e, "o"); got != 1 {
		t.Fatalf("gauge should be repaired to 1, got %v", got)
	}
}

func TestReconcileOrg_Idempotent(t *testing.T) {
	e := newRecountEngine(t)
	seedGauge(t, e, "o", 5)
	e.ObservedUsage = func(_ context.Context, _ string) (map[string]int, error) {
		return map[string]int{"compute.concurrent": 1}, nil
	}
	first, _ := e.ReconcileOrg(context.Background(), "o", "compute.concurrent")
	if !first.Resources["compute.concurrent"].Changed {
		t.Fatal("first pass should change")
	}
	second, _ := e.ReconcileOrg(context.Background(), "o", "compute.concurrent")
	s := second.Resources["compute.concurrent"]
	if s.Changed || s.Status != "in_sync" || s.Before != 1 || s.After != 1 {
		t.Fatalf("second pass should be in_sync no-op, got %+v", s)
	}
}

// ---- Phase 2.2: read-repair ----------------------------------------------

func TestMaybeReadRepair_RepairsThenThrottles(t *testing.T) {
	e := newRecountEngine(t)
	e.ReadRepairThrottle = 5 * time.Minute
	seedGauge(t, e, "o", 5)
	e.ObservedUsage = func(_ context.Context, _ string) (map[string]int, error) {
		return map[string]int{"compute.concurrent": 0}, nil
	}
	e.MaybeReadRepair(context.Background(), "o")
	if got := rcGaugeVal(t, e, "o"); got != 0 {
		t.Fatalf("read-repair should heal to 0, got %v", got)
	}
	// inject drift again; within the throttle window it must NOT repair
	seedGauge(t, e, "o", 7)
	e.MaybeReadRepair(context.Background(), "o")
	if got := rcGaugeVal(t, e, "o"); got != 7 {
		t.Fatalf("throttled read-repair must leave the stale value, got %v", got)
	}
}

// ---- Phase 1: acquire recount --------------------------------------------

func TestAcquireRecount_StaleHighAdmitsAfterRepair(t *testing.T) {
	e := newRecountEngine(t)
	seedGauge(t, e, "o", 5)
	e.ObservedUsage = func(_ context.Context, _ string) (map[string]int, error) {
		return map[string]int{"compute.concurrent": 0}, nil
	}
	res, err := e.Acquire(context.Background(), AcquireInput{OrgID: "o", ResourceKey: "compute.concurrent"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Admitted {
		t.Fatalf("acquire must admit after recount-repair, got %+v", res)
	}
	if got := rcGaugeVal(t, e, "o"); got != 1 {
		t.Fatalf("repaired(0) then spent(1) → expect 1, got %v", got)
	}
}

func TestAcquireRecount_TruthDownFailsOpen(t *testing.T) {
	e := newRecountEngine(t)
	seedGauge(t, e, "o", 5)
	e.ObservedUsage = func(_ context.Context, _ string) (map[string]int, error) {
		return nil, errors.New("down")
	}
	res, err := e.Acquire(context.Background(), AcquireInput{OrgID: "o", ResourceKey: "compute.concurrent"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Admitted {
		t.Fatalf("acquire must fail OPEN when truth is down, got %+v", res)
	}
}
