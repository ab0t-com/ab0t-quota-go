package counters

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// randToken returns a short random hex string to make rate-ZSET members
// unique per Record call (avoids same-instant member collisions in a shared
// sorted set — see Record / QG-06).
func randToken() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback: nanosecond clock is unique enough for the degenerate case.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// TASK P5.1 (findings QG-01/QG-02) — real go-redis backends for the counter
// primitives. Ticket 20260709_ab0t_quota_systemic_integrity_redesign.
//
// Before P5.1 these constructors were stubs returning ErrRedisNotAvailable
// and every counter was process-local (QG-01): restart zeroed usage, N
// replicas each enforced their own limit. They now build a durable store
// backed by the passed *redis.Client. The constructor still returns a typed
// error (not a silent memory swap) when no client is supplied, so callers
// degrade LOUDLY — see quota.Setup + handlerledger.AutoSelectStore honesty
// (QG-02).
//
// NOTE (QG-03/P5.3): the KEY SHAPES these stores read/write are decided by
// the counter builders (gauge.go / accumulator.go / rate.go), not here — a
// FloatStore is key-agnostic. Cross-runtime key parity is P5.3's concern.

// ErrRedisNotAvailable is returned when a Redis backend is requested but no
// client was provided. Retained for callers that fall back to InMemoryStore.
var ErrRedisNotAvailable = errors.New("redis backend requested but no *redis.Client supplied; falling back to in-memory store")

// redisFloatStore is a durable FloatStore backed by go-redis.
type redisFloatStore struct {
	c redis.Cmdable
}

// NewRedisStore returns a FloatStore backed by go-redis. Pass a
// *redis.Client (or any redis.Cmdable). A nil client yields
// ErrRedisNotAvailable so the caller can degrade explicitly.
func NewRedisStore(client redis.Cmdable) (FloatStore, error) {
	if client == nil {
		return nil, ErrRedisNotAvailable
	}
	return &redisFloatStore{c: client}, nil
}

func (s *redisFloatStore) IncrByFloat(ctx context.Context, key string, delta float64) (float64, error) {
	return s.c.IncrByFloat(ctx, key, delta).Result()
}

// decrFloorScript: INCRBYFLOAT by a negative delta, then floor at zero — in
// ONE atomic server-side step so concurrent replicas can't drive the gauge
// negative (finding QG-06; avoids the Python read-then-SET race QI-02).
// decrFloorSrc — QI-09: the ONLY key touched (KEYS[1]) is declared in KEYS.
const decrFloorSrc = `
local v = redis.call('INCRBYFLOAT', KEYS[1], ARGV[1])
if tonumber(v) < 0 then
  redis.call('SET', KEYS[1], '0')
  return '0'
end
return v
`

var decrFloorScript = redis.NewScript(decrFloorSrc)

func (s *redisFloatStore) DecrByFloorZero(ctx context.Context, key string, amount float64) (float64, error) {
	neg := strconv.FormatFloat(-amount, 'f', -1, 64)
	res, err := decrFloorScript.Run(ctx, s.c, []string{key}, neg).Result()
	if err != nil {
		return 0, err
	}
	str, ok := res.(string)
	if !ok {
		return 0, fmt.Errorf("decrFloor: unexpected reply type %T", res)
	}
	return strconv.ParseFloat(str, 64)
}

func (s *redisFloatStore) GetFloat(ctx context.Context, key string) (float64, bool, error) {
	v, err := s.c.Get(ctx, key).Result()
	if err == redis.Nil {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false, err
	}
	return f, true, nil
}

func (s *redisFloatStore) Set(ctx context.Context, key string, value float64, ttl time.Duration) error {
	// Store as a plain decimal string so INCRBYFLOAT can operate on it
	// (mirrors Python, which stores float strings via SET/INCRBYFLOAT).
	return s.c.Set(ctx, key, strconv.FormatFloat(value, 'f', -1, 64), ttl).Err()
}

func (s *redisFloatStore) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return s.c.Del(ctx, keys...).Err()
}

func (s *redisFloatStore) Expire(ctx context.Context, key string, ttl time.Duration) error {
	if ttl <= 0 {
		return s.c.Persist(ctx, key).Err()
	}
	return s.c.Expire(ctx, key, ttl).Err()
}

func (s *redisFloatStore) SetIfAbsent(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	if ttl < 0 {
		ttl = 0
	}
	return s.c.SetNX(ctx, key, value, ttl).Result()
}

// redisRateStore is a durable RateStore backed by a go-redis sorted set.
type redisRateStore struct {
	c redis.Cmdable
}

// NewRedisRateStore returns a RateStore backed by go-redis. A nil client
// yields ErrRedisNotAvailable so the caller can degrade explicitly.
func NewRedisRateStore(client redis.Cmdable) (RateStore, error) {
	if client == nil {
		return nil, ErrRedisNotAvailable
	}
	return &redisRateStore{c: client}, nil
}

// epochSeconds returns time as epoch SECONDS with sub-second precision —
// exactly Python's time.time() (finding QG-06). Scores and cutoffs are in
// these units so a Python/Go fleet can share the rate ZSET.
func epochSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// cutoffStr is the exclusive lower bound (now-window) as a seconds string
// for ZREMRANGEBYSCORE.
func (s *redisRateStore) cutoffStr(now time.Time, window time.Duration) string {
	return "(" + strconv.FormatFloat(epochSeconds(now.Add(-window)), 'f', -1, 64)
}

// Record adds one timestamped member and trims the window.
//
// QG-06 parity: score = epoch SECONDS (Python's time.time()). The member is
// made unique per call (Python appends id(self):i) so two events in the
// same instant don't collide into one ZSET member and undercount.
func (s *redisRateStore) Record(ctx context.Context, key string, now time.Time, window time.Duration, member string) error {
	if member == "" {
		member = strconv.FormatInt(now.UnixNano(), 10)
	}
	member = member + ":" + randToken()
	pipe := s.c.TxPipeline()
	pipe.ZAdd(ctx, key, redis.Z{Score: epochSeconds(now), Member: member})
	pipe.ZRemRangeByScore(ctx, key, "-inf", s.cutoffStr(now, window))
	pipe.Expire(ctx, key, window+60*time.Second)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *redisRateStore) Count(ctx context.Context, key string, now time.Time, window time.Duration) (int64, error) {
	if err := s.Trim(ctx, key, now, window); err != nil {
		return 0, err
	}
	return s.c.ZCard(ctx, key).Result()
}

func (s *redisRateStore) Trim(ctx context.Context, key string, now time.Time, window time.Duration) error {
	return s.c.ZRemRangeByScore(ctx, key, "-inf", s.cutoffStr(now, window)).Err()
}
