package reconcile

// K-8 — the reconciler must inherit the factory's keyspace (spec §6.3): a
// reconciler blind to v2 would read the WRONG shape (always 0), "heal" a
// healthy counter to Σ open under the v1 key nobody reads, and the real v2
// gauge would drift forever — the silent reconciler outage the spec names.

import (
	"context"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/activations"
	"github.com/ab0t-com/ab0t-quota-go/counters"
)

// RK1: with a v2 keyspace, convergence reads AND force-sets the v2 key.
func TestReconcileKeyspaceV2ConvergesV2Key(t *testing.T) {
	ctx := context.Background()
	fs := counters.NewMemoryFactory("quota")
	ks, err := counters.NewKeyspace("test-svc", 2, false)
	if err != nil {
		t.Fatal(err)
	}
	fs.Keyspace = ks
	v2Key := "quota:v2:{test-svc/o1}:sandboxes:gauge"
	_, _ = fs.Floats.IncrByFloat(ctx, v2Key, 99) // drift, in the LIVE shape
	ds := durableStore{activations.NewInMemoryStore()}
	openN(ds, "o1", "sandboxes", 3)
	r := &Reconciler{Store: ds, Factory: fs, Reg: testReg(t), RecentlyTouched: noGuard}
	if _, err := r.ReconcileOrg(ctx, "o1"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := fs.Floats.GetFloat(ctx, v2Key); v != 3 {
		t.Errorf("v2 gauge must converge to Σ open=3, got %v — the reconciler is "+
			"blind to the engine's keyspace (spec §6.3)", v)
	}
	if v, ok, _ := fs.Floats.GetFloat(ctx, "quota:o1:sandboxes:gauge"); ok && v != 0 {
		t.Errorf("reconciler wrote the v1 shape nobody reads: %v", v)
	}
}

// RK2: during dual, a force-set maintains BOTH shapes (Python gauge.reset
// parity) — a single-shape force-set re-diverges the twin it skipped.
func TestReconcileDualForceSetsBothShapes(t *testing.T) {
	ctx := context.Background()
	fs := counters.NewMemoryFactory("quota")
	ks, err := counters.NewKeyspace("test-svc", 1, true)
	if err != nil {
		t.Fatal(err)
	}
	fs.Keyspace = ks
	v1Key := "quota:o1:sandboxes:gauge"
	v2Key := "quota:v2:{test-svc/o1}:sandboxes:gauge"
	_, _ = fs.Floats.IncrByFloat(ctx, v1Key, 99)
	_, _ = fs.Floats.IncrByFloat(ctx, v2Key, 99)
	ds := durableStore{activations.NewInMemoryStore()}
	openN(ds, "o1", "sandboxes", 3)
	r := &Reconciler{Store: ds, Factory: fs, Reg: testReg(t), RecentlyTouched: noGuard}
	if _, err := r.ReconcileOrg(ctx, "o1"); err != nil {
		t.Fatal(err)
	}
	v1v, _, _ := fs.Floats.GetFloat(ctx, v1Key)
	v2v, _, _ := fs.Floats.GetFloat(ctx, v2Key)
	if v1v != 3 || v2v != 3 {
		t.Errorf("dual force-set must maintain BOTH shapes: v1=%v v2=%v want 3/3", v1v, v2v)
	}
}
