package handlerledger

// TASK P5.1 — conformance + targeted tests for the real go-redis ledger
// backend, against an in-process miniredis. Ticket
// 20260709_ab0t_quota_systemic_integrity_redesign (findings QG-01/QG-02).

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newRedisLedgerForTest(t *testing.T) *redisLedgerStore {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return &redisLedgerStore{c: c}
}

// TestConformance_Redis runs the full LedgerStore contract against Redis —
// the same suite InMemory passes.
func TestConformance_Redis(t *testing.T) {
	runConformance(t, "Redis", func() LedgerStore { return newRedisLedgerForTest(t) })
}

// TestRedisLedger_AutoSelectReturnsRealStore proves QG-02's positive side:
// given a real redis.Cmdable, AutoSelectStore returns the durable store
// (NOT *InMemoryLedgerStore) and the honesty test's affirmative-log branch
// is now truthful.
func TestRedisLedger_AutoSelectReturnsRealStore(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })

	store := AutoSelectStore(AutoSelectOptions{Redis: c})
	if _, isMemory := store.(*InMemoryLedgerStore); isMemory {
		t.Fatal("QG-02: a real redis client must yield the durable redis ledger, not *InMemoryLedgerStore")
	}
	if _, isRedis := store.(*redisLedgerStore); !isRedis {
		t.Fatalf("expected *redisLedgerStore, got %T", store)
	}
}

// TestRedisLedger_SurvivesClientRestart is the QG-01 property for the
// ledger: a terminal outcome recorded by one client is seen by a fresh
// client against the same server (idempotency persists across restart).
func TestRedisLedger_SurvivesClientRestart(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()

	c1 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	s1 := &redisLedgerStore{c: c1}
	res, err := s1.RecordAttempt(ctx, AttemptInput{HandlerName: "h", EventID: "e1", UserID: "u1"})
	if err != nil || !res.Proceed {
		t.Fatalf("first attempt should proceed: res=%+v err=%v", res, err)
	}
	if err := s1.RecordOutcome(ctx, OutcomeInput{HandlerName: "h", EventID: "e1", Status: StatusSuccess}); err != nil {
		t.Fatal(err)
	}
	_ = c1.Close()

	c2 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer c2.Close()
	s2 := &redisLedgerStore{c: c2}
	res2, err := s2.RecordAttempt(ctx, AttemptInput{HandlerName: "h", EventID: "e1", UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Proceed {
		t.Error("QG-01: a completed event re-attempted after restart must NOT proceed (idempotency lost)")
	}
	if res2.CachedRow == nil || res2.CachedRow.Status != StatusSuccess {
		t.Errorf("expected cached terminal row after restart, got %+v", res2.CachedRow)
	}
}
