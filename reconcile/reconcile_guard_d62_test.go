package reconcile

// D-62 — the recent-activity guard is a live fail-open when absent. A resource
// is created → counter=1; the consumer's product row hasn't appeared yet →
// provider reports 0; the reconciler converges the counter DOWN to 0 → the org
// holds a live resource nothing counts → under-count → phantom headroom →
// over-admission, on EVERY fast create. This is the 20260626 production
// incident. A nil guard is NOT "no guard configured"; it is "reconcile against
// a provider that lags reality". The reconciler refuses without a guard.

import (
	"context"
	"errors"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/activations"
	"github.com/ab0t-com/ab0t-quota-go/counters"
)

// Refuse-gate: a durable ledger but NO guard → refuse; counter untouched.
func TestReconcile_RefusesWithoutGuard_D62(t *testing.T) {
	fs := counters.NewMemoryFactory("quota")
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	_, _ = fs.Floats.IncrByFloat(context.Background(), g.OrgKey("o1"), 1) // a just-created resource
	ds := durableStore{activations.NewInMemoryStore()}

	// nil RecentlyTouched — a durable ledger, a provider, but no guard.
	r := &Reconciler{
		Store: ds, Factory: fs, Reg: testReg(t),
		Provider: func(_ context.Context, _ string) (map[string]int, error) {
			return map[string]int{"sandboxes": 0}, nil // provider lags: reports 0
		},
	}
	_, err := r.ReconcileOrg(context.Background(), "o1")
	if !errors.Is(err, ErrNoRecentActivityGuard) {
		t.Fatalf("must refuse without a recent-activity guard (D-62), got %v", err)
	}
	// The just-created resource's counter must be UNTOUCHED (refused → no force-set).
	if v, _, _ := fs.Floats.GetFloat(context.Background(), g.OrgKey("o1")); v != 1 {
		t.Errorf("refused reconcile must not converge a just-created resource down; counter=%v", v)
	}
}

// THE property + its NEGATIVE CONTROL. Same setup (provider lags, reports 0,
// counter=1), run twice:
//
//	guard=true  → the recently-touched resource is NEVER force-set (stays 1).
//	guard=false → WITHOUT the guard protecting it, it converges to 0.
//
// The delta between the two IS the guard's teeth (removing the guard check
// makes the first case go red).
func TestReconcile_RecentlyTouched_NeverForceSet_D62(t *testing.T) {
	run := func(recentlyTouched bool) float64 {
		fs := counters.NewMemoryFactory("quota")
		g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
		_, _ = fs.Floats.IncrByFloat(context.Background(), g.OrgKey("o1"), 1)
		ds := durableStore{activations.NewInMemoryStore()} // ledger has 0 open (row not written yet)
		r := &Reconciler{
			Store: ds, Factory: fs, Reg: testReg(t),
			RecentlyTouched: func(_, _ string) bool { return recentlyTouched },
			Provider: func(_ context.Context, _ string) (map[string]int, error) {
				return map[string]int{"sandboxes": 0}, nil // provider lags reality
			},
		}
		if _, err := r.ReconcileOrg(context.Background(), "o1"); err != nil {
			t.Fatal(err)
		}
		v, _, _ := fs.Floats.GetFloat(context.Background(), g.OrgKey("o1"))
		return v
	}

	if guarded := run(true); guarded != 1 {
		t.Errorf("D-62: a recently-touched (org,resource) must NEVER be force-set; counter=%v (want 1)", guarded)
	}
	// Negative control — proves the guard is what protects it (not something else).
	if unguarded := run(false); unguarded != 0 {
		t.Errorf("negative control broken: without the guard the fast-create SHOULD converge to 0, got %v "+
			"(so the guarded case above proves nothing)", unguarded)
	}
}
