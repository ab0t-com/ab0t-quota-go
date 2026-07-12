package quota

// D3/D4/(f) — the reconciler is CONSTRUCTED by Setup (not "later leg"), its
// state is VISIBLE in Capabilities, absence is OFF (never silently healthy),
// the heartbeat is STARTED (not just Stop-able), and Healthy() fails closed.

import (
	"context"
	"strings"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

// No org source → reconciler OFF, and Capabilities SAYS so (absence is OFF).
func TestSetup_ReconcilerOff_NotRequested_D3(t *testing.T) {
	q, err := Setup(context.Background(), Options{ConfigOverride: minimalConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	if q.Capabilities().Reconciler != "OFF — not requested" {
		t.Errorf("reconciler should report OFF — not requested, got %q", q.Capabilities().Reconciler)
	}
	if q.Reconciler != nil {
		t.Error("reconciler should be nil when not requested")
	}
}

// Org source requested but the activation ledger is non-durable (in-memory,
// the default without DDB) → reconciler OFF with the D-39 reason, and the
// reconciler is NOT constructed. Absence of durability = OFF, never healthy.
func TestSetup_ReconcilerOff_NonDurableLedger_D39(t *testing.T) {
	q, err := Setup(context.Background(), Options{
		ConfigOverride: minimalConfig(),
		ReconcileOrgs:  func() []string { return []string{"o1"} },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	if !strings.Contains(q.Capabilities().Reconciler, "not durable") {
		t.Errorf("reconciler should report OFF — not durable (D-39), got %q", q.Capabilities().Reconciler)
	}
	if q.Reconciler != nil {
		t.Error("reconciler must not run on a non-durable ledger")
	}
}

// (f) Healthy() fails when the reconciler is degraded (absence = unknown =
// unhealthy, D-51). A non-durable ledger with an org source requested is a
// degraded reconciler → Healthy() is false and names the reason.
func TestSetup_Healthy_FailsOnDegradedReconciler_F(t *testing.T) {
	q, err := Setup(context.Background(), Options{
		ConfigOverride: minimalConfig(),
		ReconcileOrgs:  func() []string { return []string{"o1"} },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	ok, reasons := q.Healthy()
	if ok {
		t.Error("Healthy() must be false when the reconciler is degraded (D-51)")
	}
	if !strings.Contains(reasons["reconciler"], "not durable") {
		t.Errorf("health reasons should name the reconciler problem, got %v", reasons)
	}
}

// A not-paid service with no reconciler requested is healthy (both subsystems
// are in a known, acceptable state — the negative control for Healthy()).
func TestSetup_Healthy_TrueWhenAllKnown_F(t *testing.T) {
	q, err := Setup(context.Background(), Options{ConfigOverride: minimalConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	if ok, reasons := q.Healthy(); !ok {
		t.Errorf("a not-paid, reconciler-not-requested service should be healthy, got false: %v", reasons)
	}
}

// D4 — the heartbeat is STARTED when billing is wired (not just Stop-able).
func TestSetup_HeartbeatStarted_WhenBillingWired_D4(t *testing.T) {
	t.Setenv("AB0T_QUOTA_BILLING_URL", "http://billing.test")
	c := minimalConfig()
	c.Outbox = config.OutboxConfig{AllowEphemeral: true} // billing wired ⇒ paid; allow start
	q, err := Setup(context.Background(), Options{ConfigOverride: c})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	if q.Heartbeat == nil {
		t.Error("D4/QB-04: heartbeat must be constructed + started when billing is wired")
	}
}
