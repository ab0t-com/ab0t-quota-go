// Package outbox is the Go port of Python's durable lifecycle-settlement
// outbox (ab0t_quota/billing/outbox.py). Ticket 20260709 (D-44, D-29, D-30,
// D-32, D-12). It closes QB-01 in Go: a money-bearing lifecycle event is
// written to a DURABLE store BEFORE publish and drained FROM THE STORE — so a
// crash/pod-restart between "intent written" and "delivered" RESUMES delivery
// instead of silently losing the billing event.
//
// The distinguishing property (D-29): discard the emitter AND the in-process
// store object, and a fresh process resumes delivery from the external store.
// An in-process queue that evaporates on the crash it exists to survive is a
// retry loop wearing the word "durable".
//
// ⚠️ Real-Redis/real-AWS UNVERIFIED here: Redis stores + CONFIG durability run
// against miniredis; DDB runs against DynamoDB Local — never a live
// Redis/DynamoDB. Pre-deploy gate (mirrors the lane's standing caveat).
package outbox

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Status values.
const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusVoided    = "voided"
)

// Record is one durable outbox intent. FirstTS is EPOCH seconds (survives a
// restart — never monotonic), the horizon anchor (D-12).
type Record struct {
	Key           string          `json:"key"` // reservation_id:event_type (activation_id:event_type after re-key)
	Event         json.RawMessage `json:"event"`
	EventType     string          `json:"event_type"`
	ResourceType  string          `json:"resource_type"`
	ReservationID string          `json:"reservation_id"`
	Status        string          `json:"status"`
	FirstTS       float64         `json:"first_ts"`
	Attempts      int             `json:"attempts"`
	Reason        string          `json:"reason,omitempty"`
}

// Store is the durable-intent persistence contract. PutIntent is create-only
// on the key (preserves FirstTS across a re-emit). ListPending reads from the
// store, never from memory.
type Store interface {
	PutIntent(ctx context.Context, r Record) (Record, error)
	MarkDelivered(ctx context.Context, key string) error
	MarkVoided(ctx context.Context, key, reason string) error
	BumpAttempt(ctx context.Context, key string) error
	ListPending(ctx context.Context, limit int) ([]Record, error)
	// Durable reports whether this store survives a process crash. An
	// in-memory store returns false; Redis/DDB return true. The gate (D-34)
	// refuses paid billing onto a non-durable store.
	Durable() bool
}

// ---- InMemoryStore (tests + explicit degraded mode; NOT crash-durable) ----

type InMemoryStore struct {
	mu   sync.Mutex
	rows map[string]*Record
}

func NewInMemoryStore() *InMemoryStore { return &InMemoryStore{rows: map[string]*Record{}} }

func (s *InMemoryStore) PutIntent(_ context.Context, r Record) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ex, ok := s.rows[r.Key]; ok && ex.Status == StatusPending {
		return *ex, nil // preserve first_ts / attempts across re-emit
	}
	if r.Status == "" {
		r.Status = StatusPending
	}
	cp := r
	s.rows[r.Key] = &cp
	return cp, nil
}
func (s *InMemoryStore) MarkDelivered(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, key)
	return nil
}
func (s *InMemoryStore) MarkVoided(_ context.Context, key, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rows[key]; ok {
		r.Status = StatusVoided
		r.Reason = reason
	}
	return nil
}
func (s *InMemoryStore) BumpAttempt(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rows[key]; ok {
		r.Attempts++
	}
	return nil
}
func (s *InMemoryStore) ListPending(_ context.Context, limit int) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for _, r := range s.rows {
		if r.Status == StatusPending {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FirstTS < out[j].FirstTS })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (s *InMemoryStore) Durable() bool { return false }

// ---- RedisStore (durable across restarts — Redis is external) ----

type RedisStore struct {
	c      redis.Cmdable
	prefix string
}

func NewRedisStore(c redis.Cmdable, prefix string) *RedisStore {
	if prefix == "" {
		prefix = "outbox"
	}
	return &RedisStore{c: c, prefix: prefix}
}

func (s *RedisStore) intentKey(key string) string { return s.prefix + ":intent:" + key }
func (s *RedisStore) pendingIdx() string          { return s.prefix + ":pending" }

func (s *RedisStore) PutIntent(ctx context.Context, r Record) (Record, error) {
	if r.Status == "" {
		r.Status = StatusPending
	}
	blob, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	// create-only: preserves first_ts (horizon anchor) across a re-emit.
	created, err := s.c.SetNX(ctx, s.intentKey(r.Key), blob, 0).Result()
	if err != nil {
		return r, err
	}
	if created {
		if err := s.c.ZAdd(ctx, s.pendingIdx(), redis.Z{Score: r.FirstTS, Member: r.Key}).Err(); err != nil {
			return r, err
		}
		return r, nil
	}
	raw, err := s.c.Get(ctx, s.intentKey(r.Key)).Result()
	if err != nil {
		return r, nil
	}
	var ex Record
	if json.Unmarshal([]byte(raw), &ex) == nil {
		return ex, nil
	}
	return r, nil
}
func (s *RedisStore) MarkDelivered(ctx context.Context, key string) error {
	pipe := s.c.TxPipeline()
	pipe.Del(ctx, s.intentKey(key))
	pipe.ZRem(ctx, s.pendingIdx(), key)
	_, err := pipe.Exec(ctx)
	return err
}
func (s *RedisStore) MarkVoided(ctx context.Context, key, reason string) error {
	raw, err := s.c.Get(ctx, s.intentKey(key)).Result()
	if err == nil {
		var r Record
		if json.Unmarshal([]byte(raw), &r) == nil {
			r.Status = StatusVoided
			r.Reason = reason
			if b, e := json.Marshal(r); e == nil {
				_ = s.c.Set(ctx, s.intentKey(key), b, 0).Err()
			}
		}
	}
	return s.c.ZRem(ctx, s.pendingIdx(), key).Err()
}
func (s *RedisStore) BumpAttempt(ctx context.Context, key string) error {
	raw, err := s.c.Get(ctx, s.intentKey(key)).Result()
	if err != nil {
		return nil
	}
	var r Record
	if json.Unmarshal([]byte(raw), &r) != nil {
		return nil
	}
	r.Attempts++
	b, _ := json.Marshal(r)
	return s.c.Set(ctx, s.intentKey(key), b, 0).Err()
}
func (s *RedisStore) ListPending(ctx context.Context, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = 100
	}
	keys, err := s.c.ZRange(ctx, s.pendingIdx(), 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, k := range keys {
		raw, err := s.c.Get(ctx, s.intentKey(k)).Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			return nil, err
		}
		var r Record
		if json.Unmarshal([]byte(raw), &r) == nil && r.Status == StatusPending {
			out = append(out, r)
		}
	}
	return out, nil
}
func (s *RedisStore) Durable() bool { return true }

// AutoSelect resolves DDB > Redis > nil. Returns nil when NO durable backend
// is available — the caller must fail loudly (D-29/D-34), never fall back to
// RAM. (DDB is wired by the caller via NewDDBStore; kept separate so consumers
// that don't use DDB don't pay for the aws-sdk here.)
func AutoSelect(redisClient redis.Cmdable, ddb Store) Store {
	if ddb != nil {
		return ddb
	}
	if redisClient != nil {
		return NewRedisStore(redisClient, "outbox")
	}
	return nil
}

// nowEpoch is the horizon clock — epoch seconds, survives restart.
func nowEpoch() float64 { return float64(time.Now().UnixNano()) / 1e9 }
