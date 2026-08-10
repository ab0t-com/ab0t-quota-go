package reconcile

// P4.1 (ticket 20260810) — the reconciler emits the resolve signal (OnClean)
// for a gauge at the authoritative level and the drift signal (OnDrift) for a
// drifted one. This is the reconciler half of the drift-metric feedback loop;
// the Manager half (metric emission + sustained escalation) is tested in
// package alerts.

import (
	"context"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/activations"
	"github.com/ab0t-com/ab0t-quota-go/counters"
)

func TestReconcile_OnClean_FiresForCleanGauge(t *testing.T) {
	fs := counters.NewMemoryFactory("quota")
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	ds := durableStore{activations.NewInMemoryStore()}
	// Counter already AT the authoritative level (Σ open = 2, counter = 2): clean.
	openN(ds, "o1", "sandboxes", 2)
	_, _ = fs.Floats.IncrByFloat(context.Background(), g.OrgKey("o1"), 2)

	var cleanCalls, driftCalls int
	r := &Reconciler{
		Store: ds, Factory: fs, Reg: testReg(t), RecentlyTouched: noGuard,
		OnClean: func(_, _ string) { cleanCalls++ },
		OnDrift: func(DriftAlert) { driftCalls++ },
	}
	if _, err := r.ReconcileOrg(context.Background(), "o1"); err != nil {
		t.Fatal(err)
	}
	if cleanCalls != 1 {
		t.Errorf("OnClean should fire once for the clean gauge; got %d", cleanCalls)
	}
	if driftCalls != 0 {
		t.Errorf("no drift → OnDrift must not fire; got %d", driftCalls)
	}
}

func TestReconcile_OnDrift_FiresForDriftedGauge(t *testing.T) {
	fs := counters.NewMemoryFactory("quota")
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	ds := durableStore{activations.NewInMemoryStore()}
	openN(ds, "o1", "sandboxes", 1)
	_, _ = fs.Floats.IncrByFloat(context.Background(), g.OrgKey("o1"), 5) // drift: 5 vs Σ open 1

	var cleanCalls int
	var drift []DriftAlert
	r := &Reconciler{
		Store: ds, Factory: fs, Reg: testReg(t), RecentlyTouched: noGuard,
		OnClean: func(_, _ string) { cleanCalls++ },
		OnDrift: func(a DriftAlert) { drift = append(drift, a) },
	}
	if _, err := r.ReconcileOrg(context.Background(), "o1"); err != nil {
		t.Fatal(err)
	}
	if len(drift) != 1 || drift[0].Source != "activations" {
		t.Fatalf("expected one activations drift, got %+v", drift)
	}
	if drift[0].CounterWas != 5 || drift[0].ConvergedTo != 1 {
		t.Errorf("drift before/after wrong: was=%v to=%v", drift[0].CounterWas, drift[0].ConvergedTo)
	}
	if cleanCalls != 0 {
		t.Errorf("a drifted gauge must not fire OnClean; got %d", cleanCalls)
	}
}
