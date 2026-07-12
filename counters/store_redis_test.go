package counters

// TASK P5.1 — conformance + durability tests for the real go-redis
// FloatStore + RateStore, exercised against an in-process miniredis.
// Ticket 20260709_ab0t_quota_systemic_integrity_redesign (finding QG-01).

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) (redis.Cmdable, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

func TestRedisFloatStore_IncrGetSetExpire(t *testing.T) {
	c, mr := newTestRedis(t)
	s, err := NewRedisStore(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if v, err := s.IncrByFloat(ctx, "k", 1.5); err != nil || v != 1.5 {
		t.Fatalf("incr: v=%v err=%v", v, err)
	}
	if v, err := s.IncrByFloat(ctx, "k", 2.25); err != nil || v != 3.75 {
		t.Fatalf("incr2: v=%v err=%v", v, err)
	}
	got, found, err := s.GetFloat(ctx, "k")
	if err != nil || !found || got != 3.75 {
		t.Fatalf("get: got=%v found=%v err=%v", got, found, err)
	}

	// Absent key → (0,false,nil), not an error.
	if v, found, err := s.GetFloat(ctx, "missing"); err != nil || found || v != 0 {
		t.Fatalf("absent: v=%v found=%v err=%v", v, found, err)
	}

	// TTL via Set + Expire honored by miniredis fast-forward.
	if err := s.Set(ctx, "ttlk", 9, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.GetFloat(ctx, "ttlk"); !found {
		t.Fatal("ttlk should exist before expiry")
	}
	mr.FastForward(2 * time.Hour)
	if _, found, _ := s.GetFloat(ctx, "ttlk"); found {
		t.Error("ttlk should be expired")
	}
}

func TestRedisFloatStore_SetIfAbsent(t *testing.T) {
	c, _ := newTestRedis(t)
	s, _ := NewRedisStore(c)
	ctx := context.Background()

	won, err := s.SetIfAbsent(ctx, "idem", "1", time.Minute)
	if err != nil || !won {
		t.Fatalf("first claim should win: won=%v err=%v", won, err)
	}
	won2, err := s.SetIfAbsent(ctx, "idem", "1", time.Minute)
	if err != nil || won2 {
		t.Fatalf("second claim should lose: won=%v err=%v", won2, err)
	}
}

// TestRedisFloatStore_SurvivesClientRestart is the QG-01 property at the
// store level: a NEW client against the SAME server sees prior writes.
func TestRedisFloatStore_SurvivesClientRestart(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()

	c1 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	s1, _ := NewRedisStore(c1)
	if _, err := s1.IncrByFloat(ctx, "gauge", 3); err != nil {
		t.Fatal(err)
	}
	_ = c1.Close()

	c2 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer c2.Close()
	s2, _ := NewRedisStore(c2)
	v, found, err := s2.GetFloat(ctx, "gauge")
	if err != nil || !found || v != 3 {
		t.Errorf("QG-01: value did not survive client restart: v=%v found=%v err=%v", v, found, err)
	}
}

// QG-06 floor: the Lua decrement-with-floor never lets a value go negative.
func TestRedisFloatStore_DecrByFloorZero(t *testing.T) {
	c, _ := newTestRedis(t)
	s, _ := NewRedisStore(c)
	ctx := context.Background()

	if _, err := s.IncrByFloat(ctx, "g", 2); err != nil {
		t.Fatal(err)
	}
	// Decrement past zero — must floor, not go negative.
	v, err := s.DecrByFloorZero(ctx, "g", 5)
	if err != nil {
		t.Fatal(err)
	}
	if v != 0 {
		t.Errorf("floor: got %v want 0", v)
	}
	got, _, _ := s.GetFloat(ctx, "g")
	if got != 0 {
		t.Errorf("stored value after floor: got %v want 0", got)
	}
	// Normal decrement still works.
	_, _ = s.IncrByFloat(ctx, "g2", 10)
	v, _ = s.DecrByFloorZero(ctx, "g2", 3)
	if v != 7 {
		t.Errorf("normal decr: got %v want 7", v)
	}
}

func TestRedisRateStore_RecordCountTrim(t *testing.T) {
	c, _ := newTestRedis(t)
	rs, err := NewRedisRateStore(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	window := time.Minute
	t0 := time.Unix(1_700_000_000, 0)

	for i := 0; i < 3; i++ {
		if err := rs.Record(ctx, "rate", t0.Add(time.Duration(i)*time.Second), window, ""); err != nil {
			t.Fatal(err)
		}
	}
	n, err := rs.Count(ctx, "rate", t0.Add(3*time.Second), window)
	if err != nil || n != 3 {
		t.Fatalf("in-window count: n=%v err=%v (want 3)", n, err)
	}
	// Advance past the window — everything should be trimmed out.
	n, err = rs.Count(ctx, "rate", t0.Add(2*window), window)
	if err != nil || n != 0 {
		t.Fatalf("post-window count: n=%v err=%v (want 0)", n, err)
	}
}

func TestNewRedisStore_NilClientDegradesLoudly(t *testing.T) {
	if _, err := NewRedisStore(nil); err == nil {
		t.Error("nil client must return ErrRedisNotAvailable, not a silent memory swap")
	}
	if _, err := NewRedisRateStore(nil); err == nil {
		t.Error("nil client must return ErrRedisNotAvailable for rate store too")
	}
}
