package quota

// D-81 (Go lane) — a configured guarantee is not a working one.
//
// D-32 asked Redis `appendonly`. It never asked whether the writes were SUCCEEDING. A full
// disk, a permissions error, a failing volume → AOF configured, AOF failing, the config check
// GREEN, and the OUTBOX losing money events.
//
// WORSE than D-80's eviction: the counter only needs non-eviction and can HEAL (reconciler →
// Σ open activations). The OUTBOX REQUIRES persistence — a lost outbox row is money nobody
// can reconstruct.
//
// The real leg runs against a redis:7 whose AOF directory is a 4 MiB tmpfs, filled until the
// writes genuinely FAIL (not a stubbed status string). Two real behaviours, both worth
// knowing: with `appendfsync always` Redis EXITS on the write error (loud); with the DEFAULT
// `everysec` it STAYS UP reporting aof_last_write_status:err — the quiet case that costs money.

import (
	"context"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/ab0t-com/ab0t-quota-go/redisguard"
)

// persistProbe drives the REAL checks with canned INFO persistence answers.
type persistProbe struct {
	mutableProbe
	aofWrite, rdbBgsave, aofRewrite string
}

func (p *persistProbe) Info(ctx context.Context, section ...string) *redis.StringCmd {
	if len(section) > 0 && section[0] == "persistence" {
		cmd := redis.NewStringCmd(ctx)
		cmd.SetVal("# Persistence\r\naof_enabled:1\r\naof_last_write_status:" + p.aofWrite +
			"\r\naof_last_bgrewrite_status:" + p.aofRewrite +
			"\r\nrdb_last_bgsave_status:" + p.rdbBgsave + "\r\n")
		return cmd
	}
	return p.mutableProbe.Info(ctx, section...)
}

func newPersistProbe(aofWrite string) *persistProbe {
	return &persistProbe{
		mutableProbe: mutableProbe{policy: "noeviction"},
		aofWrite:     aofWrite, rdbBgsave: "ok", aofRewrite: "ok",
	}
}

func TestEvaluatePersistFacts_D81(t *testing.T) {
	for _, tc := range []struct {
		name  string
		facts redisguard.PersistFacts
		want  string
	}{
		{"working persistence is ok", redisguard.PersistFacts{
			Found: true, AOFEnabled: "1", AOFWriteStatus: "ok", RDBBgsaveStatus: "ok"}, "ok"},
		{"a FAILING aof write is a money incident", redisguard.PersistFacts{
			Found: true, AOFEnabled: "1", AOFWriteStatus: "err", RDBBgsaveStatus: "ok"}, "persist_failing"},
		{"a FAILING bgsave is a money incident", redisguard.PersistFacts{
			Found: true, AOFEnabled: "0", AOFWriteStatus: "ok", RDBBgsaveStatus: "err"}, "persist_failing"},
		{"a FAILING rewrite is reported too", redisguard.PersistFacts{
			Found: true, AOFEnabled: "1", AOFWriteStatus: "ok", RDBBgsaveStatus: "ok", AOFRewriteState: "err"}, "persist_failing"},
		{"unreadable is unknown, never assumed ok", redisguard.PersistFacts{}, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := redisguard.EvaluatePersistFacts(tc.facts)
			if got != tc.want {
				t.Fatalf("status = %q (want %q) — %s", got, tc.want, detail)
			}
		})
	}
	if redisguard.PersistFactsOK("FAILING (aof_last_write_status=err)") {
		t.Error("an observed persist failure must degrade the probe")
	}
	if !redisguard.PersistFactsOK("unknown") {
		t.Error("unknown must not degrade — the CONFIG check already fails closed there")
	}
}

// The ONE durability implementation (D-35) now asks BOTH: is it configured, and is it working?
func TestDurability_UsesTheFact_D81(t *testing.T) {
	ctx := context.Background()
	if durable, reason := redisguard.CheckDurability(ctx, newPersistProbe("err"), false); durable {
		t.Fatalf("a Redis whose AOF writes are FAILING is not durable, however green the config reads: %s", reason)
	}
	// [control] a working AOF is durable — the guard is not a blanket reject.
	if durable, reason := redisguard.CheckDurability(ctx, newPersistProbe("ok"), false); !durable {
		t.Fatalf("a working Redis must be durable: %s", reason)
	}
}

// Severity BY CONSEQUENCE, not uniformity (the D-76 lesson).
func TestPersistFailure_SeverityDependsOnWhereTheOutboxLives_D81(t *testing.T) {
	ctx := context.Background()

	// The outbox lives HERE ⇒ money nobody can reconstruct ⇒ degrade + alert.
	caps, unsafe := verifyRedisInvariants(ctx, newPersistProbe("err"), minimalConfig(), true)
	found := false
	for _, u := range unsafe {
		if u[0] == "redis_persist_status" {
			found = true
		}
	}
	if !found {
		t.Fatal("a failing AOF on the Redis holding the OUTBOX is a money incident (D-81)")
	}
	if !strings.HasPrefix(caps["redis_persist_status"], "FAILING") {
		t.Errorf("capability must say FAILING, got %q", caps["redis_persist_status"])
	}

	// The outbox is on DDB ⇒ the counter HEALS (reconciler → Σ open activations) ⇒ recorded and
	// logged, but NOT reported as money loss. Over-refusing trains operators to ignore the probe.
	caps2, unsafe2 := verifyRedisInvariants(ctx, newPersistProbe("err"), minimalConfig(), false)
	for _, u := range unsafe2 {
		if u[0] == "redis_persist_status" {
			t.Fatal("the counter can heal — this must not be reported as money loss (D-49)")
		}
	}
	if !redisguard.PersistFactsOK(caps2["redis_persist_status"]) {
		t.Error("the health predicate must not degrade when the outbox is elsewhere")
	}
	if !strings.Contains(strings.ToLower(caps2["redis_persist_status"]), "outbox") {
		t.Errorf("the capability must explain WHY it is not a money incident: %q", caps2["redis_persist_status"])
	}
}

func TestHealthy_FailsOnAFailingPersist_D81(t *testing.T) {
	q := &Quota{capability: Capabilities{
		BillingStatus: "OFF (paid disabled)", Reconciler: "OFF — not requested",
		FloatStore: "redis", RedisTopology: TopologySingleNode,
		CounterEvictionPolicy: "noeviction", RedisScripting: "on (EVAL verified)",
		CounterEvictionsObserved: "0 (no keys evicted on this server)",
		PreflightReverification:  "on (rides the reconciler loop)",
		RedisPersistStatus:       "FAILING (aof_last_write_status=err)",
	}}
	if ok, reasons := q.Healthy(); ok {
		t.Fatalf("a Redis that is FAILING to persist must degrade the probe: %v", reasons)
	}
}

// ---------------------------------------------------------------------------
// REAL Redis with a REAL failing AOF (operator-gated) — not a stubbed status
// ---------------------------------------------------------------------------

func TestRealFailingAOF_IsCaught_D81(t *testing.T) {
	addr := envOr("AB0T_QUOTA_TEST_AOF_FAIL_ADDR")
	if addr == "" {
		t.Skip("AB0T_QUOTA_TEST_AOF_FAIL_ADDR not set — the real failing-AOF leg is operator-gated")
	}
	ctx := context.Background()
	c := redis.NewClient(&redis.Options{Addr: addr})
	defer c.Close()

	// Sticky-signal discipline (my own D-80 lesson): this leg does NOT reset the server — the
	// failure IS the fixture, and it is a throwaway container.
	facts := redisguard.CheckPersistFacts(ctx, c)
	if facts.AOFWriteStatus != "err" {
		t.Fatalf("the fixture failed to make the AOF genuinely fail: %+v", facts)
	}

	cfgv, err := c.ConfigGet(ctx, "appendonly").Result()
	if err != nil || cfgv["appendonly"] != "yes" {
		t.Fatalf("the CONFIG must still read appendonly=yes (that is the point): %v %v", cfgv, err)
	}

	durable, reason := redisguard.CheckDurability(ctx, c, false)
	if durable {
		t.Fatal("D-32's config-only check called this Redis DURABLE while its AOF writes were " +
			"failing on a full disk — that is exactly the defect D-81 closes")
	}
	if !strings.Contains(reason, "err") {
		t.Errorf("the reason must name the failing status: %s", reason)
	}
}
