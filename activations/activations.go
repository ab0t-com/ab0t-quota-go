// Package activations is the Go port of Python's activation core
// (ab0t_quota/activations.py). Ticket 20260709_ab0t_quota_systemic_integrity_
// redesign, TASK P5.2.
//
// An activation is minted (OPEN) atomically with the counter spend, closed
// (RELEASED) atomically with the counter release, and SETTLED when its cost
// is recorded. The activation_id is library-minted and random (no org hot
// partition), and it is the ONLY key release/settle dedup on — no TTL
// horizon, no caller-composed key. That is what makes a duplicate release
// idempotent FOREVER (D-22: the mixed-fleet safety condition).
//
// PARITY: the row/index key shapes and the JSON record format match Python
// byte-for-byte so a Python-minted activation is releasable by Go against a
// shared Redis and vice versa (mixed-fleet). Reference:
//
//	row key   activation:row:{id}
//	open index activation:open:org:{org}
//	record    {"activation_id","org_id","user_id","resource_key","cost",
//	           "opened_at","state","spend","released_at","settled_at"}
//
// ⚠️ Real-Redis Lua UNVERIFIED here: the _TRANSITION script and all store
// ops are exercised only against miniredis/gopher-lua in tests, never a real
// Redis EVAL. Pre-deploy gate (mirrors the Python lane's standing caveat).
package activations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultReleasedTTL is the TTL for RELEASED/SETTLED rows — chosen to
// outlive the longest legitimate create→terminate gap, NOT anchored to
// QI-05's old 24h idempotency horizon. OPEN rows NEVER expire (they are the
// drift alarm). Matches Python DEFAULT_RELEASED_TTL_S (14 days).
const DefaultReleasedTTL = 14 * 24 * time.Hour

// State values match Python ActivationState.
const (
	StateOpen     = "open"
	StateReleased = "released"
	StateSettled  = "settled"
)

// MintActivationID returns a random, unguessable id: "act_" + 32 hex chars
// (matches Python mint_activation_id → secrets.token_hex(16)).
func MintActivationID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "act_" + hex.EncodeToString(b[:])
}

func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Activation is a GENERIC activation record — no client-domain fields.
// JSON tags match Python's asdict() output exactly (mixed-fleet parity).
type Activation struct {
	ActivationID string             `json:"activation_id"`
	OrgID        string             `json:"org_id"`
	UserID       *string            `json:"user_id"`
	ResourceKey  string             `json:"resource_key"`
	Cost         *string            `json:"cost"`
	OpenedAt     string             `json:"opened_at"`
	State        string             `json:"state"`
	Spend        map[string]float64 `json:"spend"`
	ReleasedAt   *string            `json:"released_at"`
	SettledAt    *string            `json:"settled_at"`
}

// Store is the persistence contract (implemented against the client's own
// datastore). MarkReleased/MarkSettled are idempotent and return the row ONLY
// if THIS call performed the transition — so the caller makes the counter
// release a no-op on replay.
type Store interface {
	PutOpen(ctx context.Context, a Activation) error
	Get(ctx context.Context, activationID string) (*Activation, error)
	MarkReleased(ctx context.Context, activationID string) (*Activation, error)
	MarkSettled(ctx context.Context, activationID, cost string) (*Activation, error)
	ListOpen(ctx context.Context, orgID string, limit int) ([]Activation, error)
	CountOpen(ctx context.Context, orgID string) (int, error)
}

// ---- InMemoryStore (default; loud-not-durable, mirrors the ledger fallback) ----

type InMemoryStore struct {
	mu   sync.Mutex
	rows map[string]*Activation
}

func NewInMemoryStore() *InMemoryStore { return &InMemoryStore{rows: map[string]*Activation{}} }

// Durable — the in-memory store is NOT crash/eviction durable (the reconciler
// refuses to run on it: reconciling from a per-process partial view
// under-counts a shared counter, D-37).
func (s *InMemoryStore) Durable() bool { return false }

func (s *InMemoryStore) PutOpen(_ context.Context, a Activation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[a.ActivationID]; !ok { // first writer wins (minted id is unique)
		cp := a
		s.rows[a.ActivationID] = &cp
	}
	return nil
}

func (s *InMemoryStore) Get(_ context.Context, id string) (*Activation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

func (s *InMemoryStore) MarkReleased(_ context.Context, id string) (*Activation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok || r.State != StateOpen {
		return nil, nil
	}
	r.State = StateReleased
	t := nowISO()
	r.ReleasedAt = &t
	cp := *r
	return &cp, nil
}

func (s *InMemoryStore) MarkSettled(_ context.Context, id, cost string) (*Activation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok || r.State == StateSettled {
		return nil, nil
	}
	r.State = StateSettled
	r.Cost = &cost
	t := nowISO()
	r.SettledAt = &t
	cp := *r
	return &cp, nil
}

func (s *InMemoryStore) ListOpen(_ context.Context, orgID string, limit int) ([]Activation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Activation
	for _, r := range s.rows {
		if r.OrgID == orgID && r.State == StateOpen {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenedAt < out[j].OpenedAt })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *InMemoryStore) CountOpen(_ context.Context, orgID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.rows {
		if r.OrgID == orgID && r.State == StateOpen {
			n++
		}
	}
	return n, nil
}

// ---- RedisStore (durable; mixed-fleet parity with Python) ----

// transitionScript is the atomic idempotent state transition (matches Python
// _TRANSITION). KEYS[1]=row. ARGV: from_state, to_state, released_ttl, extra_json.
// Returns the updated row json if THIS call transitioned it, else ” (already
// done). QI-09: the ONLY key accessed (KEYS[1]) is declared in KEYS.
// transitionSrc — QI-09: the ONLY key touched (KEYS[1]) is declared in KEYS.
const transitionSrc = `
local raw = redis.call('GET', KEYS[1])
if not raw then return '' end
local row = cjson.decode(raw)
if row['state'] ~= ARGV[1] then return '' end
row['state'] = ARGV[2]
local extra = cjson.decode(ARGV[4])
for k, v in pairs(extra) do row[k] = v end
local out = cjson.encode(row)
redis.call('SET', KEYS[1], out)
if tonumber(ARGV[3]) > 0 then redis.call('EXPIRE', KEYS[1], ARGV[3]) end
return out
`

var transitionScript = redis.NewScript(transitionSrc)

type RedisStore struct {
	c      redis.Cmdable
	prefix string
	ttl    time.Duration
}

// NewRedisStore builds a durable activation store. prefix defaults to
// "activation" (Python's default — parity). ttl<=0 uses DefaultReleasedTTL.
func NewRedisStore(c redis.Cmdable, prefix string, ttl time.Duration) *RedisStore {
	if prefix == "" {
		prefix = "activation"
	}
	if ttl <= 0 {
		ttl = DefaultReleasedTTL
	}
	return &RedisStore{c: c, prefix: prefix, ttl: ttl}
}

// Durable — a bare Redis store is a CACHE (may evict money-bearing rows,
// D-30/D-39); the reconciler treats it as NON-durable unless Setup's operator
// durability gate confirmed it.
func (s *RedisStore) Durable() bool { return false }

func (s *RedisStore) rowKey(id string) string        { return s.prefix + ":row:" + id }
func (s *RedisStore) openIndexKey(org string) string { return s.prefix + ":open:org:" + org }

func (s *RedisStore) PutOpen(ctx context.Context, a Activation) error {
	if a.Spend == nil {
		a.Spend = map[string]float64{}
	}
	blob, err := json.Marshal(a)
	if err != nil {
		return err
	}
	// SET NX: first writer wins; a replayed acquire with the same minted id
	// is a no-op. No TTL on OPEN rows — they are load-bearing.
	ok, err := s.c.SetNX(ctx, s.rowKey(a.ActivationID), blob, 0).Result()
	if err != nil {
		return err
	}
	if ok {
		return s.c.SAdd(ctx, s.openIndexKey(a.OrgID), a.ActivationID).Err()
	}
	return nil
}

func (s *RedisStore) Get(ctx context.Context, id string) (*Activation, error) {
	raw, err := s.c.Get(ctx, s.rowKey(id)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a Activation
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *RedisStore) transition(ctx context.Context, id, from, to string, extra map[string]any) (*Activation, error) {
	extraJSON, err := json.Marshal(extra)
	if err != nil {
		return nil, err
	}
	out, err := transitionScript.Run(ctx, s.c,
		[]string{s.rowKey(id)},
		from, to, int(s.ttl.Seconds()), string(extraJSON),
	).Result()
	if err != nil {
		return nil, err
	}
	str, _ := out.(string)
	if str == "" {
		return nil, nil // already transitioned / unknown
	}
	var a Activation
	if err := json.Unmarshal([]byte(str), &a); err != nil {
		return nil, err
	}
	_ = s.c.SRem(ctx, s.openIndexKey(a.OrgID), id).Err()
	return &a, nil
}

func (s *RedisStore) MarkReleased(ctx context.Context, id string) (*Activation, error) {
	return s.transition(ctx, id, StateOpen, StateReleased, map[string]any{"released_at": nowISO()})
}

func (s *RedisStore) MarkSettled(ctx context.Context, id, cost string) (*Activation, error) {
	// A settle may follow either RELEASED or OPEN. Try both.
	for _, from := range []string{StateReleased, StateOpen} {
		row, err := s.transition(ctx, id, from, StateSettled, map[string]any{"settled_at": nowISO(), "cost": cost})
		if err != nil {
			return nil, err
		}
		if row != nil {
			return row, nil
		}
	}
	return nil, nil
}

func (s *RedisStore) ListOpen(ctx context.Context, orgID string, limit int) ([]Activation, error) {
	ids, err := s.c.SMembers(ctx, s.openIndexKey(orgID)).Result()
	if err != nil {
		return nil, err
	}
	var out []Activation
	for _, id := range ids {
		a, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if a != nil && a.State == StateOpen {
			out = append(out, *a)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenedAt < out[j].OpenedAt })
	return out, nil
}

func (s *RedisStore) CountOpen(ctx context.Context, orgID string) (int, error) {
	n, err := s.c.SCard(ctx, s.openIndexKey(orgID)).Result()
	return int(n), err
}

// StaleOpen returns OPEN activations older than olderThan (the drift alarm —
// QB-03 made observable). opened_at is RFC3339Nano.
func StaleOpen(opens []Activation, olderThan time.Duration, now time.Time) []Activation {
	var out []Activation
	for _, a := range opens {
		t, err := time.Parse(time.RFC3339Nano, a.OpenedAt)
		if err != nil {
			continue
		}
		if now.Sub(t) >= olderThan {
			out = append(out, a)
		}
	}
	return out
}
