package quota

// Red test for finding QG-01 (evidence E-D1) — ticket
// sandbox-platform/tickets/20260709_ab0t_quota_systemic_integrity_redesign.
//
// Contract under test: a service that CONFIGURES a Redis backend
// (storage.redis_url) gets counters that survive a process restart and are
// shared across replicas. Today counters.NewRedisStore is a stub returning
// ErrRedisNotAvailable (counters/store_redis.go:16-18) and Setup always
// builds an in-memory factory (quota/setup.go:134), so every restart zeroes
// all usage and N replicas each enforce their own limit.
//
// EXPECTED RED until TASK P5.1 (real Redis backend). When P5.1 lands, point
// AB0T_QUOTA_TEST_REDIS_ADDR at a test Redis (miniredis / local) — the test
// is written so the default URL below only matters once a real client
// exists; the assertion fails today before any connection is attempted.

import (
	"context"
	"os"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/engine"
)

func TestConfiguredRedisBackend_PersistsAcrossRestart_QG01(t *testing.T) {
	// P5.1: an in-process miniredis stands in for a shared Redis. The two
	// Setup calls below are two "processes" pointing at the SAME server —
	// exactly the durability property QG-01 said was missing.
	redisURL := os.Getenv("AB0T_QUOTA_TEST_REDIS_ADDR")
	if redisURL == "" {
		mr := miniredis.RunT(t)
		redisURL = "redis://" + mr.Addr()
	}

	mkConfig := func() *config.Config {
		cfg := minimalConfig()
		cfg.Storage = config.StorageConfig{RedisURL: redisURL}
		return cfg
	}
	ctx := context.Background()
	in := engine.CheckInput{UserID: "alice", OrgID: "org-1", ResourceKey: "sandbox.concurrent", Cost: 1}

	// Process 1: spend one unit.
	q1, err := Setup(ctx, Options{ConfigOverride: mkConfig()})
	if err != nil {
		t.Fatalf("Setup #1: %v", err)
	}
	defer q1.Close(ctx)
	if _, err := q1.Engine.Spend(ctx, in); err != nil {
		t.Fatalf("Spend: %v", err)
	}

	// Simulated restart: a fresh Setup from the same config must see the
	// same usage if the configured Redis backend is real.
	q2, err := Setup(ctx, Options{ConfigOverride: mkConfig()})
	if err != nil {
		t.Fatalf("Setup #2: %v", err)
	}
	defer q2.Close(ctx)

	res, err := q2.Engine.Check(ctx, in)
	if err != nil {
		t.Fatalf("Check after restart: %v", err)
	}
	if res.Used != 1 {
		t.Errorf("QG-01: configured Redis backend did not persist across restart: Used=%v after restart (want 1). "+
			"FloatStore=%q — counters are process-local; every restart zeroes usage and each replica enforces its own limit.",
			res.Used, q2.Capabilities().FloatStore)
	}
}
