package outbox

// D-32 — turn OPERATOR CHECK 2 into a machine check. The outbox stores money events; if
// Redis can evict them (allkeys-*) or isn't persisted, the outbox is not durable — it is
// lucky. Ask Redis at startup rather than trusting a human to have configured it.
//
// D-72 GENERALISED this judgement: the ONE implementation now lives in package redisguard
// (which also checks the COUNTER's eviction policy — the same law, different severity: the
// counter tolerates a restart but never an eviction). These functions are NAMES, not a
// second copy — a second copy of the same judgement is D-35's mistake in a new costume.

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/ab0t-com/ab0t-quota-go/redisguard"
)

// CheckRedisDurability asks Redis its persistence + eviction policy.
//   - maxmemory-policy allkeys-* → NOT durable (pending money events evictable). Hard,
//     and NOT overridable by `confirmed`.
//   - no persistence (appendonly=no AND no save points) → NOT durable (a restart loses
//     pending events). Hard unless `confirmed`.
//   - CONFIG unavailable (e.g. ElastiCache disables it) → require the explicit operator
//     assertion `confirmed` (redis_durability_confirmed=true).
//
// Returns (durable, human_reason).
func CheckRedisDurability(ctx context.Context, c redis.Cmdable, confirmed bool) (bool, string) {
	return redisguard.CheckDurability(ctx, c, confirmed)
}

// EvaluateDurability is the pure durability decision (D-32), separated from the Redis
// plumbing so it is directly testable. configUnavailable models a CONFIG call that errored
// (e.g. ElastiCache disables CONFIG).
func EvaluateDurability(policy, appendonly, save string, configUnavailable, confirmed bool) (bool, string) {
	return redisguard.EvaluateDurability(policy, appendonly, save, configUnavailable, confirmed)
}
