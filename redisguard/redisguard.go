// Package redisguard machine-checks every infrastructure assumption ab0t-quota makes
// about the client's Redis, at boot, and refuses loudly (D-32/D-71/D-72/D-73/D-74).
//
// It owns the ONE durability implementation — outbox.CheckRedisDurability now delegates
// here. A second copy of the same judgement is D-35's mistake in a new costume.
//
// D-72 — the counter may not live on an evicting Redis (the urgent one).
// maxmemory-policy=allkeys-* lets Redis evict ANY key under memory pressure, including a
// LIVE gauge. The counter then reads ZERO for a resource that is still running:
// under-count → phantom headroom → over-admission (D-31's forbidden direction). The
// counter is not a cache of convenience; it IS the admission gate. And unlike D-71 this
// never announces itself: D-71 refuses loudly at boot, D-72 fails SILENTLY at runtime, as
// free quota, behind a green health check. A loud refusal is a support ticket; a silently
// evicted gauge is unbilled revenue and an over-admitted customer.
//
// Note the asymmetry with the OUTBOX check: the counter's fatal property is EVICTION, not
// persistence. A restart-lost counter heals (the reconciler converges it to Σ open
// activations, D-28); an evicted counter silently under-counts while the process keeps
// serving. So appendonly=no alone does not block startup — over-refusing trains operators
// to ignore the guard (D-49's false-503 lesson). The outbox holds money events nothing can
// reconstruct, so it needs BOTH.
//
// D-73 — scripting. Every counter op is EVAL. SCRIPT LOAD of the REAL acquire source at
// boot is definitive (loading `return 1` proves nothing about our scripts) and warms the
// script cache.
//
// D-74 — a version floor, asserted at boot.
//
// The law throughout: a DEFINITIVE negative is a hard, unoverridable refusal; an ABSENT
// signal (CONFIG unavailable — ElastiCache disables it) needs an explicit operator
// assertion on the record (storage.redis_durability_confirmed); absence is never health
// (D-49/D-51).
package redisguard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/ab0t-com/ab0t-quota-go/counters"
)

// EvictingPolicies evict ANY key — a live gauge included.
var EvictingPolicies = map[string]bool{
	"allkeys-lru": true, "allkeys-lfu": true, "allkeys-random": true,
}

// VersionFloor is the oldest Redis this library is tested against (D-74).
var VersionFloor = [3]int{6, 0, 0}

// DurabilityConfirmEnv mirrors storage.redis_durability_confirmed (config wins).
const DurabilityConfirmEnv = "AB0T_QUOTA_REDIS_DURABILITY_CONFIRMED"

// Typed startup refusals.
var (
	ErrCounterEviction      = errors.New("quota: unsafe Redis counter store (eviction)")
	ErrScriptingUnsupported = errors.New("quota: Redis cannot run the counter's Lua scripts")
	ErrRedisVersion         = errors.New("quota: Redis below the supported version floor")
)

// Prober is the narrow Redis surface the preflight needs — satisfied by *redis.Client.
// Narrow so tests drive the REAL checks with canned server answers rather than
// re-implementing them (a test of a copy of the logic asserts nothing about the logic).
type Prober interface {
	ConfigGet(ctx context.Context, parameter string) *redis.MapStringStringCmd
	ScriptLoad(ctx context.Context, script string) *redis.StringCmd
	Info(ctx context.Context, section ...string) *redis.StringCmd
}

// EnvDurabilityConfirmed reports the env form of the operator's assertion.
func EnvDurabilityConfirmed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(DurabilityConfirmEnv))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// ReadPolicy does the shared CONFIG GET. Returns (policy, appendonly, save, unavailable).
func ReadPolicy(ctx context.Context, c Prober) (string, string, string, bool) {
	get := func(k string) (string, error) {
		m, err := c.ConfigGet(ctx, k).Result()
		if err != nil {
			return "", err
		}
		return strings.ToLower(strings.TrimSpace(m[k])), nil
	}
	policy, perr := get("maxmemory-policy")
	appendonly, aerr := get("appendonly")
	save, serr := get("save")
	return policy, appendonly, save, perr != nil || aerr != nil || serr != nil
}

// EvaluateEviction is D-72's pure decision: may the COUNTER live on this Redis?
// An allkeys-* policy READ from the server is a DEFINITIVE negative and is NOT overridable
// by the operator assertion — asserting that an evicting Redis does not evict does not stop
// it evicting (D-32's law: redis_durability_confirmed never overrode allkeys-* either).
func EvaluateEviction(policy string, configUnavailable, confirmed bool) (bool, string) {
	policy = strings.ToLower(strings.TrimSpace(policy))
	if configUnavailable {
		if confirmed {
			return true, "CONFIG unavailable (e.g. ElastiCache); a non-evicting policy is asserted by the operator (storage.redis_durability_confirmed=true)"
		}
		return false, "Redis CONFIG is unavailable and storage.redis_durability_confirmed is not set — the counter's eviction policy cannot be verified, and an unverified counter store is not a safe one"
	}
	if EvictingPolicies[policy] {
		return false, fmt.Sprintf("maxmemory-policy=%s can EVICT a live gauge key — the counter would read zero for a resource that is still running (under-count → phantom headroom → over-admission). Use noeviction (or a volatile-* policy: the counter keys carry no TTL)", policy)
	}
	if policy == "" {
		policy = "unset"
	}
	return true, policy
}

// EvaluateDurability is D-32's pure decision (the OUTBOX): eviction AND persistence.
// Money events cannot be reconstructed, so the outbox needs both; the counter needs only
// the first.
func EvaluateDurability(policy, appendonly, save string, configUnavailable, confirmed bool) (bool, string) {
	policy = strings.ToLower(strings.TrimSpace(policy))
	appendonly = strings.ToLower(strings.TrimSpace(appendonly))
	save = strings.TrimSpace(save)
	if ok, _ := EvaluateEviction(policy, configUnavailable, confirmed); !ok {
		if configUnavailable {
			return false, "Redis CONFIG unavailable and redis_durability_confirmed not set — cannot verify persistence/eviction; treated as NON-durable"
		}
		return false, fmt.Sprintf("maxmemory-policy=%s can silently evict pending money events; use noeviction (or a volatile-* policy with no TTL on outbox keys)", policy)
	}
	if configUnavailable { // confirmed
		return true, "Redis CONFIG unavailable (e.g. ElastiCache); durability asserted by operator (redis_durability_confirmed=true)"
	}
	if !(appendonly == "yes" || save != "" || confirmed) {
		return false, "no Redis persistence (appendonly=no and no save points) — a restart/failover loses pending events; enable appendonly or RDB save"
	}
	return true, fmt.Sprintf("maxmemory-policy=%s, appendonly=%s", nz(policy), nz(appendonly))
}

// CheckDurability — D-32 (the outbox): persistence + a non-evicting policy — AND (D-81)
// whether the persistence is actually WORKING. A Redis with `appendonly yes` whose
// aof_last_write_status is `err` is NOT durable, however green its configuration reads:
// asking only the config is asking only the intent. The existing boot gate (a paid service
// that cannot durably bill must not start) then refuses this Redis for free.
func CheckDurability(ctx context.Context, c Prober, confirmed bool) (bool, string) {
	policy, appendonly, save, unavailable := ReadPolicy(ctx, c)
	durable, reason := EvaluateDurability(policy, appendonly, save, unavailable, confirmed)
	if !durable {
		return durable, reason
	}
	if status, detail := EvaluatePersistFacts(CheckPersistFacts(ctx, c)); status == "persist_failing" {
		return false, detail
	}
	return true, reason
}

// CheckCounterEviction — D-72 (the counter): a non-evicting policy. Same CONFIG read, same
// law, correct severity: the counter tolerates a restart but never an eviction.
func CheckCounterEviction(ctx context.Context, c Prober, confirmed bool) (bool, string) {
	policy, _, _, unavailable := ReadPolicy(ctx, c)
	return EvaluateEviction(policy, unavailable, confirmed)
}

// CheckScriptCapability — D-73. SCRIPT LOAD the REAL acquire source, so a Redis that cannot
// run our Lua is a STARTUP refusal, not a first-acquire outage.
func CheckScriptCapability(ctx context.Context, c Prober) (bool, string) {
	sha, err := c.ScriptLoad(ctx, counters.AcquireSrc).Result()
	if err != nil {
		return false, fmt.Sprintf("SCRIPT LOAD of the counter's acquire script failed (%s) — this Redis cannot run the atomic counter", err.Error())
	}
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return true, fmt.Sprintf("on (EVAL verified, acquire sha=%s…)", sha)
}

var verRe = regexp.MustCompile(`(\d+)\.(\d+)\.?(\d+)?`)

// EvaluateVersion returns (status, detail): "ok" | "below_floor" | "unknown".
func EvaluateVersion(version string, floor [3]int) (string, string) {
	if strings.TrimSpace(version) == "" {
		return "unknown", "Redis version could not be read (INFO unavailable) — the supported floor could not be verified"
	}
	m := verRe.FindStringSubmatch(version)
	if m == nil {
		return "unknown", fmt.Sprintf("unparseable Redis version %q", version)
	}
	var parts [3]int
	for i := 0; i < 3; i++ {
		if m[i+1] != "" {
			parts[i], _ = strconv.Atoi(m[i+1])
		}
	}
	floorS := fmt.Sprintf("%d.%d.%d", floor[0], floor[1], floor[2])
	if less(parts, floor) {
		return "below_floor", fmt.Sprintf("Redis %s is below ab0t-quota's supported floor %s", version, floorS)
	}
	return "ok", version
}

// CheckVersion — D-74.
func CheckVersion(ctx context.Context, c Prober, floor [3]int) (string, string) {
	raw, err := c.Info(ctx, "server").Result()
	if err != nil {
		return EvaluateVersion("", floor)
	}
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "redis_version:") {
			return EvaluateVersion(strings.TrimSpace(strings.SplitN(line, ":", 2)[1]), floor)
		}
	}
	return EvaluateVersion("", floor)
}

// ---- the loud, typed refusals: name the CAUSE and the REMEDY --------------------------

// CounterEvictionError (D-72).
func CounterEvictionError(detail string) error {
	return fmt.Errorf("%w: ab0t-quota cannot run its COUNTER on this Redis. The counter is not a cache "+
		"of convenience — it IS the admission gate. Under an `allkeys-*` maxmemory-policy Redis may EVICT a "+
		"live gauge key under memory pressure; the counter then reads zero for a resource that is still "+
		"running, and the library silently ADMITS work it has already run out of room for (under-count → "+
		"phantom headroom → over-admission). Remedy: set `maxmemory-policy noeviction` on the Redis backing "+
		"storage.redis_url (a `volatile-*` policy is also safe — the counter keys carry no TTL), or, if this "+
		"Redis cannot report its CONFIG (some managed Redis disable it) and you KNOW it does not evict, put "+
		"that assertion on the record: storage.redis_durability_confirmed: true (env: %s=true). [detail: %s]",
		ErrCounterEviction, DurabilityConfirmEnv, detail)
}

// ScriptingError (D-73).
func ScriptingError(detail string) error {
	return fmt.Errorf("%w: every counter operation is an EVAL of a Lua script (the atomic "+
		"acquire/incr/decr family), and this server did not accept a SCRIPT LOAD of the real acquire source. "+
		"Some managed Redis disable or rename the SCRIPT/EVAL commands. Remedy: use a Redis with scripting "+
		"enabled. Refusing here is deliberate: the alternative is a service that boots green and fails at its "+
		"first admission decision. [detail: %s]", ErrScriptingUnsupported, detail)
}

// VersionError (D-74).
func VersionError(detail string) error {
	return fmt.Errorf("%w: ab0t-quota requires Redis >= %d.%d.%d. Remedy: upgrade the Redis backing "+
		"storage.redis_url. [detail: %s]", ErrRedisVersion, VersionFloor[0], VersionFloor[1], VersionFloor[2], detail)
}

func less(a, b [3]int) bool {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func nz(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}

// ---------------------------------------------------------------------------
// D-77 — memory headroom (the cliff we never surfaced)
// ---------------------------------------------------------------------------

// MemoryWarnRatio is the fraction of maxmemory at which we start degrading.
// `noeviction` fails CLOSED when Redis runs out (writes OOM → acquire raises → admission
// denies) — the SAFE direction, but the service DIES. "Dies at 3am with no warning" is not
// zero-caveats either.
const MemoryWarnRatio = 0.90

// EvaluateMemoryHeadroom returns (status, detail): "ok" | "low_headroom" | "unbounded" | "unknown".
func EvaluateMemoryHeadroom(maxmemory, used int64, found bool) (string, string) {
	if !found {
		return "unknown", "Redis INFO memory unavailable — headroom cannot be computed"
	}
	if maxmemory == 0 {
		return "unbounded", "maxmemory=0 (no eviction/OOM cliff configured)"
	}
	ratio := float64(used) / float64(maxmemory)
	pct := int(ratio*100 + 0.5)
	if ratio >= MemoryWarnRatio {
		return "low_headroom", fmt.Sprintf("%d%% of maxmemory used (%d/%d) — with a non-evicting "+
			"policy Redis will start REFUSING WRITES at the cliff, and the counter's admission path "+
			"fails closed (safe, but the service is down). Raise maxmemory or reduce load.",
			pct, used, maxmemory)
	}
	return "ok", fmt.Sprintf("%d%% of maxmemory used (%d/%d)", pct, used, maxmemory)
}

// CheckMemoryHeadroom asks Redis for INFO memory (D-77).
func CheckMemoryHeadroom(ctx context.Context, c Prober) (string, string) {
	raw, err := c.Info(ctx, "memory").Result()
	if err != nil {
		return EvaluateMemoryHeadroom(0, 0, false)
	}
	var maxmemory, used int64
	var okMax, okUsed bool
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "maxmemory:"); ok {
			if n, e := strconv.ParseInt(strings.TrimSpace(v), 10, 64); e == nil {
				maxmemory, okMax = n, true
			}
		}
		if v, ok := strings.CutPrefix(line, "used_memory:"); ok {
			if n, e := strconv.ParseInt(strings.TrimSpace(v), 10, 64); e == nil {
				used, okUsed = n, true
			}
		}
	}
	return EvaluateMemoryHeadroom(maxmemory, used, okMax && okUsed)
}

// MemoryHeadroomOK is the D-77 health predicate: degrade only on a READ low-headroom. An
// unreadable memory statistic is not a hazard the way an unreadable eviction policy is (the
// D-74 deviation, ratified, applied here).
func MemoryHeadroomOK(v string) bool {
	return !strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "low_headroom")
}

// ---------------------------------------------------------------------------
// D-80 — the EFFECT, not the policy. "Did it already happen?"
// ---------------------------------------------------------------------------
//
// Every guard we own asks the server what its configuration IS. None asked what it DID.
// A Redis whose maxmemory-policy was corrected to `noeviction` at 09:00 — AFTER it evicted a
// live gauge at 03:00 — passes EVERY check in this package, while the damage sits in the
// counter: an evicted gauge reads LOW, so the counter under-counts a resource that still
// exists → phantom headroom → over-admission (D-31's forbidden direction).
//
// `INFO stats.evicted_keys` is the FACT. The policy is only the forecast.
//
// The generalisation: for every assumption we check, ask "is there an observable FACT proving
// it was ALREADY violated?" — and check that too.

// EvaluateEvictionFacts returns (status, detail): "ok" | "evictions_observed" | "unknown".
// ANY eviction is a money incident: we cannot know the evicted key was not a live gauge, and
// an evicted gauge reads LOW. The counter is no longer trustworthy — it must be reconciled.
func EvaluateEvictionFacts(evicted int64, found bool) (string, string) {
	if !found {
		return "unknown", "Redis INFO stats unavailable — cannot tell whether this server has ALREADY evicted keys"
	}
	if evicted > 0 {
		return "evictions_observed", fmt.Sprintf("this Redis has EVICTED %d key(s) "+
			"(INFO stats.evicted_keys). Any one of them may have been a live gauge: an evicted gauge "+
			"reads LOW, so the counter now UNDER-counts resources that still exist (phantom headroom → "+
			"over-admission). The eviction policy may since have been corrected — the damage is already "+
			"in the counter. The counter must be reconciled, and the Redis must never evict again "+
			"(maxmemory-policy noeviction).", evicted)
	}
	return "ok", "0 (no keys evicted on this server)"
}

// CheckEvictedKeys reads INFO stats.evicted_keys — the fact a policy check cannot see (D-80).
func CheckEvictedKeys(ctx context.Context, c Prober) (int64, bool) {
	raw, err := c.Info(ctx, "stats").Result()
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "evicted_keys:"); ok {
			if n, e := strconv.ParseInt(strings.TrimSpace(v), 10, 64); e == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// EvictionFactsOK is the D-80 health predicate: degrade on an OBSERVED eviction. `unknown`
// does not degrade (an unreadable statistic is not itself a hazard — the ratified D-74
// deviation); the POLICY check already fails closed on an unverifiable server.
func EvictionFactsOK(v string) bool {
	return !strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "evictions_observed")
}

// ---------------------------------------------------------------------------
// D-81 — persistence is CONFIGURED. Is it WORKING?
// ---------------------------------------------------------------------------
//
// D-32 asked Redis `appendonly`. It never asked whether the writes were SUCCEEDING. A full
// disk, a permissions error, a failing volume → AOF configured, AOF failing, the config check
// GREEN, and the OUTBOX silently losing money events.
//
// WORSE than D-80's eviction: the counter only needs non-eviction and can HEAL (the reconciler
// converges it to Σ open activations). The OUTBOX REQUIRES persistence (D-30/D-32) — a lost
// outbox row is money nobody can reconstruct.
//
// Observed on a real server (not a stubbed status string): with `appendfsync always` Redis
// EXITS on an AOF write error (loud); with the DEFAULT `everysec` it STAYS UP reporting
// `aof_last_write_status:err` — the quiet case, and the one that costs money.

// PersistFacts holds the INFO persistence fields that say whether persistence WORKS.
type PersistFacts struct {
	AOFEnabled      string
	AOFWriteStatus  string
	AOFRewriteState string
	RDBBgsaveStatus string
	Found           bool
}

// CheckPersistFacts reads INFO persistence — the facts a config check cannot see (D-81).
func CheckPersistFacts(ctx context.Context, c Prober) PersistFacts {
	raw, err := c.Info(ctx, "persistence").Result()
	if err != nil {
		return PersistFacts{}
	}
	f := PersistFacts{}
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		v = strings.ToLower(strings.TrimSpace(v))
		switch k {
		case "aof_enabled":
			f.AOFEnabled, f.Found = v, true
		case "aof_last_write_status":
			f.AOFWriteStatus, f.Found = v, true
		case "aof_last_bgrewrite_status":
			f.AOFRewriteState, f.Found = v, true
		case "rdb_last_bgsave_status":
			f.RDBBgsaveStatus, f.Found = v, true
		}
	}
	return f
}

// EvaluatePersistFacts returns (status, detail): "ok" | "persist_failing" | "unknown".
func EvaluatePersistFacts(f PersistFacts) (string, string) {
	if !f.Found {
		return "unknown", "Redis INFO persistence unavailable — cannot tell whether this server's persistence is actually WORKING"
	}
	var failing []string
	for _, kv := range [][2]string{
		{"aof_last_write_status", f.AOFWriteStatus},
		{"rdb_last_bgsave_status", f.RDBBgsaveStatus},
		{"aof_last_bgrewrite_status", f.AOFRewriteState},
	} {
		if kv[1] != "" && kv[1] != "ok" {
			failing = append(failing, kv[0]+"="+kv[1])
		}
	}
	if len(failing) > 0 {
		sort.Strings(failing)
		return "persist_failing", fmt.Sprintf("Redis reports FAILED persistence: %s. The configuration "+
			"says it persists (appendonly=%s); the SERVER says the writes are not landing — a full disk, a "+
			"permissions error, a failing volume. The outbox holds MONEY events that nobody can reconstruct: "+
			"a pending settlement written here is lost on the next restart/failover. Free the disk / fix the "+
			"volume, then verify aof_last_write_status=ok.", strings.Join(failing, ", "), nz(f.AOFEnabled))
	}
	return "ok", fmt.Sprintf("persistence verified working (aof_last_write_status=%s, rdb_last_bgsave_status=%s)",
		nz(f.AOFWriteStatus), nz(f.RDBBgsaveStatus))
}

// PersistFactsOK is the D-81 health predicate: degrade on an OBSERVED persist failure.
// `unknown` does not degrade — the CONFIG check (D-32) already fails closed on a server it
// cannot interrogate.
func PersistFactsOK(v string) bool {
	return !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(v)), "FAILING")
}
