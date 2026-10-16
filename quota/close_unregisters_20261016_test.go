package quota

// Ticket 20261016 review C-09 (lane C, re-verified): Setup registers the default
// credit-grant handler in authevents' PACKAGE-LEVEL registry and Close never
// removed it, so every Setup added one more — a closed Quota's handler (old
// config, old granter) kept running on every event. RED on v0.1.9.

import (
	"context"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/shopspring/decimal"
)

func TestClose_RemovesTheCreditGrantHandler_20261016(t *testing.T) {
	authevents.ClearHandlers()
	defer authevents.ClearHandlers()
	cfg := minimalConfig()
	cfg.Tiers[0].CreditGrant = &config.CreditGrant{Trigger: config.CreditTriggerSignup,
		AmountPerPeriod: config.Decimal{Decimal: decimal.NewFromInt(1)}, Dedup: config.DedupPerUserPerTier}
	gr := granterFunc(func(context.Context, authevents.CreditGrantRequest) error { return nil })
	for i := 0; i < 3; i++ {
		q, err := Setup(context.Background(), Options{ConfigOverride: cfg, CreditGranter: gr, SkipBackgroundLoops: true})
		if err != nil {
			t.Fatal(err)
		}
		if n := len(authevents.DefaultRegistry().Handlers("org.created")); n != 1 {
			t.Fatalf("Setup #%d: %d org.created handlers registered, want 1", i+1, n)
		}
		_ = q.Close(context.Background())
		if n := len(authevents.DefaultRegistry().Handlers("org.created")); n != 0 {
			t.Fatalf("after Close #%d: %d handlers still registered, want 0", i+1, n)
		}
	}
}
