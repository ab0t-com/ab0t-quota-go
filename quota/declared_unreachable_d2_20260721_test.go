package quota

// T-13 REDs (D-2 + GO-10, pack 20260721_shared_lib_declared_not_discovered).
// Contract (DECISIONS.md D-2, exact Python semantics — setup.py
// _gate_redis_reachable): a DECLARED but unreachable Redis at boot RETRIES
// the *unreachable* kind for up to storage.connect_retry_seconds (default
// 30, 0 = fail immediately), then REFUSES with a typed reachability error.
// It NEVER degrades to in-memory — the old degrade served per-process
// counters BEHIND A GREEN HEALTH PROBE (GO-10, established by T-G8's
// write-up: WhyOff feeds no predicate, empty-value predicates pass).
// Auth failures refuse IMMEDIATELY: retrying a wrong password is just a
// slower wrong password. Runtime (post-boot) failure stays loud-not-fatal
// (D-75) — untouched.
//
// Migration note written BEFORE this change: CHANGELOG.md [Unreleased] §2.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// closedPortAddr reserves a port and closes it — connection-refused, fast.
func closedPortAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestUnreachableDeclaredRedis_RefusesAndNeverDegrades(t *testing.T) {
	t.Setenv("QUOTA_REDIS_URL", "")
	addr := closedPortAddr(t)
	doc := `{"storage": {"redis_url": "redis://` + addr + `/0",
	          "connect_retry_seconds": 0},` + minimalCoreJSON + `}`
	q, err := Setup(context.Background(), Options{ConfigOverride: configFromJSON(t, doc)})
	if err == nil {
		fs := q.Capabilities().FloatStore
		defer q.Close(context.Background())
		t.Fatalf("D-2/GO-10: a DECLARED but unreachable Redis must REFUSE to start, got a "+
			"running Quota with FloatStore=%q — the old degrade served per-process counters "+
			"behind a GREEN health probe (every replica admits the full limit; restart zeroes "+
			"usage)", fs)
	}
	for _, tok := range []string{"REACH", "connect_retry_seconds", "NOT a topology verdict"} {
		if !containsStr(err.Error(), tok) {
			t.Errorf("the reachability refusal must contain %q; got: %v", tok, err)
		}
	}
	if containsStr(err.Error(), "redis_cluster_confirmed_disabled: true") {
		t.Errorf("the refusal must not advise the topology assertion flags: %v", err)
	}
}

func TestAuthFailure_RefusesImmediately_NoRetry(t *testing.T) {
	t.Setenv("QUOTA_REDIS_URL", "")
	mr := miniredis.RunT(t)
	mr.RequireAuth("the-real-password")
	// Wrong password + a LARGE budget: an auth failure must not consume it.
	doc := `{"storage": {"redis_url": "redis://:wrong-pass@` + mr.Addr() + `/0",
	          "connect_retry_seconds": 30},` + minimalCoreJSON + `}`
	start := time.Now()
	q, err := Setup(context.Background(), Options{ConfigOverride: configFromJSON(t, doc)})
	elapsed := time.Since(start)
	if err == nil {
		defer q.Close(context.Background())
		t.Fatalf("an AUTH failure against a declared Redis must refuse (got FloatStore=%q)",
			q.Capabilities().FloatStore)
	}
	if elapsed > 5*time.Second {
		t.Errorf("auth refusal took %v — the D-2 retry budget must apply to the UNREACHABLE "+
			"kind only (retrying a wrong password is a slower wrong password)", elapsed)
	}
	if !containsStr(err.Error(), "AUTHENTICATE") {
		t.Errorf("the auth-class refusal must say AUTHENTICATE (Python-parity verb): %v", err)
	}
	if containsStr(err.Error(), "wrong-pass") || containsStr(err.Error(), "the-real-password") {
		t.Errorf("a password value leaked into the refusal: %v", err)
	}
}

func TestTransientBlip_HealsWithinBudget(t *testing.T) {
	t.Setenv("QUOTA_REDIS_URL", "")
	addr := closedPortAddr(t)
	// The blip: nothing listens now; a real server appears on the SAME addr
	// ~1.2s in — the co-start ordering case the D-2 window exists for.
	mr := miniredis.NewMiniRedis()
	go func() {
		time.Sleep(1200 * time.Millisecond)
		if err := mr.StartAddr(addr); err != nil {
			fmt.Println("late-start miniredis:", err)
		}
	}()
	defer mr.Close()

	doc := `{"storage": {"redis_url": "redis://` + addr + `/0",
	          "redis_cluster_confirmed_disabled": true,
	          "connect_retry_seconds": 15},` + minimalCoreJSON + `}`
	q, err := Setup(context.Background(), Options{ConfigOverride: configFromJSON(t, doc)})
	if err != nil {
		t.Fatalf("a transient boot blip INSIDE the budget must heal, not refuse (D-2): %v", err)
	}
	defer q.Close(context.Background())
	if fs := q.Capabilities().FloatStore; fs != "redis" {
		t.Fatalf("FloatStore = %q, want redis — the retry never reconnected (or degraded)", fs)
	}
	// The reachability verdict is PUBLISHED (GO-10: a verdict nobody can read
	// is not a verdict). JSON-shape assertion so this file is a true RED
	// before the field exists.
	raw, _ := json.Marshal(q.Capabilities())
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if v, _ := m["RedisReachable"].(string); v != "on (PING verified)" {
		t.Errorf("Capabilities.RedisReachable = %q, want \"on (PING verified)\" "+
			"(the Python REACHABLE_OK literal — cross-runtime vocabulary)", v)
	}
}

// TestHealthy_FailsOnMissingOrFailedReachability_GO10 pins the GO-10 closure
// (green-time — the guard did not exist at RED): the reachability verdict
// FEEDS a health predicate. An empty value is absence, and absence is not
// health (D-49/D-51) — the exact hole T-G8 found behind the old degrade.
func TestHealthy_FailsOnMissingOrFailedReachability_GO10(t *testing.T) {
	base := Capabilities{
		BillingStatus: "OFF (paid disabled)", Reconciler: "OFF — not requested",
		FloatStore: "redis", RedisTopology: TopologySingleNode,
		CounterEvictionPolicy: "noeviction", RedisScripting: "on (EVAL verified)",
		PreflightReverification: "on (rides the reconciler loop)",
		CounterEvictionsObserved: "0 (no keys evicted on this server)",
		RedisPersistStatus: "ok", MemoryHeadroom: "ok",
	}
	for _, tc := range []struct {
		name    string
		value   string
		healthy bool
	}{
		{"affirmative PING verdict is healthy (control)", "on (PING verified)", true},
		{"EMPTY reachability is not health (GO-10)", "", false},
		{"probe-failed never reads green", TopologyProbeFailed + " [unreachable: dial tcp]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := base
			cap.RedisReachable = tc.value
			q := &Quota{capability: cap}
			healthy, reasons := q.Healthy()
			if healthy != tc.healthy {
				t.Errorf("Healthy() = %v, want %v — reasons: %v", healthy, tc.healthy, reasons)
			}
			if _, published := reasons["redis_reachable"]; !published {
				t.Error("redis_reachable must be published in the health reasons (a verdict nobody reads is not a verdict)")
			}
		})
	}
	// And the memory:// declaration stays exempt (no store to reach).
	capMem := base
	capMem.FloatStore = "memory"
	capMem.RedisReachable = TopologyNA
	if healthy, reasons := (&Quota{capability: capMem}).Healthy(); !healthy {
		t.Errorf("declared memory mode must stay healthy: %v", reasons)
	}
}
