package quota

// D-71 — machine-check the Redis TOPOLOGY at startup. The Go twin of Python's
// ab0t_quota/topology.py; identical behaviour is asserted by the structural
// conformance item ST-TOPOLOGY-1 in conformance/scenarios.json (D-43).
//
// The atomic counter is implemented with MULTI-KEY Lua scripts (acquireSrc and
// the incr/decr family: idempotency key + org/user counter keys). On a Redis
// Cluster those keys hash to different slots and every one of them fails with
// CROSSSLOT — observed at a real clustered server (D-23,
// information_real_redis_conformance_20260711.md §4; Go's acquireSrc is
// byte-identical to Python's, so it CROSSSLOTs identically).
//
// Our own prod Redis is single-node, so WE never hit it. But this is a LIBRARY:
// a client on a clustered Redis writes one quota-config.json (the drop-in
// promise), boots, and the counter primitive fails outright at the first
// Acquire — with no startup signal explaining why. "Drop-in" must mean "it tells
// you at startup that your setup will not work", never "it silently breaks".
//
// Same shape as the durability machine-check (D-32, outbox/durability.go):
//   - a DEFINITIVE negative (cluster_enabled:1, like an allkeys-* eviction
//     policy) is a hard refusal NO operator flag can override;
//   - an ABSENT signal (topology unprobeable) is refused UNLESS the operator puts
//     an assertion on the record (storage.redis_cluster_confirmed_disabled);
//   - the verdict lands in Capabilities and FAILS Healthy() — an event with no
//     sink is not observability (D-40); absence is not health (D-49/D-51).
//
// Cluster SUPPORT (hash-tagged quota:{org} keyspace) is gated roadmap work (D-23).
// v1 ships an honest refusal: refusing loudly is shippable; breaking silently is not.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/redisguard"
)

// Capability values for Capabilities.RedisTopology.
const (
	TopologySingleNode = "single-node"
	TopologyCluster    = "CLUSTER (unsupported)"
	TopologyUnknown    = "unknown"
	// TopologyNA — no Redis counter store at all (in-memory): there is no cluster
	// to CROSSSLOT on. An affirmative statement, not an absence.
	TopologyNA = "n/a (no redis counter store)"

	// ClusterConfirmEnv mirrors storage.redis_cluster_confirmed_disabled (config wins).
	ClusterConfirmEnv = "AB0T_QUOTA_REDIS_CLUSTER_CONFIRMED_DISABLED"
)

// ErrClusterTopology is the typed startup refusal (D-71).
var ErrClusterTopology = errors.New("quota: unsupported or unverifiable Redis topology")

const clusterRefusal = "ab0t-quota requires a NON-CLUSTERED Redis. The atomic counter is implemented with " +
	"multi-key Lua scripts (idempotency key + org/user counter keys); on a Redis Cluster those keys hash to " +
	"different slots and EVERY counter script fails with CROSSSLOT — so the library would admit work it cannot " +
	"count. Your Redis reports cluster_enabled:1. Remedy: point storage.redis_url at a single-node " +
	"(non-clustered) Redis. Hash-tagged keyspace support for Redis Cluster is on the roadmap (D-23); until it " +
	"ships, ab0t-quota refuses to start rather than break silently at the first acquire. " +
	"(storage.redis_cluster_confirmed_disabled does NOT override a positive cluster_enabled:1 signal — it " +
	"exists only for a Redis whose topology cannot be probed.)"

const unknownRefusal = "ab0t-quota could not VERIFY the Redis topology: neither `INFO cluster` nor `CLUSTER INFO` " +
	"gave a usable answer (some managed Redis trim INFO and disable CLUSTER). The atomic counter uses multi-key " +
	"Lua scripts that fail with CROSSSLOT on a Redis Cluster, so an unverified topology cannot be assumed safe — " +
	"unknown fails closed. Remedy: use a Redis whose `INFO cluster` is reachable, or — if you KNOW this Redis is " +
	"not clustered — put that assertion on the record by setting storage.redis_cluster_confirmed_disabled: true " +
	"in quota-config.json (env: AB0T_QUOTA_REDIS_CLUSTER_CONFIRMED_DISABLED=true)."

// TopologyError builds the loud, typed refusal. It names the CAUSE and the
// REMEDY — a refusal a client cannot act on is just an outage.
func TopologyError(topology, detail string) error {
	head := unknownRefusal
	if topology == TopologyCluster {
		head = clusterRefusal
	}
	return fmt.Errorf("%w: %s [detail: %s]", ErrClusterTopology, head, detail)
}

// ParseClusterEnabled pulls cluster_enabled:{0,1} out of an INFO payload.
// Returns (value, found) — found=false is UNKNOWN, never "safe" (D-51).
func ParseClusterEnabled(raw string) (bool, bool) {
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "cluster_enabled:") {
			return strings.TrimSpace(strings.SplitN(line, ":", 2)[1]) == "1", true
		}
	}
	return false, false
}

// TopologyProber is the narrow Redis surface the topology probe needs — satisfied
// by *redis.Client (redis.Cmdable). Narrow so a test can drive the REAL probe with
// canned server answers instead of re-implementing it (a test of a copy of the logic
// asserts nothing about the logic).
type TopologyProber interface {
	Info(ctx context.Context, section ...string) *redis.StringCmd
	ClusterInfo(ctx context.Context) *redis.StringCmd
}

// ProbeClusterEnabled asks the client's Redis whether it is clustered.
// Returns (enabled, found, probe-description).
//
// `INFO cluster`, NOT `CLUSTER INFO` — and the distinction is load-bearing,
// verified against real redis:7 servers (an emulator would have agreed with
// whatever we assumed):
//   - a NON-clustered redis:7 ERRORS on `CLUSTER INFO`
//     ("ERR This instance has cluster support disabled");
//   - a CLUSTER-enabled node ANSWERS `CLUSTER INFO`, but its payload carries NO
//     cluster_enabled field at all.
//
// So a CLUSTER-INFO-only guard would refuse every correct single-node deployment
// and could not even parse the cluster. `INFO cluster` answers cluster_enabled on
// BOTH and is the primary probe; CLUSTER INFO is the fallback for a trimmed INFO,
// where *answering at all* is the positive cluster signal and the "cluster support
// disabled" error is the positive single-node signal.
func ProbeClusterEnabled(ctx context.Context, c TopologyProber) (bool, bool, string) {
	infoWhy := "no cluster_enabled field"
	if raw, err := c.Info(ctx, "cluster").Result(); err != nil {
		infoWhy = err.Error()
	} else if v, ok := ParseClusterEnabled(raw); ok {
		return v, true, "INFO cluster"
	}

	raw, err := c.ClusterInfo(ctx).Result()
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "cluster support disabled") {
			return false, true, "CLUSTER INFO (server: cluster support disabled)"
		}
		return false, false, fmt.Sprintf("INFO cluster [%s]; CLUSTER INFO [%s]", infoWhy, err.Error())
	}
	if v, ok := ParseClusterEnabled(raw); ok {
		return v, true, "CLUSTER INFO"
	}
	if strings.Contains(raw, "cluster_state") {
		// It answered CLUSTER INFO at all ⇒ this server runs in cluster mode.
		return true, true, "CLUSTER INFO (answered ⇒ cluster mode)"
	}
	return false, false, fmt.Sprintf("INFO cluster [%s]; CLUSTER INFO [unparseable]", infoWhy)
}

// EvaluateTopology is the pure topology decision (D-71), separated from the Redis
// plumbing so it is directly testable — the same split as EvaluateDurability (D-32).
// found=false means the probes could not verify the topology.
// Returns (topology, human_reason).
func EvaluateTopology(clusterEnabled, found, confirmedDisabled bool, probe string) (string, string) {
	if probe == "" {
		probe = "topology probe"
	}
	if found && clusterEnabled {
		// A DEFINITIVE negative. NOT overridable by the operator assertion — that
		// assertion exists for an ABSENT signal, exactly as redis_durability_confirmed
		// cannot override an allkeys-* eviction policy (D-32). CROSSSLOT does not care
		// what anyone asserted.
		return TopologyCluster, probe + " reports cluster_enabled:1"
	}
	if found {
		return TopologySingleNode, probe + " reports cluster_enabled:0"
	}
	if confirmedDisabled {
		return TopologySingleNode, fmt.Sprintf(
			"%s (topology unverifiable — %s; non-clustered asserted by the operator: "+
				"storage.redis_cluster_confirmed_disabled=true)", TopologySingleNode, probe)
	}
	return TopologyUnknown, fmt.Sprintf(
		"topology unverifiable (%s) and storage.redis_cluster_confirmed_disabled is not set — "+
			"an unverified topology is not a safe one", probe)
}

// CheckRedisClusterTopology asks the client's Redis what it is → (topology, reason).
// Never returns an error: the caller decides what to do with UNKNOWN/CLUSTER.
func CheckRedisClusterTopology(ctx context.Context, c TopologyProber, confirmedDisabled bool) (string, string) {
	enabled, found, probe := ProbeClusterEnabled(ctx, c)
	return EvaluateTopology(enabled, found, confirmedDisabled, probe)
}

// clusterConfirmedDisabled reads the operator's on-the-record assertion: config
// first, env as the equivalent. An assertion is a positive act, never a default.
func clusterConfirmedDisabled(sc config.StorageConfig) bool {
	if sc.RedisClusterConfirmedDisabled {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ClusterConfirmEnv))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// TopologyCapabilityValue is what lands on Capabilities.RedisTopology and is read
// by Healthy() — so a bad topology FAILS the probe rather than terminating in a log
// line nobody reads (D-40). An operator assertion is carried into the value: the
// record shows WHY we believe the topology is safe.
func TopologyCapabilityValue(topology, detail string) string {
	if topology == TopologySingleNode {
		if strings.HasPrefix(detail, TopologySingleNode) {
			return detail
		}
		return TopologySingleNode
	}
	return topology
}

// gateCounterStore runs the D-72/D-73/D-74 preflight against the counter's Redis and
// REFUSES to start on anything unsafe. Every verdict is written to Capabilities BEFORE any
// refusal, so the cause is readable (D-40/D-49/D-51).
//
//   - D-72 (eviction): an allkeys-* Redis EVICTS a live gauge → the counter reads zero for
//     a running resource → over-admission. Hard error. CONFIG unavailable → the explicit
//     on-the-record storage.redis_durability_confirmed assertion (D-32's shape); an
//     assertion never overrides a policy the server actually reported.
//   - D-73 (scripting): SCRIPT LOAD the REAL acquire source at boot.
//   - D-74 (version floor): below the floor → refuse. Unreadable → `unknown`, which
//     degrades the probe but does not refuse (a deliberate, stated deviation — a version we
//     cannot read is not the hazard an eviction policy we cannot read is).
func gateCounterStore(ctx context.Context, c redisguard.Prober, cfg *config.Config, cap *Capabilities) error {
	confirmed := cfg.Storage.RedisDurabilityConfirmed || redisguard.EnvDurabilityConfirmed()

	ok, detail := redisguard.CheckCounterEviction(ctx, c, confirmed)
	if ok {
		cap.CounterEvictionPolicy = detail
	} else {
		cap.CounterEvictionPolicy = "EVICTING/UNVERIFIED (" + detail + ")"
	}
	if !ok {
		slog.Error("COUNTER STORE UNSAFE (D-72) — refusing to start", "detail", detail)
		return redisguard.CounterEvictionError(detail)
	}
	slog.Info("counter eviction policy verified (D-72)", "policy", detail)

	scriptOK, scriptDetail := redisguard.CheckScriptCapability(ctx, c)
	if scriptOK {
		cap.RedisScripting = scriptDetail
	} else {
		cap.RedisScripting = "OFF (" + scriptDetail + ")"
		slog.Error("REDIS CANNOT RUN THE COUNTER'S LUA (D-73) — refusing to start", "detail", scriptDetail)
		return redisguard.ScriptingError(scriptDetail)
	}

	status, verDetail := redisguard.CheckVersion(ctx, c, redisguard.VersionFloor)
	switch status {
	case "ok":
		cap.RedisVersion = verDetail
	case "below_floor":
		cap.RedisVersion = "below_floor (" + verDetail + ")"
		slog.Error("REDIS BELOW SUPPORTED VERSION FLOOR (D-74) — refusing to start", "detail", verDetail)
		return redisguard.VersionError(verDetail)
	default:
		cap.RedisVersion = "unknown (" + verDetail + ")"
		slog.Warn("Redis version could not be verified (D-74)", "detail", verDetail)
	}

	// D-77 — memory headroom. `noeviction` fails CLOSED at the cliff (writes OOM → Acquire
	// errors → admission denies): the SAFE direction, but the service DIES. So this never
	// refuses; it DEGRADES on the way there, so the cliff is visible before 3am.

	memStatus, memDetail := redisguard.CheckMemoryHeadroom(ctx, c)
	switch memStatus {
	case "low_headroom":
		cap.MemoryHeadroom = "low_headroom (" + memDetail + ")"
		slog.Error("REDIS MEMORY HEADROOM LOW (D-77)", "detail", memDetail)
	case "ok":
		cap.MemoryHeadroom = memDetail
	default:
		cap.MemoryHeadroom = memStatus
	}
	return nil
}

// CounterStoreOK is the D-72 health predicate: only a policy READ from the server (or an
// on-the-record assertion) and known non-evicting is healthy. Missing, empty, `unknown`,
// `EVICTING/…`, or any allkeys-* value is NOT (D-49/D-51).
func CounterStoreOK(v string) bool {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" || strings.HasPrefix(s, "unknown") || strings.HasPrefix(s, "evicting") {
		return false
	}
	for p := range redisguard.EvictingPolicies {
		if strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// ScriptingOK is the D-73 health predicate: only an affirmative "on" is healthy.
func ScriptingOK(v string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "on")
}

// TopologyOK is the health predicate. Only an affirmative single-node (or n/a —
// no Redis counter store, hence no cluster to break on) is healthy; missing, empty,
// unknown, or CLUSTER is NOT (D-49/D-51: absence is not a value; unknown fails closed).
func TopologyOK(v string) bool {
	s := strings.ToLower(strings.TrimSpace(v))
	return strings.HasPrefix(s, TopologySingleNode) || strings.HasPrefix(s, "n/a")
}

// ---------------------------------------------------------------------------
// D-75 — the invariants, re-verifiable at ANY time
// ---------------------------------------------------------------------------

// verifyRedisInvariants re-runs the WHOLE Redis preflight (topology, eviction, scripting,
// version, headroom) and reports the truth about the world NOW. Same judgement as the boot
// gates; different consequence. It never returns an error: at BOOT a violation is a refusal,
// at RUNTIME it is LOUD, NOT FATAL — degrade health, alert, keep serving. A running service
// that suddenly refuses is its own outage; the operator decides whether to drain.
//
// "An assumption machine-checked once is an assumption trusted thereafter" (D-75).
func verifyRedisInvariants(ctx context.Context, c redisguard.Prober, cfg *config.Config, outboxOnRedis bool) (map[string]string, [][2]string) {
	caps := map[string]string{}
	var unsafe [][2]string

	topo, topoDetail := CheckRedisClusterTopology(ctx, c.(TopologyProber),
		cfg.Storage.RedisClusterConfirmedDisabled || envClusterConfirmed())
	caps["redis_topology"] = TopologyCapabilityValue(topo, topoDetail)
	if topo != TopologySingleNode {
		unsafe = append(unsafe, [2]string{"redis_topology", topoDetail})
	}

	confirmed := cfg.Storage.RedisDurabilityConfirmed || redisguard.EnvDurabilityConfirmed()
	ok, detail := redisguard.CheckCounterEviction(ctx, c, confirmed)
	if ok {
		caps["counter_eviction_policy"] = detail
	} else {
		caps["counter_eviction_policy"] = "EVICTING/UNVERIFIED (" + detail + ")"
		unsafe = append(unsafe, [2]string{"counter_eviction_policy", detail})
	}

	scriptOK, scriptDetail := redisguard.CheckScriptCapability(ctx, c)
	if scriptOK {
		caps["redis_scripting"] = scriptDetail
	} else {
		caps["redis_scripting"] = "OFF (" + scriptDetail + ")"
		unsafe = append(unsafe, [2]string{"redis_scripting", scriptDetail})
	}

	status, verDetail := redisguard.CheckVersion(ctx, c, redisguard.VersionFloor)
	switch status {
	case "ok":
		caps["redis_version"] = verDetail
	case "below_floor":
		caps["redis_version"] = "below_floor (" + verDetail + ")"
		unsafe = append(unsafe, [2]string{"redis_version", verDetail})
	default:
		caps["redis_version"] = "unknown (" + verDetail + ")"
	}

	// D-80 the FACT: has this server ALREADY evicted? A corrected policy hides a counter that
	// is already wrong — this is the one check that catches damage the config no longer admits.
	evicted, found := redisguard.CheckEvictedKeys(ctx, c)
	factStatus, factDetail := redisguard.EvaluateEvictionFacts(evicted, found)
	switch factStatus {
	case "evictions_observed":
		caps["counter_evictions_observed"] = "evictions_observed (" + factDetail + ")"
		unsafe = append(unsafe, [2]string{"counter_evictions_observed", factDetail})
	case "ok":
		caps["counter_evictions_observed"] = factDetail
	default:
		caps["counter_evictions_observed"] = "unknown"
	}

	// D-81 the FACT: persistence is configured — is it WORKING? Severity BY CONSEQUENCE (the
	// D-76 lesson, not uniformity): a failing AOF on the Redis holding the OUTBOX is money
	// nobody can reconstruct. The same failure on a Redis that only holds the COUNTER is not:
	// the counter HEALS (reconciler → Σ open activations, D-28). Reporting both as money loss
	// would be the D-49 false-503 mistake.
	pStatus, pDetail := redisguard.EvaluatePersistFacts(redisguard.CheckPersistFacts(ctx, c))
	switch {
	case pStatus == "persist_failing" && outboxOnRedis:
		caps["redis_persist_status"] = "FAILING (" + pDetail + ")"
		unsafe = append(unsafe, [2]string{"redis_persist_status", pDetail})
	case pStatus == "persist_failing":
		caps["redis_persist_status"] = "persistence failing, but the OUTBOX is not on this Redis — " +
			"the counter heals (reconciler → Σ open activations). Fix it anyway: " + pDetail
		slog.Warn("redis persistence is FAILING (D-81), though the outbox is elsewhere", "detail", pDetail)
	case pStatus == "ok":
		caps["redis_persist_status"] = pDetail
	default:
		caps["redis_persist_status"] = "unknown"
	}

	memStatus, memDetail := redisguard.CheckMemoryHeadroom(ctx, c)
	switch memStatus {
	case "low_headroom":
		caps["memory_headroom"] = "low_headroom (" + memDetail + ")"
		unsafe = append(unsafe, [2]string{"memory_headroom", memDetail})
	case "ok":
		caps["memory_headroom"] = memDetail
	default:
		caps["memory_headroom"] = memStatus
	}
	return caps, unsafe
}

// envClusterConfirmed reports the env form of the D-71 operator assertion.
func envClusterConfirmed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ClusterConfirmEnv))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// makeRevalidator (D-75) builds the periodic re-verification that RIDES the reconciler loop
// (never its own worker — D-50). It updates Capabilities (so Healthy() degrades immediately),
// fires a money-incident alert on each NEW violation, and a paired `restored` when it heals.
// It never panics and never stops the process.
func (q *Quota) makeRevalidator(c redisguard.Prober, cfg *config.Config) func(context.Context) {
	return func(ctx context.Context) {
		q.capMu.RLock()
		outboxOnRedis := q.outboxOnRedis
		q.capMu.RUnlock()
		caps, unsafe := verifyRedisInvariants(ctx, c, cfg, outboxOnRedis)

		nowUnsafe := map[string]bool{}
		for _, u := range unsafe {
			nowUnsafe[u[0]] = true
		}

		q.capMu.Lock()
		prev := q.unsafeInvariants
		if prev == nil {
			prev = map[string]bool{}
		}
		q.capability.RedisTopology = caps["redis_topology"]
		q.capability.CounterEvictionPolicy = caps["counter_eviction_policy"]
		q.capability.RedisScripting = caps["redis_scripting"]
		q.capability.RedisVersion = caps["redis_version"]
		q.capability.MemoryHeadroom = caps["memory_headroom"]
		q.capability.CounterEvictionsObserved = caps["counter_evictions_observed"]
		q.capability.RedisPersistStatus = caps["redis_persist_status"]
		// D-80: an OBSERVED eviction means the COUNTER itself is untrustworthy (an evicted gauge
		// reads LOW). Mark it — the reconcile pass runs immediately after this re-check in the
		// SAME loop tick, so convergence back to Σ open activations is STRUCTURAL, not a callback
		// somebody has to remember to wire (which is how half the defects in this ticket began).
		q.counterUntrusted = nowUnsafe["counter_evictions_observed"]
		q.unsafeInvariants = nowUnsafe
		q.capMu.Unlock()

		for _, u := range unsafe {
			if !prev[u[0]] {
				slog.Error("REDIS INVARIANT VIOLATED AT RUNTIME (D-75) — the library verified this "+
					"at startup and it has CHANGED underneath us. Health is degraded; the service keeps "+
					"serving (a sudden refusal is its own outage) — drain or fix.",
					"capability", u[0], "detail", u[1])
			}
			if q.Alerts != nil {
				q.Alerts.NotifyInvariantViolated(ctx, u[0], u[1])
			}
		}
		for name := range prev {
			if !nowUnsafe[name] {
				slog.Info("redis invariant RESTORED (D-75)", "capability", name)
				if q.Alerts != nil {
					q.Alerts.NotifyInvariantRestored(ctx, name)
				}
			}
		}
	}
}
