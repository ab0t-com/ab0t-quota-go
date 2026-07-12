package engine

// TASK P5.2 tests — the Go activation API (Acquire/ReleaseActivation/Settle).
// Ticket 20260709_ab0t_quota_systemic_integrity_redesign. Claims 1, 2, 5.

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/ab0t-com/ab0t-quota-go/activations"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/messages"
	"github.com/ab0t-com/ab0t-quota-go/providers"
	"github.com/ab0t-com/ab0t-quota-go/registry"
)

func activationEngine(t *testing.T, factory *counters.Factory, store activations.Store, limit float64) *Engine {
	t.Helper()
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", DefaultTier: "pro", Mapping: map[string]string{"alice": "pro", "bob": "pro"}},
		Tiers: []config.Tier{{TierID: "pro", Limits: map[string]config.TierLimit{
			"sandboxes": {Limit: &limit},
		}}},
		Resources: []config.ResourceDef{{ResourceKey: "sandboxes", CounterType: config.CounterGauge}},
	}
	prov, _ := providers.New(cfg.TierProvider)
	return &Engine{
		Cfg: cfg, Reg: registry.New(cfg), Provider: prov, Factory: factory,
		Messages:    messages.New(messages.Templates{}),
		Clock:       func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Activations: store,
	}
}

// Claim 5 — golden-key parity for the activation-related keys.
func TestGoldenKeys_Activation_Parity_P52(t *testing.T) {
	raw, err := os.ReadFile("testdata/golden_keys_python_v052.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ActivationKeys map[string]string `json:"activation_keys"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	ak := doc.ActivationKeys
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	store := activations.NewRedisStore(nil, "", 0)
	_ = store // key builders below are pure

	checks := []struct{ name, got, want string }{
		{"gauge_seq_user", g.UserSeqKey("org-123", "user-1"), ak["gauge_seq_user"]},
		{"acquire_idem", g.IdemKey("org-123", "mykey"), ak["acquire_idem"]},
		{"acquire_idem_unused", g.IdemKey("org-123", ""), ak["acquire_idem_unused"]},
		{"activation_row", "activation:row:act_abc123", ak["activation_row"]},
		{"activation_open_index", "activation:open:org:org-123", ak["activation_open_index"]},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("P5.2 activation key %q: Go %q != Python golden %q", c.name, c.got, c.want)
		}
	}
	if activations.MintActivationID()[:4] != ak["mint_prefix"] {
		t.Errorf("mint prefix mismatch: %q != %q", activations.MintActivationID()[:4], ak["mint_prefix"])
	}
}

// Claim 1 — Acquire atomic check-and-spend: at limit 1, concurrent acquires
// admit EXACTLY ONE (QI-03 TOCTOU killed).
func TestAcquire_AtLimit_ExactlyOneAdmitted_P52(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	fs, _ := counters.NewRedisFactory(c, "quota")
	e := activationEngine(t, fs, activations.NewInMemoryStore(), 1)
	ctx := context.Background()

	const N = 12
	var admitted int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			res, err := e.Acquire(ctx, AcquireInput{OrgID: "o1", ResourceKey: "sandboxes"})
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			if res.Admitted {
				atomic.AddInt64(&admitted, 1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if admitted != 1 {
		t.Errorf("QI-03: %d acquires admitted at limit 1 (want exactly 1) — TOCTOU over-admit", admitted)
	}
	v, _, _ := fs.Floats.GetFloat(ctx, counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}.OrgKey("o1"))
	if v != 1 {
		t.Errorf("gauge = %v after concurrent acquire at limit 1 (want 1)", v)
	}
}

// Claim 1 — ReleaseActivation is idempotent forever on the minted id: a
// DUPLICATE release does NOT double-decrement (QI-04). Two resources active,
// one released twice → gauge = 1, not 0.
func TestReleaseActivation_DuplicateDelivery_AppliesOnce_P52(t *testing.T) {
	fs := counters.NewMemoryFactory("quota")
	store := activations.NewInMemoryStore()
	e := activationEngine(t, fs, store, 10)
	ctx := context.Background()

	r1, err := e.Acquire(ctx, AcquireInput{OrgID: "o1", ResourceKey: "sandboxes"})
	if err != nil || !r1.Admitted || r1.ActivationID == "" {
		t.Fatalf("acquire1: %+v err=%v", r1, err)
	}
	r2, err := e.Acquire(ctx, AcquireInput{OrgID: "o1", ResourceKey: "sandboxes"})
	if err != nil || !r2.Admitted {
		t.Fatalf("acquire2: %+v err=%v", r2, err)
	}
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}
	if v, _, _ := fs.Floats.GetFloat(ctx, g.OrgKey("o1")); v != 2 {
		t.Fatalf("gauge after 2 acquires = %v (want 2)", v)
	}
	// Release activation 1 TWICE (duplicate delivery).
	did1, _ := e.ReleaseActivation(ctx, r1.ActivationID)
	did2, _ := e.ReleaseActivation(ctx, r1.ActivationID)
	if !did1 {
		t.Error("first release should perform the release")
	}
	if did2 {
		t.Error("QI-04: duplicate release performed a SECOND decrement — not idempotent")
	}
	if v, _, _ := fs.Floats.GetFloat(ctx, g.OrgKey("o1")); v != 1 {
		t.Errorf("QI-04: gauge = %v after duplicate release (want 1 — one resource still active)", v)
	}
	// Releasing the still-open activation 2 brings it to 0.
	_, _ = e.ReleaseActivation(ctx, r2.ActivationID)
	if v, _, _ := fs.Floats.GetFloat(ctx, g.OrgKey("o1")); v != 0 {
		t.Errorf("gauge = %v after releasing both (want 0)", v)
	}
}

// Claim 2 — the mixed-fleet acceptance test (D-22's release condition). A
// PYTHON-minted activation (its row + gauge spend, written directly into a
// shared Redis with Python's exact key shapes) is released by GO, and a
// DUPLICATE Go release applies exactly once. This is what lifts D-22's gate.
func TestMixedFleet_DuplicateGoReleaseOfPythonActivation_AppliesOnce_P52(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	fs, _ := counters.NewRedisFactory(c, "quota")
	store := activations.NewRedisStore(c, "", 0)
	e := activationEngine(t, fs, store, 10)
	g := counters.Gauge{Prefix: "quota", ResourceKey: "sandboxes"}

	// --- Simulate PYTHON acquiring: it spends the org gauge (+1) and writes
	// an activation row in the shared keyspace using Python's exact shapes.
	if _, err := fs.Floats.IncrByFloat(ctx, g.OrgKey("o1"), 1); err != nil {
		t.Fatal(err)
	}
	actID := "act_pythonminted00000000000000000000"
	row := map[string]any{
		"activation_id": actID, "org_id": "o1", "user_id": nil,
		"resource_key": "sandboxes", "cost": nil, "opened_at": "2026-03-15T10:00:00Z",
		"state": "open", "spend": map[string]any{"sandboxes": 1.0},
		"released_at": nil, "settled_at": nil,
	}
	blob, _ := json.Marshal(row)
	if err := c.Set(ctx, "activation:row:"+actID, blob, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := c.SAdd(ctx, "activation:open:org:o1", actID).Err(); err != nil {
		t.Fatal(err)
	}

	// --- GO releases it, twice (duplicate delivery).
	did1, err := e.ReleaseActivation(ctx, actID)
	if err != nil {
		t.Fatal(err)
	}
	did2, err := e.ReleaseActivation(ctx, actID)
	if err != nil {
		t.Fatal(err)
	}
	if !did1 {
		t.Error("Go's first release of the Python activation should perform it")
	}
	if did2 {
		t.Error("D-22: Go's DUPLICATE release double-applied — mixed-fleet undercount")
	}
	v, _, _ := fs.Floats.GetFloat(ctx, g.OrgKey("o1"))
	if v != 0 {
		t.Errorf("D-22: gauge = %v after duplicate Go release of a Python activation (want 0, applied exactly once)", v)
	}
}

// Settle is idempotent on the activation id (QB-02).
func TestSettle_IdempotentOnID_P52(t *testing.T) {
	fs := counters.NewMemoryFactory("quota")
	store := activations.NewInMemoryStore()
	e := activationEngine(t, fs, store, 10)
	ctx := context.Background()
	r, _ := e.Acquire(ctx, AcquireInput{OrgID: "o1", ResourceKey: "sandboxes"})
	first, _ := e.Settle(ctx, r.ActivationID, "0.30")
	second, _ := e.Settle(ctx, r.ActivationID, "0.30")
	if !first {
		t.Error("first settle should record")
	}
	if second {
		t.Error("QB-02: duplicate settle recorded twice — not idempotent")
	}
}
