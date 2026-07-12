package engine

// W-T3 (ticket 20260709_ab0t_quota_systemic_integrity_redesign) — numeric
// edge cases + cross-runtime semantic agreement on the engine surface.
// Governing rule D-31: a weird input (or an IO error) may never silently
// widen a limit or erase a spend; over-count / deny is the only acceptable
// silent direction. Expectations here were derived by EXECUTING the Python
// reference (shared/ab0t-quota, fakeredis), not by reading it.
//
// Defects these tests were RED against (pre-fix — red→green evidence in
// information_tests_lua_crossruntime_20260710.md):
//   GT-01  Acquire swallowed an activation-store persist failure (slog.Warn)
//          and returned admitted=true + an ActivationID whose row does not
//          exist. Python re-raises (D-27/D-28 fail-closed). The Go caller
//          provisions; ReleaseActivation(id) later finds no row → the gauge
//          spend is stranded AND the ledger is missing a row, so the
//          reconciler converges the counter DOWN below the live resource —
//          under-count / phantom headroom, the forbidden direction. This was
//          the exact D-28 Python defect, alive in Go.
//   GT-02  Spend with a negative Cost applied it raw (IncrByFloat(-3)) —
//          ERASING spend. Python's increment is magnitude semantics
//          (increment(-3) ⇒ +3). Divergence in the forbidden direction.
//   GT-03  legacy Release with a negative Cost INCREASED the gauge
//          (DecrByFloorZero negates it) where Python's decrement(-2)
//          decrements by 2. Silent cross-runtime divergence (safe direction,
//          but a mixed fleet disagrees on the same call).
//   GT-04  Spend on a RATE with Cost=N recorded exactly ONE event where
//          Python records int(N) events — under-count ⇒ a silently WIDENED
//          rate limit in Go (forbidden direction).
//   GT-05  a non-finite Cost (NaN/±Inf) flowed into INCRBYFLOAT /
//          crossed-limit comparisons unvalidated.
//
// Emulator caveat: miniredis (gopher-lua) throughout — never a real Redis.

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/ab0t-com/ab0t-quota-go/activations"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/registry"
)

// failingActivationStore errors on PutOpen — the D-28 crash-window injection.
type failingActivationStore struct {
	activations.Store
}

func (f *failingActivationStore) PutOpen(ctx context.Context, a activations.Activation) error {
	return errors.New("injected: activation store unavailable")
}

// GT-01 — Acquire must FAIL CLOSED when the activation row cannot be
// persisted (Python parity: engine.py acquire re-raises, D-27/D-28).
// The caller must NOT provision; the orphaned counter spend is an OVER-count
// that the reconciler heals to Σ open — never an under-count.
func TestAcquire_PersistFailure_FailsClosed_WT3(t *testing.T) {
	fs := counters.NewMemoryFactory("quota")
	store := &failingActivationStore{Store: activations.NewInMemoryStore()}
	e := activationEngine(t, fs, store, 10)
	ctx := context.Background()

	res, err := e.Acquire(ctx, AcquireInput{OrgID: "o1", BundleName: "", ResourceKey: "sandboxes"})
	if err == nil {
		t.Fatalf("GT-01 (D-28 in Go): Acquire returned err=nil (admitted=%v, id=%q) "+
			"despite the activation row never persisting — the caller will "+
			"provision an unreleasable resource and the reconciler will later "+
			"erase its slot (phantom headroom)", res.Admitted, res.ActivationID)
	}
	if res.Admitted || res.ActivationID != "" {
		t.Errorf("GT-01: a failed acquire must not report admitted (%+v)", res)
	}
	// The orphaned spend is allowed to remain (over-count = safe direction):
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	v, _, _ := fs.Floats.GetFloat(ctx, g.OrgKey("o1"))
	if v < 0 {
		t.Errorf("gauge negative after failed acquire: %v", v)
	}
}

// GT-02 — Spend with a negative Cost must never erase spend. Python
// reference (executed): gauge at 5, increment(-3) ⇒ 8 (magnitude).
func TestSpend_NegativeCost_NeverErasesSpend_WT3(t *testing.T) {
	e, fs := d24Engine(t, "")
	ctx := context.Background()
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	_ = fs.Floats.Set(ctx, g.OrgKey("o1"), 5, 0)

	v, err := e.Spend(ctx, CheckInput{OrgID: "o1", ResourceKey: "sandboxes", Cost: -3})
	if err != nil {
		t.Fatalf("Spend(-3): %v", err)
	}
	if v != 8 {
		t.Fatalf("GT-02 (D-31): Spend(Cost=-3) on gauge=5 yielded %v — Python "+
			"reference is 8 (magnitude); a sign flip must never erase spend", v)
	}
}

// GT-03 — legacy Release with a negative Cost: Python decrement(-2) on
// gauge=5 ⇒ 3 (magnitude). Go pre-fix INCREASED the gauge to 7.
func TestRelease_NegativeCost_IsMagnitude_WT3(t *testing.T) {
	e, fs := d24Engine(t, "")
	ctx := context.Background()
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	_ = fs.Floats.Set(ctx, g.OrgKey("o1"), 5, 0)

	if err := e.Release(ctx, CheckInput{OrgID: "o1", ResourceKey: "sandboxes", Cost: -2}); err != nil {
		t.Fatalf("Release(-2): %v", err)
	}
	v, _, _ := fs.Floats.GetFloat(ctx, g.OrgKey("o1"))
	if v != 3 {
		t.Fatalf("GT-03 (cross-runtime divergence): Release(Cost=-2) on gauge=5 "+
			"yielded %v — Python reference is 3 (magnitude semantics)", v)
	}
}

// GT-05 — a non-finite Cost must fail LOUD and mutate nothing.
func TestSpend_NonFiniteCost_RejectedLoud_WT3(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		e, fs := d24Engine(t, "")
		ctx := context.Background()
		g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
		_ = fs.Floats.Set(ctx, g.OrgKey("o1"), 5, 0)

		_, err := e.Spend(ctx, CheckInput{OrgID: "o1", ResourceKey: "sandboxes", Cost: bad})
		if err == nil {
			t.Fatalf("GT-05 (D-31): Spend(Cost=%v) succeeded — non-finite deltas "+
				"must be rejected before they reach the store", bad)
		}
		v, _, _ := fs.Floats.GetFloat(ctx, g.OrgKey("o1"))
		if v != 5 {
			t.Errorf("Spend(Cost=%v): gauge mutated to %v (want 5 untouched)", bad, v)
		}
		if err := e.Release(ctx, CheckInput{OrgID: "o1", ResourceKey: "sandboxes", Cost: bad}); err == nil {
			t.Errorf("GT-05: Release(Cost=%v) succeeded — must reject", bad)
		}
	}
}

// GT-04 — rate Spend with Cost=N must record N events (Python reference,
// executed: rate.increment(3) adds 3 members). Recording 1 under-counts ⇒
// the rate limit is silently WIDER in Go than in Python on the same keyspace.
func TestSpend_RateCostN_RecordsNEvents_WT3(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	fs, err := counters.NewRedisFactory(client, "quota")
	if err != nil {
		t.Fatal(err)
	}
	e := rateEngineWT3(t, fs)
	ctx := context.Background()

	v, err := e.Spend(ctx, CheckInput{OrgID: "o1", ResourceKey: "api.calls", Cost: 3})
	if err != nil {
		t.Fatalf("Spend(rate, 3): %v", err)
	}
	if v != 3 {
		t.Fatalf("GT-04 (D-31/undercount): Spend(rate, Cost=3) counted %v events — "+
			"Python reference records 3; recording fewer silently widens the rate limit", v)
	}

	// Python parity on the truncation edges (executed reference):
	// int(0.5) == 0 events; int(-2) ⇒ no events. Both are no-ops.
	v, err = e.Spend(ctx, CheckInput{OrgID: "o1", ResourceKey: "api.calls", Cost: 0.5})
	if err != nil {
		t.Fatalf("Spend(rate, 0.5): %v", err)
	}
	if v != 3 {
		t.Errorf("Spend(rate, 0.5): count %v (want 3 — fractional events truncate, Python parity)", v)
	}
	v, err = e.Spend(ctx, CheckInput{OrgID: "o1", ResourceKey: "api.calls", Cost: -2})
	if err != nil {
		t.Fatalf("Spend(rate, -2): %v", err)
	}
	if v != 3 {
		t.Errorf("Spend(rate, -2): count %v (want 3 — negative event counts are no-ops, Python parity)", v)
	}
}

func rateEngineWT3(t *testing.T, fs *counters.Factory) *Engine {
	t.Helper()
	e, _ := d24Engine(t, "")
	// Extend the registry with a rate resource + swap in the redis factory.
	e.Cfg.Resources = append(e.Cfg.Resources,
		config.ResourceDef{ResourceKey: "api.calls", CounterType: config.CounterRate, WindowSeconds: 3600})
	e.Reg = registry.New(e.Cfg)
	e.Factory = fs
	return e
}
