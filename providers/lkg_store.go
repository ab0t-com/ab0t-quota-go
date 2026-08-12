package providers

// Durable last-known-good (LKG) tier store — Go/Python parity for ticket
// 20260810_tier_failclosed_lastknowngood (follow-up recorded in
// PROGRAM_FOLLOWUPS_20260810 "Go last-known-good durability" and
// verify_fabel_5_report_20260812 §7/H8).
//
// WHY: Python's AuthServiceTierProvider persists its LKG record in Redis
// (`quota:tier:lkg:{org}`), so the never-downgrade guarantee survives process
// restarts and is shared across replicas. Go's Cache kept LKG only in a
// per-process map — after a restart that coincided with a billing outage, a
// paying org resolved the fallback default until the first successful fetch.
// This file closes that parity gap with an ADDITIVE seam: an optional durable
// LKGStore consulted when the in-memory record is missing, and written through
// on every successful fetch. Unwired => byte-identical prior behavior.
//
// CROSS-RUNTIME COMPATIBLE BY DESIGN: RedisLKGStore uses the SAME key
// (`quota:tier:lkg:{org}`) and the SAME JSON payload ({"tier": str, "ts":
// float-epoch-seconds}) as the Python provider, so a mixed Python/Go fleet
// shares one LKG record per org — a fetch that succeeds in either runtime
// protects both.

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ab0t-com/ab0t-quota-go/counters"
)

// LKGStore persists last-known-good tiers durably. Implementations MUST be
// fail-safe: a store error is reported via the ok/err contract and must never
// panic — the caller treats any failure as "no durable record" (the in-memory
// map and the fallback-default path still apply).
type LKGStore interface {
	// GetLKG returns the stored tier and its fetch timestamp for an org.
	// ok=false when there is no usable record (missing, malformed, or error).
	GetLKG(ctx context.Context, orgID string) (tier string, ts time.Time, ok bool)
	// SetLKG persists the successfully-fetched tier. Best-effort; errors are
	// the implementation's to log.
	SetLKG(ctx context.Context, orgID, tier string, ts time.Time)
}

// RedisLKGStore is the standard durable store, wire-compatible with the
// Python provider's Redis record. A nil client behaves as an empty store.
type RedisLKGStore struct {
	rdb redis.Cmdable
}

// NewRedisLKGStore wraps any redis.Cmdable (*redis.Client, cluster, ...).
func NewRedisLKGStore(rdb redis.Cmdable) *RedisLKGStore {
	return &RedisLKGStore{rdb: rdb}
}

type lkgPayload struct {
	Tier string  `json:"tier"`
	Ts   float64 `json:"ts"`
}

// GetLKG reads `quota:tier:lkg:{org}`. Never raises into the hot path — any
// failure (unreachable Redis, malformed JSON, empty tier) is ok=false, exactly
// mirroring Python's fail-safe _read_last_known_good.
func (s *RedisLKGStore) GetLKG(ctx context.Context, orgID string) (string, time.Time, bool) {
	if s == nil || s.rdb == nil || orgID == "" {
		return "", time.Time{}, false
	}
	raw, err := s.rdb.Get(ctx, counters.TierLKGKey(orgID)).Result()
	if err != nil {
		if err != redis.Nil {
			slog.Warn("last_known_good_read_failed", "org", orgID, "error", err)
		}
		return "", time.Time{}, false
	}
	var p lkgPayload
	if jsonErr := json.Unmarshal([]byte(raw), &p); jsonErr != nil || p.Tier == "" {
		slog.Warn("last_known_good_record_malformed", "org", orgID)
		return "", time.Time{}, false
	}
	sec := int64(p.Ts)
	nsec := int64((p.Ts - float64(sec)) * 1e9)
	return p.Tier, time.Unix(sec, nsec), true
}

// SetLKG writes the Python-compatible JSON record with no expiry (the
// strongest never-downgrade guarantee — a real downgrade always arrives as a
// successful fetch that overwrites it, in either runtime). Best-effort.
func (s *RedisLKGStore) SetLKG(ctx context.Context, orgID, tier string, ts time.Time) {
	if s == nil || s.rdb == nil || orgID == "" || tier == "" {
		return
	}
	payload, err := json.Marshal(lkgPayload{
		Tier: tier,
		Ts:   float64(ts.UnixNano()) / 1e9,
	})
	if err != nil {
		return
	}
	if err := s.rdb.Set(ctx, counters.TierLKGKey(orgID), string(payload), 0).Err(); err != nil {
		slog.Warn("last_known_good_write_failed", "org", orgID, "error", err)
	}
}
