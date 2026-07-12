package quota

// D-72 / D-73 / D-74 — the Redis preflight (Go lane). Twin of Python's
// tests/test_redis_preflight_d72_d73_d74_20260712.py.
//
// D-72 is the URGENT one and outranks D-71: `check_redis_outbox_durability` guarded the
// OUTBOX; the COUNTER — the thing the library exists to protect — ran on the same Redis with
// NO check. Under `allkeys-*` Redis evicts a LIVE gauge, the counter reads ZERO for a
// resource that is still running, and admission silently over-grants: under-count → phantom
// headroom → over-admission (D-31's forbidden direction). D-71 refuses loudly at boot; D-72
// fails SILENTLY at runtime, as free quota, behind a green health check.
//
// D-73: every counter op is EVAL — SCRIPT LOAD the REAL acquire source at boot.
// D-74: a version floor.
//
// The fakes below drive the REAL checks (redisguard.Prober) with canned server answers —
// never a re-implementation of the logic under test. And, after D-71 (where the emulator
// agreed with a wrong model and only a real server caught it), every gate here was ALSO run
// against real redis:7 containers: allkeys-lru, scripting-disabled (SCRIPT/EVAL renamed
// away), and a correct noeviction control. See the artifact.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/redisguard"
)

// fakeProbe drives the REAL redisguard checks.
type fakeProbe struct {
	policy, appendonly, save string
	configErr                string
	scriptErr                string
	info, infoErr            string
}

func (f fakeProbe) ConfigGet(ctx context.Context, p string) *redis.MapStringStringCmd {
	cmd := redis.NewMapStringStringCmd(ctx)
	if f.configErr != "" {
		cmd.SetErr(errors.New(f.configErr))
		return cmd
	}
	cmd.SetVal(map[string]string{
		"maxmemory-policy": f.policy, "appendonly": f.appendonly, "save": f.save,
	})
	return cmd
}

func (f fakeProbe) ScriptLoad(ctx context.Context, script string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx)
	if f.scriptErr != "" {
		cmd.SetErr(errors.New(f.scriptErr))
		return cmd
	}
	cmd.SetVal("e0e1f9fabfc9d4800c877a703b823ac0578ff8db")
	return cmd
}

func (f fakeProbe) Info(ctx context.Context, section ...string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx)
	if f.infoErr != "" {
		cmd.SetErr(errors.New(f.infoErr))
		return cmd
	}
	cmd.SetVal(f.info)
	return cmd
}

// ---------------------------------------------------------------------------
// D-72 — the counter may not live on an evicting Redis
// ---------------------------------------------------------------------------

func TestCounterEviction_D72(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		f         fakeProbe
		confirmed bool
		wantOK    bool
	}{
		{"allkeys-lru is REJECTED", fakeProbe{policy: "allkeys-lru"}, false, false},
		{"allkeys-lfu is REJECTED", fakeProbe{policy: "allkeys-lfu"}, false, false},
		{"allkeys-random is REJECTED", fakeProbe{policy: "allkeys-random"}, false, false},
		// [control] the guard is not a blanket reject — a correct Redis passes.
		{"noeviction is ACCEPTED", fakeProbe{policy: "noeviction"}, false, true},
		// volatile-* only evicts keys WITH a TTL; the counter keys carry none.
		{"volatile-lru is ACCEPTED", fakeProbe{policy: "volatile-lru"}, false, true},
		// [boundary] the counter's fatal property is EVICTION, not persistence: a
		// restart-lost counter heals (D-28); an evicted one silently under-counts.
		{"appendonly=no alone does NOT block the counter", fakeProbe{policy: "noeviction", appendonly: "no"}, false, true},
		// ElastiCache disables CONFIG: unverified ⇒ not safe (D-51) ⇒ refuse…
		{"CONFIG unavailable is REJECTED without the assertion", fakeProbe{configErr: "ERR unknown command 'config'"}, false, false},
		// …unless the operator puts the assertion on the record.
		{"CONFIG unavailable + operator assertion is ACCEPTED", fakeProbe{configErr: "ERR unknown command 'config'"}, true, true},
		// D-32's law: the assertion rescues an ABSENT signal, never a READ allkeys-*.
		{"an assertion does NOT override a READ allkeys policy", fakeProbe{policy: "allkeys-lru"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := redisguard.CheckCounterEviction(ctx, tc.f, tc.confirmed)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v (want %v) — %s", ok, tc.wantOK, reason)
			}
		})
	}
}

func TestCounterEvictionError_NamesCauseAndRemedy_D72(t *testing.T) {
	msg := strings.ToLower(redisguard.CounterEvictionError("maxmemory-policy=allkeys-lru").Error())
	for _, tok := range []string{"evict", "under-count", "noeviction", "counter"} {
		if !strings.Contains(msg, tok) {
			t.Errorf("the refusal must name %q: %s", tok, msg)
		}
	}
	if !errors.Is(redisguard.CounterEvictionError("x"), redisguard.ErrCounterEviction) {
		t.Error("the refusal must be errors.Is-able")
	}
}

// ---------------------------------------------------------------------------
// D-73 — scripting capability
// ---------------------------------------------------------------------------

func TestScriptCapability_D73(t *testing.T) {
	ctx := context.Background()
	// [control] the check loads the REAL acquire source, not `return 1`.
	if ok, reason := redisguard.CheckScriptCapability(ctx, fakeProbe{}); !ok {
		t.Fatalf("a scripting Redis must pass: %s", reason)
	}
	ok, reason := redisguard.CheckScriptCapability(ctx, fakeProbe{scriptErr: "ERR unknown command 'SCRIPT'"})
	if ok {
		t.Fatal("a Redis that cannot SCRIPT LOAD must be refused at BOOT, not at the first acquire")
	}
	if !strings.Contains(strings.ToLower(reason), "script") {
		t.Errorf("reason must name the cause: %s", reason)
	}
}

// ---------------------------------------------------------------------------
// D-74 — version floor
// ---------------------------------------------------------------------------

func TestVersionFloor_D74(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    string
	}{
		{"7.2.4", "ok"}, {"6.0.0", "ok"}, {"6.2.14", "ok"},
		{"5.0.14", "below_floor"}, {"4.0.9", "below_floor"},
		{"", "unknown"}, // absence is not a value (D-51)
	} {
		if got, detail := redisguard.EvaluateVersion(tc.version, redisguard.VersionFloor); got != tc.want {
			t.Errorf("version %q → %q (want %q) — %s", tc.version, got, tc.want, detail)
		}
	}
	// The floor is load-bearing, not a comparison that always passes.
	if got, _ := redisguard.EvaluateVersion("7.4.9", [3]int{99, 0, 0}); got != "below_floor" {
		t.Errorf("an impossible floor must refuse a 7.x server, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Setup REFUSES TO START (the point)
// ---------------------------------------------------------------------------

// miniredis implements no CONFIG at all, so it presents the "CONFIG unavailable" case —
// exactly what a managed Redis presents, and what the guard refuses without an assertion.
func TestSetup_RefusesUnverifiableCounterStore_D72(t *testing.T) {
	t.Setenv(redisguard.DurabilityConfirmEnv, "")
	mr := miniredis.RunT(t)
	c := minimalConfig()
	c.Storage = config.StorageConfig{RedisURL: "redis://" + mr.Addr(), RedisClusterConfirmedDisabled: true}

	_, err := Setup(context.Background(), Options{ConfigOverride: c})
	if err == nil {
		t.Fatal("an UNVERIFIABLE counter eviction policy must refuse to start (D-72)")
	}
	if !errors.Is(err, redisguard.ErrCounterEviction) {
		t.Fatalf("wrong error type: %v", err)
	}
	if !strings.Contains(err.Error(), "redis_durability_confirmed") {
		t.Fatalf("the refusal must name the remedy: %v", err)
	}
}

func TestSetup_OperatorAssertion_AllowsStart_AndIsOnTheRecord_D72(t *testing.T) {
	t.Setenv(redisguard.DurabilityConfirmEnv, "")
	mr := miniredis.RunT(t)
	c := minimalConfig()
	c.Storage = config.StorageConfig{
		RedisURL:                      "redis://" + mr.Addr(),
		RedisClusterConfirmedDisabled: true,
		RedisDurabilityConfirmed:      true,
	}

	q, err := Setup(context.Background(), Options{ConfigOverride: c})
	if err != nil {
		t.Fatalf("an explicit operator assertion must allow startup: %v", err)
	}
	defer q.Close(context.Background())

	got := q.Capabilities().CounterEvictionPolicy
	if !strings.Contains(strings.ToLower(got), "assert") {
		t.Fatalf("the operator assertion must be VISIBLE in Capabilities, got %q", got)
	}
	// [control] D-73 runs against the emulator for real: miniredis DOES support SCRIPT LOAD,
	// so the scripting capability is genuinely verified here, not assumed.
	if !strings.HasPrefix(q.Capabilities().RedisScripting, "on") {
		t.Fatalf("scripting should be verified on miniredis, got %q", q.Capabilities().RedisScripting)
	}
}

// ---------------------------------------------------------------------------
// the gates have a SINK — Healthy() (D-40 / D-49 / D-51)
// ---------------------------------------------------------------------------

func TestHealthy_FailsOnUnsafeCounterStore_D72_D73(t *testing.T) {
	base := Capabilities{
		BillingStatus: "OFF (paid disabled)", Reconciler: "OFF — not requested",
		RedisTopology: TopologySingleNode, FloatStore: "redis",
		CounterEvictionPolicy: "noeviction", RedisScripting: "on (EVAL verified)",
		// D-80/D-79 — the probe now also requires the FACT (no observed evictions) and the
		// DERIVED guarantee (the invariants are actually being re-verified).
		CounterEvictionsObserved: "0 (no keys evicted on this server)",
		PreflightReverification:  "on (rides the reconciler loop)",
	}
	for _, tc := range []struct {
		name              string
		policy, scripting string
		healthy           bool
	}{
		{"control: noeviction + scripting on", "noeviction", "on (EVAL verified)", true},
		{"evicting policy fails the probe", "allkeys-lru", "on (EVAL verified)", false},
		{"EVICTING/UNVERIFIED fails the probe", "EVICTING/UNVERIFIED (…)", "on (EVAL verified)", false},
		{"ABSENT policy is not health (D-49)", "", "on (EVAL verified)", false},
		{"unknown policy is not health (D-51)", "unknown", "on (EVAL verified)", false},
		{"scripting off fails the probe", "noeviction", "OFF (SCRIPT LOAD failed)", false},
		{"ABSENT scripting is not health", "noeviction", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := base
			cap.CounterEvictionPolicy = tc.policy
			cap.RedisScripting = tc.scripting
			q := &Quota{capability: cap}
			ok, reasons := q.Healthy()
			if ok != tc.healthy {
				t.Fatalf("Healthy()=%v (want %v), reasons=%v", ok, tc.healthy, reasons)
			}
			if reasons["counter_eviction_policy"] != tc.policy {
				t.Errorf("Healthy() must name the counter reason, got %q", reasons["counter_eviction_policy"])
			}
		})
	}
}

// ---------------------------------------------------------------------------
// REAL Redis (operator-gated; recipes in the artifact)
// ---------------------------------------------------------------------------

func TestRealEvictingRedis_IsRefused_D72(t *testing.T) {
	addr := envOr("AB0T_QUOTA_TEST_EVICT_ADDR")
	if addr == "" {
		t.Skip("AB0T_QUOTA_TEST_EVICT_ADDR not set — real-Redis leg is operator-gated")
	}
	t.Setenv(redisguard.DurabilityConfirmEnv, "")
	c := minimalConfig()
	c.Storage = config.StorageConfig{RedisURL: "redis://" + addr, RedisClusterConfirmedDisabled: true}

	_, err := Setup(context.Background(), Options{ConfigOverride: c})
	if err == nil {
		t.Fatal("a REAL allkeys-lru Redis must refuse to start (D-72)")
	}
	if !errors.Is(err, redisguard.ErrCounterEviction) || !strings.Contains(err.Error(), "allkeys-lru") {
		t.Fatalf("the refusal must name the cause, got: %v", err)
	}
}

func TestRealScriptingDisabledRedis_IsRefused_D73(t *testing.T) {
	addr := envOr("AB0T_QUOTA_TEST_NOSCRIPT_ADDR")
	if addr == "" {
		t.Skip("AB0T_QUOTA_TEST_NOSCRIPT_ADDR not set — real-Redis leg is operator-gated")
	}
	c := minimalConfig()
	c.Storage = config.StorageConfig{
		RedisURL:                      "redis://" + addr,
		RedisClusterConfirmedDisabled: true,
		RedisDurabilityConfirmed:      true, // isolate D-73 from D-72
	}
	_, err := Setup(context.Background(), Options{ConfigOverride: c})
	if err == nil {
		t.Fatal("a REAL scripting-disabled Redis must refuse to start (D-73)")
	}
	if !errors.Is(err, redisguard.ErrScriptingUnsupported) {
		t.Fatalf("wrong error type: %v", err)
	}
}

func TestRealSafeRedis_IsAccepted_D72_D73_D74(t *testing.T) {
	addr := envOr("AB0T_QUOTA_TEST_REAL_ADDR")
	if addr == "" {
		t.Skip("AB0T_QUOTA_TEST_REAL_ADDR not set — real-Redis leg is operator-gated")
	}
	// [control] a real, correctly-configured redis:7 must PASS every gate — a guard that
	// refuses everything has told you nothing.
	t.Setenv(redisguard.DurabilityConfirmEnv, "")
	c := minimalConfig()
	c.Storage = config.StorageConfig{RedisURL: "redis://" + addr, RedisClusterConfirmedDisabled: true}

	q, err := Setup(context.Background(), Options{ConfigOverride: c})
	if err != nil {
		t.Fatalf("a REAL noeviction redis must be accepted, got: %v", err)
	}
	defer q.Close(context.Background())
	caps := q.Capabilities()
	if caps.CounterEvictionPolicy != "noeviction" {
		t.Errorf("counter policy = %q", caps.CounterEvictionPolicy)
	}
	if !strings.HasPrefix(caps.RedisScripting, "on") {
		t.Errorf("scripting = %q", caps.RedisScripting)
	}
	if strings.HasPrefix(caps.RedisVersion, "unknown") || strings.HasPrefix(caps.RedisVersion, "below") {
		t.Errorf("version = %q (a real redis:7 must report a version above the floor)", caps.RedisVersion)
	}
	if ok, reasons := q.Healthy(); !ok {
		t.Errorf("a correct Redis must be healthy: %v", reasons)
	}
}

// ---------------------------------------------------------------------------
// cross-runtime contract (D-43) — the structural conformance item
// ---------------------------------------------------------------------------

// These are SETUP-level contracts (no engine exists yet when they run), so they are
// registered as a structural conformance item rather than forced into a scenario row that
// would not execute what it claims. Both runtimes assert their OWN refusals against the
// SAME declared tokens — one data file, two runners (D-43). Python's twin:
// TestStructuralConformance::test_python_satisfies_the_declared_structural_item.
func TestGoSatisfiesDeclaredStructuralItem_ST_PREFLIGHT_1(t *testing.T) {
	raw, err := os.ReadFile("../conformance/scenarios.json")
	if err != nil {
		t.Fatalf("read scenarios.json: %v", err)
	}
	var doc struct {
		Structural []struct {
			ID                        string   `json:"id"`
			Runtimes                  []string `json:"runtimes"`
			ConfigKey                 string   `json:"config_key"`
			EnvKey                    string   `json:"env_key"`
			EvictingPolicies          []string `json:"evicting_policies"`
			VersionFloor              string   `json:"version_floor"`
			EvictionErrorMustContain  []string `json:"eviction_error_must_contain"`
			ScriptingErrorMustContain []string `json:"scripting_error_must_contain"`
		} `json:"structural_conformance"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse scenarios.json: %v", err)
	}
	idx := -1
	for i := range doc.Structural {
		if doc.Structural[i].ID == "ST-PREFLIGHT-1" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("ST-PREFLIGHT-1 must be declared in scenarios.json")
	}
	item := doc.Structural[idx]

	evictMsg := strings.ToLower(redisguard.CounterEvictionError("maxmemory-policy=allkeys-lru").Error())
	for _, tok := range item.EvictionErrorMustContain {
		if !strings.Contains(evictMsg, strings.ToLower(tok)) {
			t.Errorf("declared token %q missing from the Go eviction refusal", tok)
		}
	}
	scriptMsg := strings.ToLower(redisguard.ScriptingError("SCRIPT LOAD failed").Error())
	for _, tok := range item.ScriptingErrorMustContain {
		if !strings.Contains(scriptMsg, strings.ToLower(tok)) {
			t.Errorf("declared token %q missing from the Go scripting refusal", tok)
		}
	}
	for _, p := range item.EvictingPolicies {
		if !redisguard.EvictingPolicies[p] {
			t.Errorf("declared evicting policy %q is not refused by Go", p)
		}
	}
	floor := fmt.Sprintf("%d.%d.%d", redisguard.VersionFloor[0], redisguard.VersionFloor[1], redisguard.VersionFloor[2])
	if item.VersionFloor != floor {
		t.Errorf("declared version floor %q != Go %q", item.VersionFloor, floor)
	}
	if item.ConfigKey != "storage.redis_durability_confirmed" || item.EnvKey != redisguard.DurabilityConfirmEnv {
		t.Errorf("declared keys drifted from the Go implementation: %+v", item)
	}
}
