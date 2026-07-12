// Real-Redis conformance for the Go money-critical scripts (W-RR, 2026-07-11).
//
// The whole Go suite runs on miniredis (gopher-lua). Per D-57, the emulators
// disagree with each other AND with real Redis; no Go Lua had ever met a real
// redis-server. This test points the ACTUAL scripts (acquireSrc via AtomicAcquire,
// decrFloorSrc via DecrByFloorZero) at a real server.
//
// It is SKIPPED unless REAL_REDIS_ADDR is set (e.g. "127.0.0.1:6500"), so it
// never breaks the emulator-only CI. Run:
//
//	REAL_REDIS_ADDR=127.0.0.1:6500 go test ./counters/ -run RealRedis -v
package counters

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func realRedisClientOrSkip(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REAL_REDIS_ADDR")
	if addr == "" {
		t.Skip("REAL_REDIS_ADDR not set — real-Redis conformance is operator-gated")
	}
	c := redis.NewClient(&redis.Options{Addr: addr})
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("cannot reach real Redis at %s: %v", addr, err)
	}
	c.FlushDB(context.Background())
	return c
}

func f64p(v float64) *float64 { return &v }

func TestRealRedis_DecrByFloorZero_FloatRoundTripAndFloor(t *testing.T) {
	c := realRedisClientOrSkip(t)
	defer c.Close()
	ctx := context.Background()
	s := &redisFloatStore{c: c}

	// Float round-trip via INCRBYFLOAT: 1.0 minus 10×0.1 must not go negative and
	// (on real Redis long double) lands clean at 0. HOLDS = never negative.
	if err := s.Set(ctx, "k", 1.0, 0); err != nil {
		t.Fatal(err)
	}
	var last float64
	for i := 0; i < 10; i++ {
		v, err := s.DecrByFloorZero(ctx, "k", 0.1)
		if err != nil {
			t.Fatalf("decr %d: %v", i, err)
		}
		last = v
		if v < 0 {
			t.Fatalf("gauge went NEGATIVE at step %d: %v (forbidden D-31 direction)", i, v)
		}
	}
	t.Logf("real-Redis 1.0 - 10x0.1 = %v (miniredis leaves a float64 residual; real long double is cleaner)", last)

	// Over-release floors at zero, never negative.
	v, err := s.DecrByFloorZero(ctx, "k", 5.0)
	if err != nil {
		t.Fatal(err)
	}
	if v != 0 {
		t.Fatalf("floor failed on real Redis: got %v want 0", v)
	}
}

func TestRealRedis_AtomicAcquire_AdmitDupDeny(t *testing.T) {
	c := realRedisClientOrSkip(t)
	defer c.Close()
	ctx := context.Background()
	s := &redisFloatStore{c: c}
	spec := []AcquireSpec{{OrgKey: "quota:o:rk:gauge", Delta: 1, OrgLimit: f64p(3)}}

	out, err := s.AtomicAcquire(ctx, "quota:o:rk:idem:a", true, time.Minute, spec)
	if err != nil || !out.Admitted || out.Dup {
		t.Fatalf("first acquire: out=%+v err=%v (want admitted, not dup)", out, err)
	}
	// Replay same idem key → admitted as dup, NOT re-spent.
	out, err = s.AtomicAcquire(ctx, "quota:o:rk:idem:a", true, time.Minute, spec)
	if err != nil || !out.Admitted || !out.Dup {
		t.Fatalf("dup acquire: out=%+v err=%v (want admitted+dup)", out, err)
	}
	// Push to the limit then deny.
	_, _ = s.AtomicAcquire(ctx, "quota:o:rk:idem:b", true, time.Minute, spec) // ->2
	_, _ = s.AtomicAcquire(ctx, "quota:o:rk:idem:c", true, time.Minute, spec) // ->3
	out, err = s.AtomicAcquire(ctx, "quota:o:rk:idem:d", true, time.Minute, spec)
	if err != nil {
		t.Fatal(err)
	}
	if out.Admitted || out.DeniedIndex != 1 {
		t.Fatalf("acquire past limit: out=%+v (want denied, index 1)", out)
	}
	gv, _, _ := s.GetFloat(ctx, "quota:o:rk:gauge")
	if gv != 3 {
		t.Fatalf("gauge = %v, want 3 (denied spend must not mutate)", gv)
	}
}

// The property miniredis fundamentally cannot prove: true atomicity under real
// concurrent connections. N goroutines race to acquire against a small limit;
// exactly `limit` must be admitted.
func TestRealRedis_AtomicAcquire_ConcurrentAtLimit(t *testing.T) {
	c := realRedisClientOrSkip(t)
	defer c.Close()
	ctx := context.Background()
	s := &redisFloatStore{c: c}

	const limit, racers = 10, 50
	for trial := 0; trial < 3; trial++ {
		c.FlushDB(ctx)
		var admitted int64
		var wg sync.WaitGroup
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				out, err := s.AtomicAcquire(ctx, "", false, time.Minute,
					[]AcquireSpec{{OrgKey: "quota:race:rk:gauge", Delta: 1, OrgLimit: f64p(limit)}})
				if err == nil && out.Admitted {
					atomic.AddInt64(&admitted, 1)
				}
			}(i)
		}
		wg.Wait()
		gv, _, _ := s.GetFloat(ctx, "quota:race:rk:gauge")
		if admitted != limit || gv != limit {
			t.Fatalf("trial %d: admitted=%d gauge=%v want %d/%d — NOT atomic on real Redis",
				trial, admitted, gv, limit, limit)
		}
		t.Logf("trial %d: %d racers, exactly %d admitted, gauge=%v — atomic HOLDS", trial, racers, admitted, gv)
	}
}
