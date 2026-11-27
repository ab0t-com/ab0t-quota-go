package mesh

// v0.2.0 — deprecation-warning cadence unified with the Python SDK: each deprecated env name is
// reported ONCE per process. A name already reported is never repeated; a deprecated name that
// appears later (another service's old variable set after the first Resolve) is still reported,
// once. Before the unification Go logged one line per process in total, so the later name was never
// named.

import (
	"strings"
	"testing"
)

func TestResolve_EachDeprecatedNameReportedOncePerProcess_20261126(t *testing.T) {
	clearEnv(t)
	logs := captureLogs(t)
	t.Setenv("AB0T_QUOTA_BILLING_URL", "https://old-billing.example")
	Resolve()
	Resolve()
	first := logs.String()
	if strings.Count(first, "deprecated env var names") != 1 || !strings.Contains(first, "AB0T_QUOTA_BILLING_URL") {
		t.Fatalf("first name: want ONE line naming AB0T_QUOTA_BILLING_URL, got:\n%s", first)
	}

	// A second deprecated name shows up later in the same process: reported once, alone.
	t.Setenv("AB0T_QUOTA_PAYMENT_URL", "https://old-payment.example")
	Resolve()
	Resolve()
	all := logs.String()
	if got := strings.Count(all, "deprecated env var names"); got != 2 {
		t.Fatalf("want 2 lines in total (one per new name), got %d:\n%s", got, all)
	}
	second := all[len(first):]
	if !strings.Contains(second, "AB0T_QUOTA_PAYMENT_URL") || strings.Contains(second, "AB0T_QUOTA_BILLING_URL") {
		t.Fatalf("the later line must name only the NEW name:\n%s", second)
	}
	if strings.Count(all, "AB0T_QUOTA_BILLING_URL") != 1 || strings.Count(all, "AB0T_QUOTA_PAYMENT_URL") != 1 {
		t.Fatalf("each name must be reported exactly once:\n%s", all)
	}
}

// Control: the reset used by the package tests re-arms every name (so tests stay independent).
func TestResolve_DeprecationResetRearms_20261126(t *testing.T) {
	clearEnv(t)
	logs := captureLogs(t)
	t.Setenv("AB0T_QUOTA_BILLING_URL", "https://old-billing.example")
	Resolve()
	clearEnv(t)
	t.Setenv("AB0T_QUOTA_BILLING_URL", "https://old-billing.example")
	Resolve()
	if got := strings.Count(logs.String(), "AB0T_QUOTA_BILLING_URL"); got != 2 {
		t.Fatalf("after a reset the name is reported again: got %d\n%s", got, logs.String())
	}
}
