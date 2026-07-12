package engine

// Claim 1 (QG-06) — the CORRECT concurrent invariant under floor-at-zero,
// asserted my way (new name, peer test left untouched — house rule + D-18).
//
// This is the correctly-stated version of the intent behind the peer test
// TestEngine_ConcurrentReleaseDoesntUnderflow ("doesn't underflow"): under
// concurrent balanced spend/release the gauge must NEVER go negative
// (used >= 0). The peer test additionally asserts an EXACT net of 0, which
// requires transient-negative gauges — precisely the free-headroom bug the
// floor kills — so that assertion is incompatible with the mandated fix
// (and with Python, which also floors). See the framed decision on the
// gauge-floor-vs-peer-test invariant (coordinator to number/adjudicate).

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/messages"
	"github.com/ab0t-com/ab0t-quota-go/providers"
	"github.com/ab0t-com/ab0t-quota-go/registry"
)

func TestGaugeConcurrent_NeverUnderflows_Correct_QG06(t *testing.T) {
	cap := 1_000_000.0
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", Mapping: map[string]string{"alice": "pro"}},
		Tiers:        []config.Tier{{TierID: "pro", Limits: map[string]config.TierLimit{"x": {Limit: &cap}}}},
		Resources:    []config.ResourceDef{{ResourceKey: "x", CounterType: config.CounterGauge}},
	}
	prov, _ := providers.New(cfg.TierProvider)
	e := &Engine{
		Cfg: cfg, Reg: registry.New(cfg), Provider: prov,
		Factory: counters.NewMemoryFactory("quota"), Messages: messages.New(messages.Templates{}),
		Clock: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
	ctx := context.Background()
	in := CheckInput{UserID: "alice", OrgID: "o", ResourceKey: "x", Cost: 1}

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N * 2)
	for i := 0; i < N; i++ {
		go func() { defer wg.Done(); _, _ = e.Spend(ctx, in) }()
		go func() { defer wg.Done(); _ = e.Release(ctx, in) }()
	}
	wg.Wait()

	// The integrity property: usage is NEVER negative, regardless of the
	// spend/release interleaving. (A negative gauge would grant phantom
	// quota headroom — QG-06.)
	res, err := e.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Used < 0 {
		t.Errorf("QG-06: concurrent balanced spend/release drove usage NEGATIVE (%v) — floor failed", res.Used)
	}

	// And an outright over-release floors to exactly 0, not below.
	for i := 0; i < 10; i++ {
		_ = e.Release(ctx, in)
	}
	res, _ = e.Check(ctx, in)
	if res.Used != 0 {
		t.Errorf("QG-06: after heavy over-release, used=%v (want 0 — floored)", res.Used)
	}
}
