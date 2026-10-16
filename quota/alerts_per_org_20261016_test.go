package quota

// Ticket 20261016 review C-11 / A-33 (re-verified at alerts/manager.go:94): the
// threshold-alert cooldown key was level+resource+tier — no org. One tenant
// crossing its warning threshold muted the same alert for EVERY other tenant on
// that tier for cooldown_seconds (default 1h). RED on v0.1.9 (1 alert, want 2).

import (
	"context"
	"sync"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/alerts"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/engine"
)

type recordingDispatcher struct {
	mu   sync.Mutex
	sent []engine.Result
}

func (d *recordingDispatcher) Send(_ context.Context, _ alerts.Level, r engine.Result) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, r)
	return nil
}

func TestAlerts_CooldownIsPerTenant_20261016(t *testing.T) {
	cfg := minimalConfig()
	cfg.Alerts.Enabled = true
	cfg.Alerts.CooldownSeconds = 3600
	cfg.Tiers[0].Limits["sandbox.concurrent"] = config.TierLimit{Limit: ptrFloat(2), WarningThreshold: 0.8, CriticalThreshold: 0.95}
	q, err := Setup(context.Background(), Options{ConfigOverride: cfg, SkipBackgroundLoops: true})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	rec := &recordingDispatcher{}
	q.Alerts.Dispatcher = rec

	ctx := context.Background()
	for _, org := range []string{"org-a", "org-b"} {
		in := engine.CheckInput{UserID: "alice", OrgID: org, ResourceKey: "sandbox.concurrent", Cost: 1}
		if _, err := q.Spend(ctx, in); err != nil {
			t.Fatal(err)
		}
		res, err := q.Check(ctx, in) // 1 used + 1 = 2/2 → critical
		if err != nil {
			t.Fatal(err)
		}
		if res.Decision != engine.Critical && res.Decision != engine.Warn {
			t.Fatalf("%s: decision %v, want warn/critical", org, res.Decision)
		}
	}
	if len(rec.sent) != 2 {
		t.Fatalf("%d threshold alert(s) for two tenants, want 2 — one tenant's alert muted the other's", len(rec.sent))
	}
}
