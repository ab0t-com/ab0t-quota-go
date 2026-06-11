package handlerledger

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// stubEvent implements the Event interface for tests.
type stubEvent struct {
	id, typ, user, org string
	raw                json.RawMessage
}

func (e stubEvent) GetEventID() string     { return e.id }
func (e stubEvent) GetEventType() string   { return e.typ }
func (e stubEvent) GetUserID() string      { return e.user }
func (e stubEvent) GetOrgID() string       { return e.org }
func (e stubEvent) Raw() json.RawMessage   { return e.raw }

func newEvent(id, typ, user, org string) Event {
	return stubEvent{
		id: id, typ: typ, user: user, org: org,
		raw: json.RawMessage(`{"event_id":"` + id + `"}`),
	}
}

func TestInMemoryRecordAndQuery(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryLedgerStore()

	r, err := s.RecordAttempt(ctx, AttemptInput{
		HandlerName: "h", EventID: "e1", EventType: "x",
		UserID: "u1", OrgID: "o1",
		EventPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Proceed {
		t.Fatal("first attempt should proceed")
	}

	if err := s.RecordOutcome(ctx, OutcomeInput{
		HandlerName: "h", EventID: "e1", Status: StatusSuccess, SideEffectID: "sid",
	}); err != nil {
		t.Fatal(err)
	}

	row, _ := s.GetRow(ctx, "h", "e1")
	if row == nil || row.Status != StatusSuccess {
		t.Errorf("got row: %+v", row)
	}

	rows, _ := s.QueryByUser(ctx, "u1", QueryOptions{})
	if len(rows) != 1 {
		t.Errorf("query by user: got %d rows", len(rows))
	}
}

func TestInMemorySecondAttemptCached(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryLedgerStore()
	_, _ = s.RecordAttempt(ctx, AttemptInput{
		HandlerName: "h", EventID: "e", EventType: "x",
	})
	_ = s.RecordOutcome(ctx, OutcomeInput{
		HandlerName: "h", EventID: "e", Status: StatusSuccess,
	})
	r, _ := s.RecordAttempt(ctx, AttemptInput{
		HandlerName: "h", EventID: "e", EventType: "x",
	})
	if r.Proceed {
		t.Error("second attempt should NOT proceed (cached terminal)")
	}
	if r.CachedRow == nil || r.CachedRow.Status != StatusSuccess {
		t.Errorf("cached row: %+v", r.CachedRow)
	}
}

func TestInMemoryFailedAllowsRetry(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryLedgerStore()
	_, _ = s.RecordAttempt(ctx, AttemptInput{
		HandlerName: "h", EventID: "e", EventType: "x",
	})
	_ = s.RecordOutcome(ctx, OutcomeInput{
		HandlerName: "h", EventID: "e", Status: StatusFailed,
	})
	r, _ := s.RecordAttempt(ctx, AttemptInput{
		HandlerName: "h", EventID: "e", EventType: "x",
	})
	if !r.Proceed {
		t.Error("Failed (not _permanent) should allow retry")
	}
}

func TestInMemoryFailedPermanentBlocksRetry(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryLedgerStore()
	_, _ = s.RecordAttempt(ctx, AttemptInput{HandlerName: "h", EventID: "e", EventType: "x"})
	_ = s.RecordOutcome(ctx, OutcomeInput{HandlerName: "h", EventID: "e", Status: StatusFailedPermanent})
	r, _ := s.RecordAttempt(ctx, AttemptInput{HandlerName: "h", EventID: "e", EventType: "x"})
	if r.Proceed {
		t.Error("FailedPermanent should block retry")
	}
}

func TestBusinessDedupRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryLedgerStore()
	if done, _ := s.AlreadyDone(ctx, "k1"); done {
		t.Fatal("should not be done before MarkDone")
	}
	_ = s.MarkDone(ctx, MarkDoneInput{DedupKey: "k1", SourceHandler: "h", SourceEventID: "e1"})
	if done, _ := s.AlreadyDone(ctx, "k1"); !done {
		t.Fatal("should be done after MarkDone")
	}
}

func TestDeleteUserCascade(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryLedgerStore()
	for _, eid := range []string{"e1", "e2"} {
		_, _ = s.RecordAttempt(ctx, AttemptInput{HandlerName: "h", EventID: eid, EventType: "x", UserID: "u1"})
		_ = s.RecordOutcome(ctx, OutcomeInput{HandlerName: "h", EventID: eid, Status: StatusSuccess})
	}
	_, _ = s.RecordAttempt(ctx, AttemptInput{HandlerName: "h", EventID: "keep", EventType: "x", UserID: "u2"})
	_ = s.RecordOutcome(ctx, OutcomeInput{HandlerName: "h", EventID: "keep", Status: StatusSuccess})

	n, _ := s.DeleteUser(ctx, "u1")
	if n != 2 {
		t.Errorf("deleted %d, want 2", n)
	}
	rows, _ := s.QueryByUser(ctx, "u2", QueryOptions{})
	if len(rows) != 1 {
		t.Errorf("u2 should still have 1 row, got %d", len(rows))
	}
}

func TestDispatchPlainSuccess(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryLedgerStore()
	var ran int32
	h := Idempotent(IdempotentConfig{Handler: "h"},
		func(ctx context.Context, e Event, hctx *Context) error {
			atomic.AddInt32(&ran, 1)
			return nil
		})
	if err := Dispatch(ctx, h, newEvent("e", "x", "u", "o"), s); err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Errorf("handler ran %d, want 1", ran)
	}
	row, _ := s.GetRow(ctx, "h", "e")
	if row == nil || row.Status != StatusSuccess {
		t.Errorf("row: %+v", row)
	}
}

func TestDispatchDeliveryDedup(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryLedgerStore()
	var ran int32
	h := Idempotent(IdempotentConfig{Handler: "h"},
		func(ctx context.Context, e Event, hctx *Context) error {
			atomic.AddInt32(&ran, 1)
			return nil
		})
	ev := newEvent("e_dup", "x", "u", "o")
	_ = Dispatch(ctx, h, ev, s)
	_ = Dispatch(ctx, h, ev, s) // second delivery — should short-circuit
	if ran != 1 {
		t.Errorf("handler ran %d times; delivery dedup failed", ran)
	}
}

func TestDispatchBusinessDedupViaKey(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryLedgerStore()
	var sideEffects int32
	h := Idempotent(IdempotentConfig{
		Handler: "h",
		Key:     func(e Event) string { return "biz:" + e.GetOrgID() },
	}, func(ctx context.Context, e Event, hctx *Context) error {
		if done, _ := hctx.AlreadyDone(ctx); done {
			return hctx.Skip("already granted to org")
		}
		atomic.AddInt32(&sideEffects, 1)
		_ = hctx.MarkDone(ctx, "sid")
		return hctx.Success("sid")
	})

	// Two different users joining same org
	_ = Dispatch(ctx, h, newEvent("e1", "x", "alice", "shared_org"), s)
	_ = Dispatch(ctx, h, newEvent("e2", "x", "bob", "shared_org"), s)

	if sideEffects != 1 {
		t.Errorf("business dedup failed: %d side effects (want 1)", sideEffects)
	}
	row, _ := s.GetRow(ctx, "h", "e2")
	if row == nil || row.Status != StatusSkipped {
		t.Errorf("bob's row should be skipped, got %+v", row)
	}
}

func TestRetryOnFailureThenSuccess(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryLedgerStore()
	var attempts int32
	h := Idempotent(IdempotentConfig{
		Handler: "h",
		Retry:   &RetryConfig{Attempts: 3, Backoff: BackoffExponential, Initial: time.Millisecond, Max: 10 * time.Millisecond},
	}, func(ctx context.Context, e Event, hctx *Context) error {
		n := atomic.AddInt32(&attempts, 1)
		if n < 2 {
			return errors.New("transient")
		}
		return hctx.Success("sid")
	})
	if err := Dispatch(ctx, h, newEvent("e", "x", "u", "o"), s); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Errorf("attempts: %d, want 2", attempts)
	}
	row, _ := s.GetRow(ctx, "h", "e")
	if row.Status != StatusSuccess {
		t.Errorf("status: %v", row.Status)
	}
}

func TestRetryExhaustedMarksFailedPermanent(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryLedgerStore()
	var attempts int32
	h := Idempotent(IdempotentConfig{
		Handler: "h",
		Retry:   &RetryConfig{Attempts: 2, Initial: time.Millisecond, Max: time.Millisecond},
	}, func(ctx context.Context, e Event, hctx *Context) error {
		atomic.AddInt32(&attempts, 1)
		return errors.New("permafail")
	})
	err := Dispatch(ctx, h, newEvent("e", "x", "u", "o"), s)
	if err == nil {
		t.Fatal("expected error after max attempts")
	}
	if attempts != 2 {
		t.Errorf("attempts: %d, want 2", attempts)
	}
	row, _ := s.GetRow(ctx, "h", "e")
	if row.Status != StatusFailedPermanent {
		t.Errorf("status: %v, want failed_permanent", row.Status)
	}
}

func TestSkipSentinel(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryLedgerStore()
	h := Idempotent(IdempotentConfig{Handler: "h"},
		func(ctx context.Context, e Event, hctx *Context) error {
			return hctx.Skip("no work to do")
		})
	if err := Dispatch(ctx, h, newEvent("e", "x", "u", "o"), s); err != nil {
		t.Fatal(err)
	}
	row, _ := s.GetRow(ctx, "h", "e")
	if row.Status != StatusSkipped {
		t.Errorf("status: %v", row.Status)
	}
	if row.Reason != "no work to do" {
		t.Errorf("reason: %v", row.Reason)
	}
}

func TestAutoSelectStorePriority(t *testing.T) {
	if _, ok := AutoSelectStore(AutoSelectOptions{}).(*InMemoryLedgerStore); !ok {
		t.Error("no clients → InMemory")
	}
}

func TestHashKeyStable(t *testing.T) {
	a := HashKey("credit:user:u1:tier:free")
	b := HashKey("credit:user:u1:tier:free")
	if a != b {
		t.Error("hash key not stable")
	}
	if len(a) != 64 {
		t.Errorf("hash key len: %d", len(a))
	}
}
