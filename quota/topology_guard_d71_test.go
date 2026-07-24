package quota

// D-71 — the Redis TOPOLOGY guard (Go lane). Twin of Python's
// tests/test_cluster_topology_guard_d71_20260711.py; both runtimes are asserted
// against the SAME declared contract (structural conformance item ST-TOPOLOGY-1 in
// conformance/scenarios.json — D-43).
//
// The atomic counter's Lua scripts are multi-key and fail with CROSSSLOT on a
// clustered Redis (D-23, observed at a real cluster). Our prod is single-node, so
// only a CLIENT would ever hit it — which is exactly why a LIBRARY must CHECK,
// never assume. These tests pin:
//
//  1. cluster_enabled:1        → Setup REFUSES TO START, loudly, naming cause+remedy.
//  2. topology unverifiable    → UNKNOWN → refuse, unless the operator asserts
//     storage.redis_cluster_confirmed_disabled (on the record).
//  3. cluster_enabled:0        → single-node → start (the control: not a blanket reject).
//  4. The verdict is in Capabilities and FAILS Healthy() (D-40/D-49/D-51).
//
// TestMain: miniredis answers "ERR 'CLUSTER INFO' not supported" and trims INFO, so
// the topology is unprobeable there — precisely the case the guard refuses. The
// emulator is, by construction, not a cluster, so the suite makes that assertion ONCE,
// here, in the open (the same on-the-record assertion a client on a managed Redis
// writes into quota-config.json). It is NOT a way to make the guard go away: the tests
// below clear the env explicitly and assert the refusal, and a POSITIVE cluster_enabled:1
// is refused regardless of any assertion.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/redisguard"
)

func TestMain(m *testing.M) {
	// D-71 + D-72: miniredis implements NEITHER `CLUSTER INFO` NOR `CONFIG GET`, so it
	// presents exactly the two "unverifiable" cases the guards refuse. The emulator is, by
	// construction, neither clustered nor evicting (it is an in-process map), so the suite
	// makes both assertions ONCE, here, in the open — the same on-the-record form a client
	// on a managed Redis writes into quota-config.json. Not a bypass: the guards' own tests
	// clear these envs and assert the refusals, and a POSITIVE signal from the server
	// (cluster_enabled:1, allkeys-*) is refused regardless of any assertion.
	if _, set := os.LookupEnv(ClusterConfirmEnv); !set {
		_ = os.Setenv(ClusterConfirmEnv, "true")
	}
	if _, set := os.LookupEnv(redisguard.DurabilityConfirmEnv); !set {
		_ = os.Setenv(redisguard.DurabilityConfirmEnv, "true")
	}
	os.Exit(m.Run())
}

// envOr is the operator-gated real-Redis address helper (shared by the D-71/D-72 legs).
func envOr(key string) string { return os.Getenv(key) }

// ---------------------------------------------------------------------------
// 1. the pure decision (mirrors outbox.EvaluateDurability's split)
// ---------------------------------------------------------------------------

func TestEvaluateTopology_D71(t *testing.T) {
	cases := []struct {
		name              string
		enabled, found    bool
		confirmedDisabled bool
		want              string
	}{
		{"cluster_enabled:1 is CLUSTER", true, true, false, TopologyCluster},
		{"cluster_enabled:0 is single-node (control: not a blanket reject)", false, true, false, TopologySingleNode},
		{"unverifiable without assertion is UNKNOWN", false, false, false, TopologyUnknown},
		{"unverifiable WITH operator assertion is single-node", false, false, true, TopologySingleNode},
		// D-71 x D-32: the assertion rescues an ABSENT signal; it must NEVER override a
		// DEFINITIVE negative — exactly as redis_durability_confirmed cannot override an
		// allkeys-* eviction policy. CROSSSLOT does not care what the operator asserted.
		{"an assertion does NOT override a positive cluster signal", true, true, true, TopologyCluster},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := EvaluateTopology(tc.enabled, tc.found, tc.confirmedDisabled, "INFO cluster")
			if got != tc.want {
				t.Fatalf("topology = %q (want %q) — %s", got, tc.want, detail)
			}
		})
	}
}

func TestParseClusterEnabled_D71(t *testing.T) {
	if v, ok := ParseClusterEnabled("# Cluster\r\ncluster_enabled:1\r\n"); !ok || !v {
		t.Fatalf("cluster_enabled:1 → (%v,%v)", v, ok)
	}
	if v, ok := ParseClusterEnabled("# Cluster\r\ncluster_enabled:0\r\n"); !ok || v {
		t.Fatalf("cluster_enabled:0 → (%v,%v)", v, ok)
	}
	// Absence is not a value (D-51): a payload with no cluster_enabled is UNKNOWN,
	// never "safe". This is the REAL `CLUSTER INFO` payload of a cluster node — it
	// genuinely has no cluster_enabled field.
	if _, ok := ParseClusterEnabled("cluster_state:ok\r\ncluster_slots_assigned:16384\r\n"); ok {
		t.Fatal("a payload with no cluster_enabled field must report NOT-found")
	}
}

// ---------------------------------------------------------------------------
// 2. the live probe — INFO cluster first, CLUSTER INFO fallback
// ---------------------------------------------------------------------------
//
// Modelled on what REAL redis:7 servers do (verified against throwaway containers,
// see the artifact). The obvious model is WRONG, and only a real server says so:
//   - a NON-clustered redis:7 ERRORS on `CLUSTER INFO`
//     ("ERR This instance has cluster support disabled");
//   - a CLUSTER-enabled node ANSWERS `CLUSTER INFO`, but with NO cluster_enabled field.

type fakeProber struct {
	info, infoErr       string
	cluster, clusterErr string
}

func (f fakeProber) Info(ctx context.Context, section ...string) *redis.StringCmd {
	if f.infoErr != "" {
		return redis.NewStringResult("", errors.New(f.infoErr))
	}
	return redis.NewStringResult(f.info, nil)
}

func (f fakeProber) ClusterInfo(ctx context.Context) *redis.StringCmd {
	if f.clusterErr != "" {
		return redis.NewStringResult("", errors.New(f.clusterErr))
	}
	return redis.NewStringResult(f.cluster, nil)
}

func TestProbePriority_D71(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		f    fakeProber
		want string
	}{
		{"real cluster node", fakeProber{
			info: "# Cluster\r\ncluster_enabled:1\r\n", cluster: "cluster_state:ok\r\n"}, TopologyCluster},
		// The real-server trap: a correct single-node redis:7 ERRORS on CLUSTER INFO.
		// A CLUSTER-INFO-only guard would refuse it — a blanket reject of the happy path.
		{"real single-node (CLUSTER INFO errors)", fakeProber{
			info:       "# Cluster\r\ncluster_enabled:0\r\n",
			clusterErr: "ERR This instance has cluster support disabled"}, TopologySingleNode},
		{"trimmed INFO, cluster answers ⇒ cluster", fakeProber{
			infoErr: "ERR unsupported", cluster: "cluster_state:ok\r\ncluster_slots_assigned:16384\r\n"}, TopologyCluster},
		{"trimmed INFO, cluster support disabled ⇒ single-node", fakeProber{
			infoErr: "ERR unsupported", clusterErr: "ERR This instance has cluster support disabled"}, TopologySingleNode},
		{"neither probe answers ⇒ unknown", fakeProber{
			infoErr: "ERR unsupported", clusterErr: "ERR unknown command 'cluster'"}, TopologyUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// drives the REAL probe (ProbeClusterEnabled) with canned server answers
			got, detail := CheckRedisClusterTopology(ctx, tc.f, false)
			if got != tc.want {
				t.Fatalf("topology = %q (want %q) — %s", got, tc.want, detail)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. Setup REFUSES TO START — the whole point (D-71.2/.3)
// ---------------------------------------------------------------------------

// miniredis cannot be made to report cluster_enabled:1, so the real-cluster leg is
// operator-gated (AB0T_QUOTA_TEST_CLUSTER_ADDR; recipe in the artifact) and the
// emulator leg drives the UNVERIFIABLE branch — the one a managed Redis presents.
func TestSetup_RefusesUnverifiableTopology_WithoutOperatorAssertion_D71(t *testing.T) {
	t.Setenv(ClusterConfirmEnv, "")
	mr := miniredis.RunT(t)
	c := minimalConfig()
	c.Storage = config.StorageConfig{RedisURL: config.Declare("redis://" + mr.Addr())}

	_, err := Setup(context.Background(), Options{ConfigOverride: c})
	if err == nil {
		t.Fatal("an UNVERIFIABLE Redis topology must refuse to start (D-71) — unknown fails closed")
	}
	if !strings.Contains(err.Error(), "redis_cluster_confirmed_disabled") ||
		!strings.Contains(err.Error(), "CROSSSLOT") {
		t.Fatalf("the refusal must name the cause and the remedy, got: %v", err)
	}
}

func TestSetup_OperatorAssertion_AllowsStart_AndIsOnTheRecord_D71(t *testing.T) {
	t.Setenv(ClusterConfirmEnv, "")
	mr := miniredis.RunT(t)
	c := minimalConfig()
	c.Storage = config.StorageConfig{RedisURL: config.Declare("redis://" + mr.Addr()), RedisClusterConfirmedDisabled: true}

	q, err := Setup(context.Background(), Options{ConfigOverride: c})
	if err != nil {
		t.Fatalf("an explicit operator assertion must allow startup: %v", err)
	}
	defer q.Close(context.Background())

	got := q.Capabilities().RedisTopology
	if !strings.HasPrefix(got, TopologySingleNode) || !strings.Contains(strings.ToLower(got), "assert") {
		t.Fatalf("the operator assertion must be VISIBLE in Capabilities, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// 4. the guard has a SINK — Healthy() (D-40 / D-49 / D-51)
// ---------------------------------------------------------------------------

func TestHealthy_FailsOnBadTopology_D71(t *testing.T) {
	base := Capabilities{BillingStatus: "OFF (paid disabled)", Reconciler: "OFF — not requested"}
	for _, tc := range []struct {
		topology string
		healthy  bool
	}{
		{TopologySingleNode, true}, // control: a verified topology is healthy
		{TopologyNA, true},         // no redis counter store ⇒ no cluster to break on
		{TopologyCluster, false},   // CROSSSLOT: the counter cannot run here
		{TopologyUnknown, false},   // unknown fails closed (D-51)
		{"", false},                // ABSENCE is not health (D-49)
		{"starting", false},        // unparseable is not affirmative
	} {
		cap := base
		cap.RedisTopology = tc.topology
		q := &Quota{capability: cap}
		ok, reasons := q.Healthy()
		if ok != tc.healthy {
			t.Errorf("topology %q → Healthy()=%v (want %v), reasons=%v", tc.topology, ok, tc.healthy, reasons)
		}
		if reasons["redis_topology"] != tc.topology {
			t.Errorf("Healthy() must name the topology reason, got %q", reasons["redis_topology"])
		}
	}
}

// ---------------------------------------------------------------------------
// 5. cross-runtime contract (D-43) — the structural conformance item
// ---------------------------------------------------------------------------

type structuralItem struct {
	ID                      string   `json:"id"`
	Runtimes                []string `json:"runtimes"`
	CapabilityKey           string   `json:"capability_key"`
	ConfigKey               string   `json:"config_key"`
	EnvKey                  string   `json:"env_key"`
	ClusterErrorMustContain []string `json:"cluster_error_must_contain"`
	UnknownErrorMustContain []string `json:"unknown_error_must_contain"`
}

// The topology guard is a SETUP-level contract (there is no engine scenario that can
// boot a library against a clustered Redis), so it is registered as a STRUCTURAL
// conformance item rather than forced into a scenario row that would not execute what
// it claims. Both runtimes assert their OWN refusal against the SAME declared tokens —
// one data file, two runners (D-43). Python's twin: TestStructuralConformance.
func TestGoSatisfiesDeclaredStructuralItem_ST_TOPOLOGY_1(t *testing.T) {
	raw, err := os.ReadFile("../conformance/scenarios.json")
	if err != nil {
		t.Fatalf("read scenarios.json: %v", err)
	}
	var doc struct {
		Structural []structuralItem `json:"structural_conformance"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse scenarios.json: %v", err)
	}
	var item *structuralItem
	for i := range doc.Structural {
		if doc.Structural[i].ID == "ST-TOPOLOGY-1" {
			item = &doc.Structural[i]
		}
	}
	if item == nil {
		t.Fatal("ST-TOPOLOGY-1 must be declared in scenarios.json")
	}

	clusterMsg := TopologyError(TopologyCluster, "INFO cluster reports cluster_enabled:1").Error()
	for _, tok := range item.ClusterErrorMustContain {
		if !strings.Contains(clusterMsg, tok) {
			t.Errorf("declared token %q missing from the Go cluster refusal", tok)
		}
	}
	unknownMsg := TopologyError(TopologyUnknown, "topology unverifiable").Error()
	for _, tok := range item.UnknownErrorMustContain {
		if !strings.Contains(unknownMsg, tok) {
			t.Errorf("declared token %q missing from the Go unknown refusal", tok)
		}
	}
	if item.CapabilityKey != "redis_topology" || item.ConfigKey != "storage.redis_cluster_confirmed_disabled" {
		t.Errorf("declared keys drifted from the Go implementation: %+v", item)
	}
	if item.EnvKey != ClusterConfirmEnv {
		t.Errorf("declared env key %q != Go %q", item.EnvKey, ClusterConfirmEnv)
	}
}

// ---------------------------------------------------------------------------
// 6. REAL cluster (operator-gated; recipe in the artifact)
// ---------------------------------------------------------------------------

func TestRealClusteredRedis_IsRefused_D71(t *testing.T) {
	addr := os.Getenv("AB0T_QUOTA_TEST_CLUSTER_ADDR")
	if addr == "" {
		t.Skip("AB0T_QUOTA_TEST_CLUSTER_ADDR not set — real-cluster leg is operator-gated")
	}
	t.Setenv(ClusterConfirmEnv, "")
	c := minimalConfig()
	c.Storage = config.StorageConfig{RedisURL: config.Declare("redis://" + addr)}

	_, err := Setup(context.Background(), Options{ConfigOverride: c})
	if err == nil {
		t.Fatal("a REAL clustered Redis must refuse to start (D-71)")
	}
	if !strings.Contains(err.Error(), "CROSSSLOT") || !strings.Contains(err.Error(), "cluster_enabled:1") {
		t.Fatalf("the refusal must name the cause, got: %v", err)
	}
}

func TestRealSingleNodeRedis_IsAccepted_D71(t *testing.T) {
	addr := os.Getenv("AB0T_QUOTA_TEST_REAL_ADDR")
	if addr == "" {
		t.Skip("AB0T_QUOTA_TEST_REAL_ADDR not set — real single-node leg is operator-gated")
	}
	// [control] a real, non-clustered redis:7 must PASS — otherwise the guard is a
	// blanket reject that breaks every honest client. (It ERRORS on CLUSTER INFO;
	// only `INFO cluster` gets this right.)
	t.Setenv(ClusterConfirmEnv, "")
	c := minimalConfig()
	c.Storage = config.StorageConfig{RedisURL: config.Declare("redis://" + addr)}

	q, err := Setup(context.Background(), Options{ConfigOverride: c})
	if err != nil {
		t.Fatalf("a REAL single-node redis must be accepted, got: %v", err)
	}
	defer q.Close(context.Background())
	if q.Capabilities().RedisTopology != TopologySingleNode {
		t.Fatalf("real single-node reported %q", q.Capabilities().RedisTopology)
	}
}
