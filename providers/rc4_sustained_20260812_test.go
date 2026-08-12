package providers

// RC4 — sustained tier-lookup-failure detection (Go parity with Python's
// AuthServiceTierProvider streaks). Ticket
// 20260810_billing_resilience_and_erroneous_tier_downgrade TICKET.md P5;
// verify_fabel_5_report_20260812. Detection-only: serving is unchanged — the
// LKG tests cover what is served; these cover that a sustained outage is
// COUNTED (and therefore escalated), and that success resets the streak.

import (
	"context"
	"testing"
	"time"
)

func TestRC4_StreakCountsAndResetsOnSuccess(t *testing.T) {
	old := SustainedFailureThreshold
	SustainedFailureThreshold = 3
	defer func() { SustainedFailureThreshold = old }()

	inner := &meshLike{tier: "pro", deflt: "free", fail: true}
	c := WithCache(inner, time.Minute)

	for i := 0; i < 4; i++ {
		if _, err := c.GetTier(context.Background(), "u1", "org1"); err != nil {
			t.Fatalf("outage serving must not error (fallback default): %v", err)
		}
	}
	if got := c.ConsecutiveFailures("u1", "org1"); got != 4 {
		t.Fatalf("streak: want 4, got %d", got)
	}

	inner.fail = false
	if tier, _ := c.GetTier(context.Background(), "u1", "org1"); tier != "pro" {
		t.Fatalf("recovery fetch: want pro, got %q", tier)
	}
	if got := c.ConsecutiveFailures("u1", "org1"); got != 0 {
		t.Fatalf("streak must reset on success, got %d", got)
	}
}

func TestRC4_StreaksArePerKey(t *testing.T) {
	inner := &meshLike{deflt: "free", fail: true}
	c := WithCache(inner, time.Minute)
	_, _ = c.GetTier(context.Background(), "u1", "org1")
	_, _ = c.GetTier(context.Background(), "u1", "org1")
	_, _ = c.GetTier(context.Background(), "u2", "org2")
	if c.ConsecutiveFailures("u1", "org1") != 2 || c.ConsecutiveFailures("u2", "org2") != 1 {
		t.Fatalf("per-key streaks wrong: %d / %d",
			c.ConsecutiveFailures("u1", "org1"), c.ConsecutiveFailures("u2", "org2"))
	}
}

func TestRC4_TTLDisabledBranchAlsoTracks(t *testing.T) {
	inner := &meshLike{tier: "pro", deflt: "free", fail: true}
	c := WithCache(inner, 0) // caching disabled branch
	_, _ = c.GetTier(context.Background(), "u1", "org1")
	_, _ = c.GetTier(context.Background(), "u1", "org1")
	if got := c.ConsecutiveFailures("u1", "org1"); got != 2 {
		t.Fatalf("ttl<=0 branch must track failures too, got %d", got)
	}
	inner.fail = false
	_, _ = c.GetTier(context.Background(), "u1", "org1")
	if got := c.ConsecutiveFailures("u1", "org1"); got != 0 {
		t.Fatalf("ttl<=0 branch must reset on success, got %d", got)
	}
}
