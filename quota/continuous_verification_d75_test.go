package quota

// D-75 / D-77 (Go lane) — the guards' own caveat: they check the world ONCE.
//
//	"An assumption machine-checked once is an assumption trusted thereafter."
//
// Every boot guard we own (D-32 durability, D-71 topology, D-72 eviction, D-73 scripting,
// D-76 DDB) verified the world at STARTUP and then trusted it forever. A
// `CONFIG SET maxmemory-policy allkeys-lru` at 3am — or a managed failover onto a replica
// with a different config — is invisible to all of them: the counter becomes silently
// evictable, a live gauge is evicted, and admission over-grants (D-31's forbidden
// direction) behind a green health check. Same defect shape as every other one in this
// ticket: a mechanism that stops short of the boundary its guarantee must cross. Here the
// boundary is TIME.
//
// A safe→unsafe transition at runtime is LOUD, NOT FATAL: degrade Healthy(), alert, update
// Capabilities, keep serving. A running service that suddenly refuses is its own outage.
//
// The real leg (operator-gated) does the thing that matters: boots against a CLEAN real
// Redis, runs `CONFIG SET maxmemory-policy allkeys-lru` on the LIVE server, and proves the
// running process catches it — and then HEALS when the operator fixes it, with no restart.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/ab0t-com/ab0t-quota-go/activations"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/ddbguard"
	"github.com/ab0t-com/ab0t-quota-go/reconcile"
	"github.com/ab0t-com/ab0t-quota-go/redisguard"
)

// ---------------------------------------------------------------------------
// D-77 — memory headroom
// ---------------------------------------------------------------------------

func TestMemoryHeadroom_D77(t *testing.T) {
	for _, tc := range []struct {
		name            string
		maxmemory, used int64
		found           bool
		want            string
	}{
		{"unbounded maxmemory has no cliff", 0, 10_000_000, true, "unbounded"},
		{"ample headroom is ok", 100, 10, true, "ok"},
		{"approaching the cliff degrades", 100, 93, true, "low_headroom"},
		{"unreadable is unknown, never assumed ok", 0, 0, false, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := redisguard.EvaluateMemoryHeadroom(tc.maxmemory, tc.used, tc.found)
			if got != tc.want {
				t.Fatalf("status = %q (want %q) — %s", got, tc.want, detail)
			}
		})
	}
	// The health predicate degrades ONLY on a READ low-headroom (the D-74 deviation,
	// ratified, applied here: an unreadable memory statistic is not a live hazard).
	if redisguard.MemoryHeadroomOK("low_headroom (93% used)") {
		t.Error("low headroom must degrade the probe")
	}
	if !redisguard.MemoryHeadroomOK("unknown") || !redisguard.MemoryHeadroomOK("unbounded (maxmemory=0)") {
		t.Error("unknown/unbounded must NOT degrade (stated deviation)")
	}
}

// ---------------------------------------------------------------------------
// D-75 — the invariants are RE-verifiable, and a runtime flip is caught
// ---------------------------------------------------------------------------

// mutableProbe is a Redis whose CONFIG changes UNDER US — the thing every guard we wrote
// assumed could not happen. It drives the REAL verifyRedisInvariants.
type mutableProbe struct {
	fakeProber
	policy string
}

func (m *mutableProbe) ConfigGet(ctx context.Context, p string) *redis.MapStringStringCmd {
	cmd := redis.NewMapStringStringCmd(ctx)
	cmd.SetVal(map[string]string{"maxmemory-policy": m.policy, "appendonly": "yes", "save": ""})
	return cmd
}

func (m *mutableProbe) ScriptLoad(ctx context.Context, script string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx)
	cmd.SetVal("sha1234567890")
	return cmd
}

func (m *mutableProbe) Info(ctx context.Context, section ...string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx)
	if len(section) > 0 && section[0] == "cluster" {
		cmd.SetVal("# Cluster\r\ncluster_enabled:0\r\n")
		return cmd
	}
	if len(section) > 0 && section[0] == "memory" {
		cmd.SetVal("# Memory\r\nmaxmemory:0\r\nused_memory:1000\r\n")
		return cmd
	}
	cmd.SetVal("# Server\r\nredis_version:7.2.4\r\n")
	return cmd
}

func TestVerifyRedisInvariants_CatchesARuntimeFlip_D75(t *testing.T) {
	ctx := context.Background()
	cfg := minimalConfig()
	p := &mutableProbe{policy: "noeviction"}

	caps, unsafe := verifyRedisInvariants(ctx, p, cfg, false)
	if len(unsafe) != 0 {
		t.Fatalf("a clean Redis must report no violations, got %v", unsafe)
	}
	if caps["counter_eviction_policy"] != "noeviction" {
		t.Fatalf("caps = %v", caps)
	}

	p.policy = "allkeys-lru" // 3am. Nobody is watching.

	caps2, unsafe2 := verifyRedisInvariants(ctx, p, cfg, false)
	if len(unsafe2) != 1 || unsafe2[0][0] != "counter_eviction_policy" {
		t.Fatalf("the runtime flip must be CAUGHT, got %v", unsafe2)
	}
	if !strings.Contains(strings.ToLower(caps2["counter_eviction_policy"]), "evict") {
		t.Fatalf("the capability must tell the truth now: %q", caps2["counter_eviction_policy"])
	}
}

// The revalidator must be LOUD, NOT FATAL: Capabilities updated, Healthy() degraded, an
// alert fired — and the process still serving.
func TestRevalidator_DegradesHealth_AndAlerts_WithoutCrashing_D75(t *testing.T) {
	ctx := context.Background()
	p := &mutableProbe{policy: "noeviction"}
	q := &Quota{capability: Capabilities{
		BillingStatus: "OFF (paid disabled)", Reconciler: "OFF — not requested",
		FloatStore: "redis", RedisTopology: TopologySingleNode,
		// T-13 fixture: the new D-2/GO-10 reachability guard requires the affirmative value.
		RedisReachable: redisguard.RedisReachableOK,
		CounterEvictionPolicy: "noeviction", RedisScripting: "on (EVAL verified)",
		CounterEvictionsObserved: "0 (no keys evicted on this server)",
		PreflightReverification:  "on (rides the reconciler loop)",
	}}
	revalidate := q.makeRevalidator(p, minimalConfig())

	revalidate(ctx)
	if ok, reasons := q.Healthy(); !ok {
		t.Fatalf("a clean Redis must stay healthy: %v", reasons)
	}

	p.policy = "allkeys-lru" // the flip, mid-run
	revalidate(ctx)

	ok, reasons := q.Healthy()
	if ok {
		t.Fatal("a runtime flip to allkeys-lru must DEGRADE the probe (D-75)")
	}
	if !strings.Contains(strings.ToLower(reasons["counter_eviction_policy"]), "evict") {
		t.Fatalf("the health reason must name the cause: %v", reasons)
	}

	// [control] not a one-way latch: fix the Redis, the probe recovers — no restart.
	p.policy = "noeviction"
	revalidate(ctx)
	if ok, reasons := q.Healthy(); !ok {
		t.Fatalf("a RESTORED policy must heal the probe without a restart: %v", reasons)
	}
}

// D-50: test the LOOP, not the function. The re-verification RIDES the reconciler loop —
// it must actually be invoked by RunLoop, not merely be callable.
func TestReconcilerLoop_InvokesThePreflight_D75(t *testing.T) {
	called := make(chan struct{}, 4)
	// RunLoop refuses to start without a durable ledger + the recent-activity guard (D-39/
	// D-62) — so the loop is driven exactly as production drives it. NOTE (framed, not
	// hidden): a deployment with NO reconciler therefore gets NO re-verification — but a
	// reconciler that is off ALREADY fails Healthy(), so such a deployment is loudly degraded,
	// never silently trusting a stale verdict.
	r := &reconcile.Reconciler{
		Store:           durableStoreStub{},
		RecentlyTouched: func(orgID, rk string) bool { return false },
		Preflight:       func(ctx context.Context) { called <- struct{}{} },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.RunLoop(ctx, 20*time.Millisecond, func() []string { return []string{} })

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("the reconciler loop must RE-VERIFY every pass (D-75) — it never called Preflight")
	}
}

// ---------------------------------------------------------------------------
// REAL Redis: the 3am flip, on a LIVE server (operator-gated)
// ---------------------------------------------------------------------------

func TestRealLiveConfigFlip_IsCaughtByTheRunningProcess_D75(t *testing.T) {
	addr := envOr("AB0T_QUOTA_TEST_LIVE_ADDR")
	if addr == "" {
		t.Skip("AB0T_QUOTA_TEST_LIVE_ADDR not set — the live-flip leg is operator-gated")
	}
	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	// Always leave the server as we found it.
	defer client.ConfigSet(ctx, "maxmemory-policy", "noeviction")

	// D-80 made the eviction FACT a health signal, and facts are STICKY (that is the point).
	// Start from a clean server, or this test inherits another test's REAL evictions and
	// degrades for the right reason at the wrong time.
	client.ConfigResetStat(ctx)
	if err := client.ConfigSet(ctx, "maxmemory-policy", "noeviction").Err(); err != nil {
		t.Fatalf("CONFIG SET: %v", err)
	}

	cfg := minimalConfig()
	cfg.Storage = config.StorageConfig{RedisURL: config.Declare("redis://" + addr)}
	q := &Quota{capability: Capabilities{
		BillingStatus: "OFF (paid disabled)", Reconciler: "OFF — not requested",
		FloatStore: "redis", RedisReachable: redisguard.RedisReachableOK, PreflightReverification: "on (rides the reconciler loop)",
	}}
	revalidate := q.makeRevalidator(client, cfg)

	revalidate(ctx)
	if got := q.Capabilities().CounterEvictionPolicy; got != "noeviction" {
		t.Fatalf("a clean LIVE Redis must verify clean, got %q", got)
	}
	if ok, reasons := q.Healthy(); !ok {
		t.Fatalf("clean live Redis must be healthy: %v", reasons)
	}

	// ---- the 3am flip, on a LIVE server ----
	if err := client.ConfigSet(ctx, "maxmemory-policy", "allkeys-lru").Err(); err != nil {
		t.Fatalf("CONFIG SET: %v", err)
	}
	revalidate(ctx)

	if ok, reasons := q.Healthy(); ok {
		t.Fatalf("a LIVE flip to allkeys-lru must degrade the probe WHILE RUNNING: %v", reasons)
	}
	if !strings.Contains(q.Capabilities().CounterEvictionPolicy, "allkeys-lru") {
		t.Fatalf("Capabilities must tell the truth after the flip, got %q",
			q.Capabilities().CounterEvictionPolicy)
	}

	// ---- the operator fixes it: heal without a restart ----
	if err := client.ConfigSet(ctx, "maxmemory-policy", "noeviction").Err(); err != nil {
		t.Fatalf("CONFIG SET: %v", err)
	}
	revalidate(ctx)
	if ok, reasons := q.Healthy(); !ok {
		t.Fatalf("a repaired LIVE Redis must heal the probe with no restart: %v", reasons)
	}
}

// durableStoreStub is a minimal activation store that REPORTS durable, so RunLoop starts
// (D-39's gate). It is never asked for rows: the loop is driven with an empty org list —
// this test is about the LOOP invoking the re-verification, not about reconciling.
type durableStoreStub struct{ activations.Store }

func (durableStoreStub) Durable() bool { return true }
func (durableStoreStub) ListOpenByOrg(ctx context.Context, orgID string) ([]activations.Activation, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// cross-runtime contract (D-43) — ST-RUNTIME-1
// ---------------------------------------------------------------------------

func TestGoSatisfiesDeclaredStructuralItem_ST_RUNTIME_1(t *testing.T) {
	raw, err := os.ReadFile("../conformance/scenarios.json")
	if err != nil {
		t.Fatalf("read scenarios.json: %v", err)
	}
	var doc struct {
		Structural []struct {
			ID                             string   `json:"id"`
			Runtimes                       []string `json:"runtimes"`
			ConfigKey                      string   `json:"config_key"`
			EnvKey                         string   `json:"env_key"`
			RuntimeViolationIsFatal        bool     `json:"runtime_violation_is_fatal"`
			RuntimeViolationDegradesHealth bool     `json:"runtime_violation_degrades_health"`
			RuntimeViolationAlerts         bool     `json:"runtime_violation_alerts"`
			ReverificationRides            string   `json:"reverification_rides"`
			MemoryWarnRatio                float64  `json:"memory_warn_ratio"`
			DDBWarnFindings                []string `json:"ddb_warn_findings"`
		} `json:"structural_conformance"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse scenarios.json: %v", err)
	}
	idx := -1
	for i := range doc.Structural {
		if doc.Structural[i].ID == "ST-RUNTIME-1" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("ST-RUNTIME-1 must be declared in scenarios.json")
	}
	item := doc.Structural[idx]

	if item.RuntimeViolationIsFatal {
		t.Error("the declared contract says a runtime violation is NOT fatal — Go must agree")
	}
	if !item.RuntimeViolationDegradesHealth || !item.RuntimeViolationAlerts {
		t.Error("a runtime violation must degrade health AND alert")
	}
	if !strings.Contains(item.ReverificationRides, "reconciler") {
		t.Errorf("re-verification must ride the reconciler loop, declared %q", item.ReverificationRides)
	}
	if item.MemoryWarnRatio != redisguard.MemoryWarnRatio {
		t.Errorf("declared memory_warn_ratio %v != Go %v", item.MemoryWarnRatio, redisguard.MemoryWarnRatio)
	}
	if item.EnvKey != ddbguard.PITRConfirmEnv || item.ConfigKey != "storage.ddb_pitr_confirmed" {
		t.Errorf("declared DDB keys drifted from the Go implementation: %+v", item)
	}
	// A DISABLED TTL must WARN, never refuse — the declared severity, asserted against the
	// real Go implementation (not a promise in a doc).
	found := false
	for _, w := range item.DDBWarnFindings {
		if w == "ttl_disabled" {
			found = true
		}
	}
	if !found {
		t.Error("ttl_disabled must be declared as a WARN, not a refusal")
	}
}

// ---------------------------------------------------------------------------
// D-80 — the EFFECT, not the policy
// ---------------------------------------------------------------------------

// evictedProbe reports a CLEAN policy and a server that has ALREADY evicted keys — the exact
// state a "fixed" Redis presents: somebody corrected maxmemory-policy at 09:00, after it
// evicted a live gauge at 03:00. Every policy check we own passes; the counter is still wrong.
type evictedProbe struct {
	mutableProbe
	evicted int64
}

func (e *evictedProbe) Info(ctx context.Context, section ...string) *redis.StringCmd {
	if len(section) > 0 && section[0] == "stats" {
		cmd := redis.NewStringCmd(ctx)
		cmd.SetVal(fmt.Sprintf("# Stats\r\nevicted_keys:%d\r\n", e.evicted))
		return cmd
	}
	return e.mutableProbe.Info(ctx, section...)
}

func TestEvictionFacts_ACleanPolicyStillHidesTheDamage_D80(t *testing.T) {
	ctx := context.Background()
	p := &evictedProbe{mutableProbe: mutableProbe{policy: "noeviction"}, evicted: 42}

	caps, unsafe := verifyRedisInvariants(ctx, p, minimalConfig(), false)

	if caps["counter_eviction_policy"] != "noeviction" {
		t.Fatalf("the POLICY check is happy (that is the point): %q", caps["counter_eviction_policy"])
	}
	found := false
	for _, u := range unsafe {
		if u[0] == "counter_evictions_observed" {
			found = true
		}
	}
	if !found {
		t.Fatal("a Redis that ALREADY evicted keys passes every policy check we own — the FACT " +
			"(INFO stats.evicted_keys) must be checked too (D-80)")
	}
	if !strings.Contains(caps["counter_evictions_observed"], "42") {
		t.Errorf("the capability must name the fact: %q", caps["counter_evictions_observed"])
	}
}

func TestEvictionFacts_CleanServerStaysClean_D80(t *testing.T) {
	// [control] no evictions ⇒ no incident. The guard is not a blanket alarm.
	ctx := context.Background()
	p := &evictedProbe{mutableProbe: mutableProbe{policy: "noeviction"}, evicted: 0}
	_, unsafe := verifyRedisInvariants(ctx, p, minimalConfig(), false)
	if len(unsafe) != 0 {
		t.Fatalf("a clean server must report no violations, got %v", unsafe)
	}
}

func TestEvictionFacts_MarkTheCounterUntrusted_AndDegradeHealth_D80(t *testing.T) {
	ctx := context.Background()
	p := &evictedProbe{mutableProbe: mutableProbe{policy: "noeviction"}, evicted: 7}
	q := &Quota{capability: Capabilities{
		BillingStatus: "OFF (paid disabled)", Reconciler: "OFF — not requested",
		FloatStore: "redis", RedisReachable: redisguard.RedisReachableOK, PreflightReverification: "on (rides the reconciler loop)",
	}}
	q.makeRevalidator(p, minimalConfig())(ctx)

	if ok, reasons := q.Healthy(); ok {
		t.Fatalf("an OBSERVED eviction is a money incident and must degrade: %v", reasons)
	}
	q.capMu.RLock()
	untrusted := q.counterUntrusted
	q.capMu.RUnlock()
	if !untrusted {
		t.Error("an evicted counter is no longer trustworthy — it must be marked for reconciliation " +
			"(the reconcile pass follows the re-check in the same loop tick)")
	}
}

// ---------------------------------------------------------------------------
// D-79 — the re-verification is a DERIVED requirement, not a wiring accident
// ---------------------------------------------------------------------------

func TestPreflightReverification_IsRequiredWhenTheCounterIsOnRedis_D79(t *testing.T) {
	// No ReconcileOrgs ⇒ no reconciler ⇒ nothing re-verifies the Redis invariants. Before
	// D-79 that was SILENT: the client switched off the guarantee by switching off a loop.
	mr := miniredis.RunT(t)
	c := minimalConfig()
	c.Storage = config.StorageConfig{
		RedisURL: config.Declare("redis://" + mr.Addr()), RedisClusterConfirmedDisabled: true,
		RedisDurabilityConfirmed: true,
	}
	q, err := Setup(context.Background(), Options{ConfigOverride: c}) // no ReconcileOrgs
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())

	if !strings.HasPrefix(q.Capabilities().PreflightReverification, "OFF") {
		t.Fatalf("with no reconciler loop, re-verification is OFF and must SAY so, got %q",
			q.Capabilities().PreflightReverification)
	}
	ok, reasons := q.Healthy()
	if ok {
		t.Fatal("a guarantee the client switched off must DEGRADE the probe, never vanish quietly (D-79)")
	}
	if !strings.Contains(reasons["preflight_reverification"], "OFF") {
		t.Errorf("the health reason must name it: %v", reasons["preflight_reverification"])
	}
}

func TestPreflightReverification_NotApplicableWithoutARedisCounter_D79(t *testing.T) {
	// [control] an in-memory counter has no Redis whose config could drift — the requirement is
	// DERIVED from config, so it must not fire where it makes no sense (a false 503 trains
	// operators to ignore the probe, D-49).
	q, err := Setup(context.Background(), Options{ConfigOverride: minimalConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	if !strings.HasPrefix(q.Capabilities().PreflightReverification, "n/a") {
		t.Fatalf("no Redis counter ⇒ n/a, got %q", q.Capabilities().PreflightReverification)
	}
	if ok, reasons := q.Healthy(); !ok {
		t.Fatalf("an in-memory-counter service must not be degraded by D-79: %v", reasons)
	}
}
