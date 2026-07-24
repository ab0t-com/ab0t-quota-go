package engine

// K-8 (board; keyspace spec §3.2/§6) — the Go engine consumes counters.Keyspace:
// v2 shape routing, dual-write maintenance of BOTH shapes, seed-if-absent,
// dual-claimed idempotency latches, dual-read fallback. Runs on BOTH built-in
// stores (memory natively; redis via miniredis Lua) where meaningful.
//
//	KS1 v1 default is byte-identical (control — the regression that matters)
//	KS2 (2,false) writes v2-shape keys, never v1
//	KS3 (1,true) maintains BOTH shapes on Spend/Release
//	KS4 seed-if-absent: a pre-dual v1 level is seeded, never zeroed/added-onto
//	KS5 dual-read fallback: authoritative miss falls back to the other shape
//	KS6 idempotency latch claimed on BOTH shapes; replay across the flip is dup
//	KS7 negative control: a single-shape write in dual is CAUGHT by KS3

import (
	"context"
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

const (
	ksSvc = "test-svc"
	ksOrg = "org1"
	ksRK  = "sandbox.concurrent"
)

func ksConfig() *config.Config {
	return &config.Config{
		ServiceName:  ksSvc,
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", Mapping: map[string]string{"alice": "pro"}},
		Tiers: []config.Tier{
			{TierID: "pro", Limits: map[string]config.TierLimit{
				ksRK: {Limit: ptrFloat(10)},
			}},
		},
		Resources: []config.ResourceDef{
			{ResourceKey: ksRK, CounterType: config.CounterGauge},
		},
	}
}

// ksEngine builds an engine over miniredis with the given keyspace state.
// Returns the engine and the raw client for byte-level key assertions.
func ksEngine(t *testing.T, ks counters.Keyspace) (*Engine, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	f, err := counters.NewRedisFactory(c, "quota")
	if err != nil {
		t.Fatal(err)
	}
	f.Keyspace = ks
	cfg := ksConfig()
	prov, err := providers.New(cfg.TierProvider)
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{
		Cfg: cfg, Reg: registry.New(cfg), Provider: prov, Factory: f,
		Messages:    messages.New(messages.Templates{}),
		Clock:       func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Activations: activations.NewInMemoryStore(),
	}, c
}

func ksKeys(t *testing.T, c *redis.Client) map[string]string {
	t.Helper()
	out := map[string]string{}
	keys, err := c.Keys(context.Background(), "quota:*").Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		v, _ := c.Get(context.Background(), k).Result()
		out[k] = v
	}
	return out
}

const (
	v1Gauge = "quota:" + ksOrg + ":" + ksRK + ":gauge"
	v2Gauge = "quota:v2:{" + ksSvc + "/" + ksOrg + "}:" + ksRK + ":gauge"
	v1User  = "quota:" + ksOrg + ":" + ksRK + ":gauge:user:alice"
	v2User  = "quota:v2:{" + ksSvc + "/" + ksOrg + "}:" + ksRK + ":gauge:user:alice"
)

// KS1 — control: the zero-value Keyspace writes EXACTLY the legacy keys.
func TestKS1_DefaultKeyspaceIsByteIdentical(t *testing.T) {
	e, c := ksEngine(t, counters.Keyspace{})
	ctx := context.Background()
	if _, err := e.Spend(ctx, CheckInput{UserID: "alice", OrgID: ksOrg, ResourceKey: ksRK, Cost: 2}); err != nil {
		t.Fatal(err)
	}
	got := ksKeys(t, c)
	if len(got) != 2 || got[v1Gauge] != "2" || got[v1User] != "2" {
		t.Fatalf("v1 default keyspace changed: %v", got)
	}
}

// KS2 — (2,false): every write lands in the v2 shape, none in v1.
func TestKS2_V2WritesV2ShapeOnly(t *testing.T) {
	ks, err := counters.NewKeyspace(ksSvc, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	e, c := ksEngine(t, ks)
	ctx := context.Background()
	if _, err := e.Spend(ctx, CheckInput{UserID: "alice", OrgID: ksOrg, ResourceKey: ksRK, Cost: 2}); err != nil {
		t.Fatal(err)
	}
	got := ksKeys(t, c)
	if got[v2Gauge] != "2" || got[v2User] != "2" {
		t.Fatalf("v2 engine did not write v2 keys: %v", got)
	}
	if _, bad := got[v1Gauge]; bad {
		t.Fatalf("v2 (dual off) engine wrote a v1 key: %v", got)
	}
}

// KS3 — (1,true): Spend and Release maintain BOTH shapes, values equal.
func TestKS3_DualWriteMaintainsBothShapes(t *testing.T) {
	ks, err := counters.NewKeyspace(ksSvc, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	e, c := ksEngine(t, ks)
	ctx := context.Background()
	if _, err := e.Spend(ctx, CheckInput{UserID: "alice", OrgID: ksOrg, ResourceKey: ksRK, Cost: 3}); err != nil {
		t.Fatal(err)
	}
	got := ksKeys(t, c)
	if got[v1Gauge] != "3" || got[v2Gauge] != "3" {
		t.Fatalf("dual Spend must maintain BOTH shapes equally: %v", got)
	}
	if got[v1User] != "3" || got[v2User] != "3" {
		t.Fatalf("dual Spend must maintain BOTH user partitions: %v", got)
	}
	if err := e.Release(ctx, CheckInput{UserID: "alice", OrgID: ksOrg, ResourceKey: ksRK, Cost: 1}); err != nil {
		t.Fatal(err)
	}
	got = ksKeys(t, c)
	if got[v1Gauge] != "2" || got[v2Gauge] != "2" {
		t.Fatalf("dual Release must maintain BOTH shapes equally: %v", got)
	}
}

// KS4 — seed-if-absent: a v1 world entering dual seeds v2 from the LIVE v1
// level inside the same mutation — never zero, never seed-then-add doubling.
func TestKS4_SeedIfAbsentNeverZeroNeverDoubles(t *testing.T) {
	ks, err := counters.NewKeyspace(ksSvc, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	e, c := ksEngine(t, ks)
	ctx := context.Background()
	// The pre-dual v1 world: gauge already at 5.
	if err := c.Set(ctx, v1Gauge, "5", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Spend(ctx, CheckInput{OrgID: ksOrg, ResourceKey: ksRK, Cost: 1}); err != nil {
		t.Fatal(err)
	}
	got := ksKeys(t, c)
	if got[v1Gauge] != "6" {
		t.Fatalf("v1 level lost: %v", got)
	}
	if got[v2Gauge] != "6" {
		t.Fatalf("v2 twin must be SEEDED from v1 then mutated (5+1=6), got %q — "+
			"a fresh-zero (1) or seed-then-add (11) here is the money bug", got[v2Gauge])
	}
}

// KS5 — dual-read fallback: after the flip (2,true), a counter whose v2 twin
// is not yet seeded must still READ its v1 level (authoritative first, then
// the other shape) — no counter reads zero while its v1 twin holds a value.
func TestKS5_DualReadFallbackOrder(t *testing.T) {
	ks, err := counters.NewKeyspace(ksSvc, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	e, c := ksEngine(t, ks)
	ctx := context.Background()
	if err := c.Set(ctx, v1Gauge, "7", 0).Err(); err != nil {
		t.Fatal(err)
	}
	res, err := e.Check(ctx, CheckInput{UserID: "alice", OrgID: ksOrg, ResourceKey: ksRK})
	if err != nil {
		t.Fatal(err)
	}
	if res.Used != 7 {
		t.Fatalf("dual-read must fall back to the unmigrated shape: Used=%v want 7", res.Used)
	}
	// Authoritative-first: once v2 holds a diverged value, v2 wins.
	if err := c.Set(ctx, v2Gauge, "9", 0).Err(); err != nil {
		t.Fatal(err)
	}
	res, err = e.Check(ctx, CheckInput{UserID: "alice", OrgID: ksOrg, ResourceKey: ksRK})
	if err != nil {
		t.Fatal(err)
	}
	if res.Used != 9 {
		t.Fatalf("dual-read must prefer the AUTHORITATIVE shape: Used=%v want 9", res.Used)
	}
}

// KS6 — the idempotency latch is claimed on BOTH shapes during dual, so a
// retry of a pre-flip acquire is recognised AFTER the flip (no double-charge).
func TestKS6_IdemLatchDualClaimedAcrossFlip(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })

	build := func(ks counters.Keyspace) *Engine {
		f, err := counters.NewRedisFactory(c, "quota")
		if err != nil {
			t.Fatal(err)
		}
		f.Keyspace = ks
		cfg := ksConfig()
		prov, _ := providers.New(cfg.TierProvider)
		return &Engine{
			Cfg: cfg, Reg: registry.New(cfg), Provider: prov, Factory: f,
			Messages:    messages.New(messages.Templates{}),
			Clock:       func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
			Activations: activations.NewInMemoryStore(),
		}
	}
	ksDualV1, _ := counters.NewKeyspace(ksSvc, 1, true)
	ksDualV2, _ := counters.NewKeyspace(ksSvc, 2, true)

	e1 := build(ksDualV1)
	out, err := e1.Acquire(ctx, AcquireInput{OrgID: ksOrg, ResourceKey: ksRK, IdempotencyKey: "op-1"})
	if err != nil || !out.Admitted || out.Reason != "ok" {
		t.Fatalf("first acquire: %+v err=%v", out, err)
	}
	// The flip: v2 becomes authoritative; the SAME redis, new process.
	e2 := build(ksDualV2)
	out2, err := e2.Acquire(ctx, AcquireInput{OrgID: ksOrg, ResourceKey: ksRK, IdempotencyKey: "op-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !out2.Admitted || out2.Reason != "dup" {
		t.Fatalf("replay across the flip must be recognised as dup (latch on BOTH shapes), got %+v", out2)
	}
	v1v, _ := c.Get(ctx, v1Gauge).Result()
	v2v, _ := c.Get(ctx, v2Gauge).Result()
	if v1v != "1" || v2v != "1" {
		t.Fatalf("replay double-charged: v1=%q v2=%q want 1/1", v1v, v2v)
	}
}

// KS7 — negative control (D-14): a broken dual that mutates only the
// authoritative shape is CAUGHT by KS3's both-shapes assertion. Simulated by
// running the same op through a NON-dual keyspace and asserting the KS3
// assertion would fail on it — proving the assertion bites on maintenance,
// not on incidental state.
func TestKS7_NegativeControl_SingleShapeWriteIsCaught(t *testing.T) {
	ks, err := counters.NewKeyspace(ksSvc, 1, false) // dual dropped
	if err != nil {
		t.Fatal(err)
	}
	e, c := ksEngine(t, ks)
	ctx := context.Background()
	if _, err := e.Spend(ctx, CheckInput{OrgID: ksOrg, ResourceKey: ksRK, Cost: 3}); err != nil {
		t.Fatal(err)
	}
	got := ksKeys(t, c)
	if got[v1Gauge] == "3" && got[v2Gauge] == "3" {
		t.Fatalf("negative control broken: a non-dual write satisfied the dual assertion: %v", got)
	}
}
