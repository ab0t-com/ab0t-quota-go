package handlerledger

// Concurrency guard for the durable ledgers (verifier follow-up, 2026-07-10).
// Python has QC-01 — a genuine concurrent-delivery test that once proved
// record_attempt admitted two deliveries. Go's exactly-once guarantees
// (Redis WATCH/MULTI, DDB conditional-put CAS) were only READ, never RACED.
// This races them.
//
// Contract: N concurrent RecordAttempt of the SAME (handler,event_id) →
// EXACTLY ONE proceeds, and the modelled handler body runs EXACTLY ONCE.
// Run under -race and repeated MANY rounds; a concurrency test that passes
// once proves almost nothing. If exactly-one does NOT hold, that is a real
// double-apply defect — reported loudly, not tuned away.

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// raceRound fires n concurrent deliveries of one eventID at store and
// returns how many proceeded and how many times the modelled handler body
// ran. The body runs iff RecordAttempt returned Proceed (the real dispatch
// rule), and is closed out with a terminal RecordOutcome.
func raceRound(t *testing.T, store LedgerStore, eventID string, n int) (proceeds, bodyRuns int) {
	t.Helper()
	ctx := context.Background()
	var proceeded, ran int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start // release all goroutines at once → maximal contention
			res, err := store.RecordAttempt(ctx, AttemptInput{
				HandlerName: "h", EventID: eventID, EventType: "payment.succeeded",
				UserID: "u1", LeaseSeconds: 60,
			})
			if err != nil {
				t.Errorf("RecordAttempt: %v", err)
				return
			}
			if !res.Proceed {
				return
			}
			atomic.AddInt64(&proceeded, 1)
			// Modelled handler body — must run exactly once across all
			// concurrent deliveries of this event.
			atomic.AddInt64(&ran, 1)
			if err := store.RecordOutcome(ctx, OutcomeInput{
				HandlerName: "h", EventID: eventID, Status: StatusSuccess, SideEffectID: "sfx",
			}); err != nil {
				t.Errorf("RecordOutcome: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	return int(atomic.LoadInt64(&proceeded)), int(atomic.LoadInt64(&ran))
}

func assertExactlyOnce(t *testing.T, backend string, rounds, workers int, store LedgerStore) {
	t.Helper()
	for r := 0; r < rounds; r++ {
		eid := fmt.Sprintf("evt-%s-%d", backend, r)
		proceeds, runs := raceRound(t, store, eid, workers)
		if proceeds != 1 {
			t.Fatalf("%s round %d: %d deliveries proceeded (want exactly 1) — DOUBLE-APPLY / lost exactly-once",
				backend, r, proceeds)
		}
		if runs != 1 {
			t.Fatalf("%s round %d: handler body ran %d times (want exactly 1)", backend, r, runs)
		}
		// A post-terminal re-delivery must also be refused.
		res, err := store.RecordAttempt(context.Background(), AttemptInput{HandlerName: "h", EventID: eid, UserID: "u1"})
		if err != nil {
			t.Fatalf("%s round %d re-deliver: %v", backend, r, err)
		}
		if res.Proceed {
			t.Fatalf("%s round %d: re-delivery after success PROCEEDED — idempotency lost", backend, r)
		}
	}
}

func TestRedisLedger_RecordAttemptRace_ExactlyOneProceeds(t *testing.T) {
	// One miniredis, many rounds with distinct event ids.
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	store := &redisLedgerStore{c: c}
	assertExactlyOnce(t, "redis", 40, 8, store)
}

func TestDDBLedger_RecordAttemptRace_ExactlyOneProceeds(t *testing.T) {
	c := ddbTestClient(t) // skips if DynamoDB Local is not reachable
	table := makeDDBTable(t, c)
	store := &ddbLedgerStore{c: c, table: table}
	// Fewer rounds than redis — each DDB round is real network round-trips.
	assertExactlyOnce(t, "ddb", 20, 6, store)
}

// --- Negative control: prove the race harness HAS TEETH -------------------
//
// A test that cannot fail proves nothing. naiveLedger is a check-then-set
// ledger with NO CAS — the classic TOCTOU that Python's QC-01 caught. It is
// data-race-free (all map access is locked) but LOGICALLY racy: it releases
// the lock between "have I seen this?" and "mark it seen", so concurrent
// deliveries can all observe "unseen" and all proceed. If the harness is
// sound it must catch this as proceeds>1. If even the naive store shows
// exactly-one, the harness is vacuous.

type naiveLedger struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newNaiveLedger() *naiveLedger { return &naiveLedger{seen: map[string]bool{}} }

func (s *naiveLedger) RecordAttempt(_ context.Context, in AttemptInput) (*AttemptResult, error) {
	k := in.HandlerName + ":" + in.EventID
	s.mu.Lock()
	already := s.seen[k]
	s.mu.Unlock() // <-- TOCTOU gap: check and set are not atomic
	if already {
		return &AttemptResult{Proceed: false}, nil
	}
	runtime.Gosched() // widen the window so the defect manifests reliably
	time.Sleep(50 * time.Microsecond)
	s.mu.Lock()
	s.seen[k] = true
	s.mu.Unlock()
	return &AttemptResult{Proceed: true}, nil
}

// Remaining LedgerStore methods are unused by the race harness's proceed
// counting; stubbed to satisfy the interface.
func (s *naiveLedger) RecordOutcome(context.Context, OutcomeInput) error { return nil }
func (s *naiveLedger) GetRow(context.Context, string, string) (*LedgerRow, error) {
	return nil, nil
}
func (s *naiveLedger) AlreadyDone(context.Context, string) (bool, error) { return false, nil }
func (s *naiveLedger) MarkDone(context.Context, MarkDoneInput) error     { return nil }
func (s *naiveLedger) QueryByUser(context.Context, string, QueryOptions) ([]*LedgerRow, error) {
	return nil, nil
}
func (s *naiveLedger) QueryByStatus(context.Context, LedgerStatus, QueryOptions) ([]*LedgerRow, error) {
	return nil, nil
}
func (s *naiveLedger) DeleteUser(context.Context, string) (int, error) { return 0, nil }

func TestRaceHarness_DetectsDoubleApply_NegativeControl(t *testing.T) {
	// The naive (CAS-less) store MUST be caught: across many rounds at
	// least one shows >1 proceed. If none do, the harness lacks teeth and
	// the durable-store greens above are worthless.
	store := newNaiveLedger()
	maxProceeds := 0
	for r := 0; r < 40; r++ {
		p, _ := raceRoundNoOutcome(t, store, fmt.Sprintf("naive-%d", r), 16)
		if p > maxProceeds {
			maxProceeds = p
		}
	}
	if maxProceeds <= 1 {
		t.Fatalf("negative control never double-applied (max proceeds=%d) — the race harness is VACUOUS; "+
			"the durable-store exactly-once greens cannot be trusted", maxProceeds)
	}
	t.Logf("negative control double-applied as expected (max proceeds in a round = %d) — harness has teeth", maxProceeds)
}

// raceRoundNoOutcome is raceRound without the RecordOutcome call (the naive
// control only implements the proceed decision).
func raceRoundNoOutcome(t *testing.T, store LedgerStore, eventID string, n int) (proceeds, bodyRuns int) {
	t.Helper()
	ctx := context.Background()
	var proceeded int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			res, err := store.RecordAttempt(ctx, AttemptInput{HandlerName: "h", EventID: eventID, UserID: "u1"})
			if err != nil {
				t.Errorf("RecordAttempt: %v", err)
				return
			}
			if res.Proceed {
				atomic.AddInt64(&proceeded, 1)
			}
		}()
	}
	close(start)
	wg.Wait()
	return int(atomic.LoadInt64(&proceeded)), 0
}
