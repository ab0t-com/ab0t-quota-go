package reconcile

// D3 — the reconciler is a WORKER; test it by driving the real loop, not by
// calling ReconcileOrg directly. (A public function whose only call site is a
// test is a disconnected guarantee — that is exactly how Go shipped D-28's
// self-heal permanently OFF.) Negative control: a cancelled loop converges
// nothing, proving the harness observes the real goroutine.

import (
	"context"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/activations"
	"github.com/ab0t-com/ab0t-quota-go/counters"
)

func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func TestReconcile_RunLoop_RealWorkerConverges_D3(t *testing.T) {
	fs := counters.NewMemoryFactory("quota")
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	_, _ = fs.Floats.IncrByFloat(context.Background(), g.OrgKey("o1"), 99) // drift
	ds := durableStore{activations.NewInMemoryStore()}
	openN(ds, "o1", "sandboxes", 3)
	r := &Reconciler{Store: ds, Factory: fs, Reg: testReg(t), RecentlyTouched: noGuard}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Drive the REAL loop (fast tick); do NOT call ReconcileOrg directly.
	go r.RunLoop(ctx, 10*time.Millisecond, func() []string { return []string{"o1"} })

	if !waitFor(func() bool {
		v, _, _ := fs.Floats.GetFloat(context.Background(), g.OrgKey("o1"))
		return v == 3
	}, 3*time.Second) {
		v, _, _ := fs.Floats.GetFloat(context.Background(), g.OrgKey("o1"))
		t.Fatalf("real reconciler loop never converged the drifted counter (=%v, want Σ open=3) — dead worker", v)
	}
}

// NEGATIVE CONTROL: a cancelled loop converges nothing → the harness observes
// the real goroutine (if it called ReconcileOrg directly, this would converge).
func TestReconcile_RunLoop_Cancelled_ConvergesNothing_D3(t *testing.T) {
	fs := counters.NewMemoryFactory("quota")
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	_, _ = fs.Floats.IncrByFloat(context.Background(), g.OrgKey("o1"), 99)
	ds := durableStore{activations.NewInMemoryStore()}
	openN(ds, "o1", "sandboxes", 3)
	r := &Reconciler{Store: ds, Factory: fs, Reg: testReg(t), RecentlyTouched: noGuard}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dead loop
	go r.RunLoop(ctx, 10*time.Millisecond, func() []string { return []string{"o1"} })

	time.Sleep(200 * time.Millisecond)
	if v, _, _ := fs.Floats.GetFloat(context.Background(), g.OrgKey("o1")); v != 99 {
		t.Errorf("cancelled loop must converge nothing; counter=%v (harness not observing the real loop)", v)
	}
}

// A misconfigured loop (non-durable ledger) does NOT start (loud-fail, not a
// silently-dead worker) → no convergence.
func TestReconcile_RunLoop_NonDurable_DoesNotStart_D3(t *testing.T) {
	fs := counters.NewMemoryFactory("quota")
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	_, _ = fs.Floats.IncrByFloat(context.Background(), g.OrgKey("o1"), 99)
	r := &Reconciler{Store: activations.NewInMemoryStore(), Factory: fs, Reg: testReg(t), RecentlyTouched: noGuard}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.RunLoop(ctx, 10*time.Millisecond, func() []string { return []string{"o1"} })
	time.Sleep(150 * time.Millisecond)
	if v, _, _ := fs.Floats.GetFloat(context.Background(), g.OrgKey("o1")); v != 99 {
		t.Errorf("loop must not run on a non-durable ledger; counter=%v", v)
	}
}
