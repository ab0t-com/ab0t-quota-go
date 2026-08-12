package providers

// Durable last-known-good tests — H8 parity fix (2026-08-12).
// Ticket 20260810_tier_failclosed_lastknowngood follow-up
// ("Go last-known-good durability", PROGRAM_FOLLOWUPS_20260810;
// verify_fabel_5_report_20260812 §7/H8): the in-process LKG map is lost on
// restart, so (restart ∧ billing outage) downgraded paying orgs to the
// fallback default until the first successful fetch. These prove the durable
// store closes that window and that the record is wire-compatible with
// Python's AuthServiceTierProvider (`quota:tier:lkg:{org}` JSON).

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// meshLike returns the sentinel-wrapped error alongside the fallback default,
// exactly like MeshProvider does on a lookup failure.
type meshLike struct {
	tier  string
	fail  bool
	deflt string
}

func (p *meshLike) GetTier(ctx context.Context, userID, orgID string) (string, error) {
	if p.fail {
		return p.deflt, errors.Join(ErrTierLookupFailed, errors.New("billing down"))
	}
	return p.tier, nil
}

func newRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	return mr, redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

func TestDurableLKG_SurvivesRestart(t *testing.T) {
	// Process 1: successful fetch writes the durable record.
	mr, rdb := newRedis(t)
	_ = mr
	inner := &meshLike{tier: "pro", deflt: "free"}
	c1 := WithCache(inner, time.Minute)
	c1.SetDurableLKGStore(NewRedisLKGStore(rdb))
	got, err := c1.GetTier(context.Background(), "u1", "org1")
	if err != nil || got != "pro" {
		t.Fatalf("seed fetch: got %q err %v", got, err)
	}

	// "Restart": a brand-new Cache (empty in-process map) + billing DOWN.
	inner.fail = true
	c2 := WithCache(inner, time.Minute)
	c2.SetDurableLKGStore(NewRedisLKGStore(rdb))
	got, err = c2.GetTier(context.Background(), "u1", "org1")
	if err != nil {
		t.Fatalf("post-restart outage: unexpected err %v", err)
	}
	if got != "pro" {
		t.Fatalf("post-restart outage must serve durable LKG pro, got %q (the H8 downgrade)", got)
	}
}

func TestDurableLKG_NoStore_PriorBehaviorIsDefault(t *testing.T) {
	// Restart with NO durable store wired: outage falls to the uncached
	// fallback default (the documented pre-fix behavior, unchanged).
	inner := &meshLike{tier: "pro", deflt: "free", fail: true}
	c := WithCache(inner, time.Minute)
	got, err := c.GetTier(context.Background(), "u1", "org1")
	if err != nil || got != "free" {
		t.Fatalf("no-store outage: got %q err %v, want free/nil", got, err)
	}
}

func TestDurableLKG_PythonWireFormatRead(t *testing.T) {
	// A record WRITTEN BY PYTHON (`quota:tier:lkg:{org}`, {"tier","ts"}) must
	// be served by the Go cache — mixed-fleet compatibility.
	mr, rdb := newRedis(t)
	payload := `{"tier": "enterprise", "ts": 1754870400.0}`
	if err := rdb.Set(context.Background(), "quota:tier:lkg:orgpy", payload, 0).Err(); err != nil {
		t.Fatal(err)
	}
	_ = mr
	inner := &meshLike{deflt: "free", fail: true}
	c := WithCache(inner, time.Minute)
	c.SetDurableLKGStore(NewRedisLKGStore(rdb))
	got, err := c.GetTier(context.Background(), "u1", "orgpy")
	if err != nil || got != "enterprise" {
		t.Fatalf("python-written LKG: got %q err %v, want enterprise/nil", got, err)
	}
}

func TestDurableLKG_PythonWireFormatWrite(t *testing.T) {
	// The record Go WRITES must parse exactly the way Python reads it.
	mr, rdb := newRedis(t)
	_ = mr
	inner := &meshLike{tier: "pro", deflt: "free"}
	c := WithCache(inner, time.Minute)
	c.SetDurableLKGStore(NewRedisLKGStore(rdb))
	if _, err := c.GetTier(context.Background(), "u1", "org1"); err != nil {
		t.Fatal(err)
	}
	raw, err := rdb.Get(context.Background(), "quota:tier:lkg:org1").Result()
	if err != nil {
		t.Fatalf("expected durable record at python key: %v", err)
	}
	var p struct {
		Tier string  `json:"tier"`
		Ts   float64 `json:"ts"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Tier != "pro" || p.Ts <= 0 {
		t.Fatalf("record not python-compatible: %s (err %v)", raw, err)
	}
}

func TestDurableLKG_MaxAgeBoundsDurableRecordToo(t *testing.T) {
	mr, rdb := newRedis(t)
	_ = mr
	inner := &meshLike{tier: "pro", deflt: "free"}
	c1 := WithCache(inner, time.Minute)
	c1.SetDurableLKGStore(NewRedisLKGStore(rdb))
	if _, err := c1.GetTier(context.Background(), "u1", "org1"); err != nil {
		t.Fatal(err)
	}

	inner.fail = true
	c2 := WithCache(inner, time.Minute)
	c2.SetDurableLKGStore(NewRedisLKGStore(rdb))
	c2.SetLastKnownGoodMaxAge(time.Hour)
	c2.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	got, err := c2.GetTier(context.Background(), "u1", "org1")
	if err != nil || got != "free" {
		t.Fatalf("expired durable LKG must fall to default (uncached): got %q err %v", got, err)
	}
}

func TestDurableLKG_StoreFailureIsFailSafe(t *testing.T) {
	// A dead Redis behind the store must not break the hot path: outage with
	// unreachable store behaves exactly like no-store (default, uncached).
	mr, rdb := newRedis(t)
	mr.Close() // kill the backend
	inner := &meshLike{tier: "pro", deflt: "free", fail: true}
	c := WithCache(inner, time.Minute)
	c.SetDurableLKGStore(NewRedisLKGStore(rdb))
	got, err := c.GetTier(context.Background(), "u1", "org1")
	if err != nil || got != "free" {
		t.Fatalf("dead store must be fail-safe: got %q err %v", got, err)
	}
}

func TestDurableLKG_DurableHitWarmsInProcessMap(t *testing.T) {
	mr, rdb := newRedis(t)
	if err := rdb.Set(context.Background(), "quota:tier:lkg:org1",
		`{"tier": "pro", "ts": 1754870400.0}`, 0).Err(); err != nil {
		t.Fatal(err)
	}
	inner := &meshLike{deflt: "free", fail: true}
	c := WithCache(inner, time.Minute)
	c.SetDurableLKGStore(NewRedisLKGStore(rdb))
	if got, _ := c.GetTier(context.Background(), "u1", "org1"); got != "pro" {
		t.Fatalf("first read: want pro, got %q", got)
	}
	// Kill Redis; the warmed in-process record must keep serving.
	mr.Close()
	if got, _ := c.GetTier(context.Background(), "u1", "org1"); got != "pro" {
		t.Fatalf("warmed map must serve pro after store death, got %q", got)
	}
}
