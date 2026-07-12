package reconcile

// Tests for the precedence law + refuse gates (D-33/D-36/D-37/D-39/D-31).

import (
	"context"
	"errors"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/activations"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/registry"
)

// durableStore wraps InMemory but reports Durable()=true — so we can test the
// precedence law without a real DDB. (The refuse-gate test uses the real
// in-memory store, which reports Durable()=false.)
type durableStore struct{ *activations.InMemoryStore }

func (durableStore) Durable() bool { return true }

// noGuard is a permissive recent-activity guard (nothing is recently touched)
// so these precedence-law tests exercise convergence. The refuse-without-guard
// behaviour is tested separately (D-62).
var noGuard = func(_, _ string) bool { return false }

func testReg(t *testing.T) *registry.Registry {
	cfg := &config.Config{
		Tiers:     []config.Tier{{TierID: "pro", Limits: map[string]config.TierLimit{}}},
		Resources: []config.ResourceDef{{ResourceKey: "sandboxes", CounterType: config.CounterGauge}, {ResourceKey: "spend", CounterType: config.CounterAccumulator, ResetPeriod: config.ResetMonthly}},
	}
	return registry.New(cfg)
}

func openN(store activations.Store, org, rk string, n int) {
	for i := 0; i < n; i++ {
		_ = store.PutOpen(context.Background(), activations.Activation{
			ActivationID: rk + "-" + string(rune('a'+i)), OrgID: org, ResourceKey: rk,
			State: activations.StateOpen, Spend: map[string]float64{rk: 1}, OpenedAt: "2026-03-15T10:00:00Z",
		})
	}
}

// D-37/D-39: refuse a non-durable ledger; DO NOT force-set. Negative control:
// a durable ledger runs.
func TestReconcile_RefusesNonDurableLedger_D37D39(t *testing.T) {
	fs := counters.NewMemoryFactory("quota")
	// seed a drifted counter that a broken reconciler would "heal" (to 0).
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	_, _ = fs.Floats.IncrByFloat(context.Background(), g.OrgKey("o1"), 99)

	r := &Reconciler{Store: activations.NewInMemoryStore(), Factory: fs, Reg: testReg(t), RecentlyTouched: noGuard}
	_, err := r.ReconcileOrg(context.Background(), "o1")
	if !errors.Is(err, ErrNonDurableLedger) {
		t.Fatalf("must refuse a non-durable (in-memory) ledger, got %v", err)
	}
	// The drifted counter must be UNTOUCHED (refused → did nothing).
	if v, _, _ := fs.Floats.GetFloat(context.Background(), g.OrgKey("o1")); v != 99 {
		t.Errorf("refused reconcile must not force-set; counter changed to %v", v)
	}

	// Negative control: a durable ledger DOES run (converges 99 → Σ open = 2).
	ds := durableStore{activations.NewInMemoryStore()}
	openN(ds, "o1", "sandboxes", 2)
	r2 := &Reconciler{Store: ds, Factory: fs, Reg: testReg(t), RecentlyTouched: noGuard}
	if _, err := r2.ReconcileOrg(context.Background(), "o1"); err != nil {
		t.Fatalf("durable ledger must reconcile: %v", err)
	}
	if v, _, _ := fs.Floats.GetFloat(context.Background(), g.OrgKey("o1")); v != 2 {
		t.Errorf("durable reconcile should converge to Σ open=2, got %v", v)
	}
}

// D-33: no provider → converge counter to Σ open activations.
func TestReconcile_NoProvider_ConvergesToOpen_D33(t *testing.T) {
	fs := counters.NewMemoryFactory("quota")
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	_, _ = fs.Floats.IncrByFloat(context.Background(), g.OrgKey("o1"), 99) // drift
	ds := durableStore{activations.NewInMemoryStore()}
	openN(ds, "o1", "sandboxes", 3)
	r := &Reconciler{Store: ds, Factory: fs, Reg: testReg(t), RecentlyTouched: noGuard}
	alerts, err := r.ReconcileOrg(context.Background(), "o1")
	if err != nil {
		t.Fatal(err)
	}
	if v, _, _ := fs.Floats.GetFloat(context.Background(), g.OrgKey("o1")); v != 3 {
		t.Errorf("converge to Σ open=3, got %v", v)
	}
	if len(alerts) != 1 || alerts[0].Source != "activations" {
		t.Errorf("expected one 'activations' drift alert, got %+v", alerts)
	}
}

// D-36: provider says MORE live than the ledger has open → converge to the
// provider AND raise an unsettleable money incident; never fabricate a row.
func TestReconcile_ProviderDivergence_MoneyIncident_D36(t *testing.T) {
	fs := counters.NewMemoryFactory("quota")
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	_, _ = fs.Floats.IncrByFloat(context.Background(), g.OrgKey("o1"), 3)
	ds := durableStore{activations.NewInMemoryStore()}
	openN(ds, "o1", "sandboxes", 3) // ledger: 3 open
	r := &Reconciler{
		Store: ds, Factory: fs, Reg: testReg(t), RecentlyTouched: noGuard,
		Provider: func(_ context.Context, _ string) (map[string]int, error) {
			return map[string]int{"sandboxes": 5}, nil // reality: 5 live
		},
	}
	alerts, err := r.ReconcileOrg(context.Background(), "o1")
	if err != nil {
		t.Fatal(err)
	}
	if v, _, _ := fs.Floats.GetFloat(context.Background(), g.OrgKey("o1")); v != 5 {
		t.Errorf("must converge to the provider's observed set (5), got %v", v)
	}
	found := false
	for _, a := range alerts {
		if a.ResourceKey == "sandboxes" {
			found = true
			if a.Source != "provider" || a.UnsettleableLive != 2 {
				t.Errorf("expected provider source + 2 unsettleable, got %+v", a)
			}
		}
	}
	if !found {
		t.Error("expected a divergence alert for sandboxes")
	}
	// The ledger still has only 3 rows — NO fabrication (D-36).
	if n, _ := ds.CountOpen(context.Background(), "o1"); n != 3 {
		t.Errorf("reconciler must not fabricate ledger rows; open=%d (want 3)", n)
	}
}

// D-31: provider unreachable → do NOTHING (no force-set) + alert.
func TestReconcile_ProviderUnreachable_DoesNothing_D31(t *testing.T) {
	fs := counters.NewMemoryFactory("quota")
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	_, _ = fs.Floats.IncrByFloat(context.Background(), g.OrgKey("o1"), 42) // drift
	ds := durableStore{activations.NewInMemoryStore()}
	openN(ds, "o1", "sandboxes", 1)
	r := &Reconciler{
		Store: ds, Factory: fs, Reg: testReg(t), RecentlyTouched: noGuard,
		Provider: func(_ context.Context, _ string) (map[string]int, error) { return nil, errors.New("provider down") },
	}
	alerts, err := r.ReconcileOrg(context.Background(), "o1")
	if err != nil {
		t.Fatal(err)
	}
	// Counter UNCHANGED — never converge to the ledger as a fallback.
	if v, _, _ := fs.Floats.GetFloat(context.Background(), g.OrgKey("o1")); v != 42 {
		t.Errorf("provider-unreachable must NOT force-set (would erase reality); counter=%v", v)
	}
	if len(alerts) == 0 || alerts[0].Source != "provider_unreachable" {
		t.Errorf("expected a provider_unreachable alert, got %+v", alerts)
	}
}

// D-33: accumulators are never reconciled.
func TestReconcile_AccumulatorsNeverReconciled_D33(t *testing.T) {
	fs := counters.NewMemoryFactory("quota")
	// A gauge with matching open set (no drift) + an accumulator that must be ignored.
	ds := durableStore{activations.NewInMemoryStore()}
	r := &Reconciler{Store: ds, Factory: fs, Reg: testReg(t), RecentlyTouched: noGuard}
	alerts, err := r.ReconcileOrg(context.Background(), "o1")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range alerts {
		if a.ResourceKey == "spend" {
			t.Error("accumulator 'spend' must never be reconciled")
		}
	}
}
