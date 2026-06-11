package handlerledger

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeEvent is a minimal Event that satisfies the handlerledger contract.
// Used in this package (where authevents.Event isn't importable due to
// the cycle).
type fakeEvent struct {
	id, t, uid, oid string
	raw             json.RawMessage
}

func (f fakeEvent) GetEventID() string   { return f.id }
func (f fakeEvent) GetEventType() string { return f.t }
func (f fakeEvent) GetUserID() string    { return f.uid }
func (f fakeEvent) GetOrgID() string     { return f.oid }
func (f fakeEvent) Raw() json.RawMessage { return f.raw }

// runConformance exercises every status path the LedgerStore contract
// must support. Add new backends here when wiring Redis / DDB in v0.2.
func runConformance(t *testing.T, name string, make func() LedgerStore) {
	t.Helper()
	t.Run(name+"/SuccessPath", func(t *testing.T) { successPath(t, make()) })
	t.Run(name+"/SkippedPath", func(t *testing.T) { skippedPath(t, make()) })
	t.Run(name+"/FailedPermanent", func(t *testing.T) { failedPermanentPath(t, make()) })
	t.Run(name+"/BusinessDedup", func(t *testing.T) { businessDedupPath(t, make()) })
	t.Run(name+"/DeleteUser", func(t *testing.T) { deleteUserPath(t, make()) })
	t.Run(name+"/SecondAttemptReturnsCached", func(t *testing.T) { secondAttemptCached(t, make()) })
}

func successPath(t *testing.T, store LedgerStore) {
	ev := fakeEvent{id: "e-ok", t: "org.created", uid: "u1", oid: "o1"}
	h := Idempotent(IdempotentConfig{Handler: "h", Retry: NoRetry},
		func(ctx context.Context, e Event, hctx *Context) error { return nil })
	if err := Dispatch(context.Background(), h, ev, store); err != nil {
		t.Fatal(err)
	}
	row, _ := store.GetRow(context.Background(), "h", "e-ok")
	if row == nil || row.Status != StatusSuccess {
		t.Errorf("row=%+v", row)
	}
}

func skippedPath(t *testing.T, store LedgerStore) {
	ev := fakeEvent{id: "e-skip", t: "org.created", uid: "u1"}
	h := Idempotent(IdempotentConfig{Handler: "h", Retry: NoRetry},
		func(ctx context.Context, e Event, hctx *Context) error {
			return hctx.Skip("nope")
		})
	if err := Dispatch(context.Background(), h, ev, store); err != nil {
		t.Fatal(err)
	}
	row, _ := store.GetRow(context.Background(), "h", "e-skip")
	if row == nil || row.Status != StatusSkipped {
		t.Errorf("row=%+v", row)
	}
}

func failedPermanentPath(t *testing.T, store LedgerStore) {
	ev := fakeEvent{id: "e-fail", t: "org.created", uid: "u1"}
	h := Idempotent(IdempotentConfig{
		Handler: "h",
		Retry: &RetryConfig{Attempts: 2, Backoff: BackoffConstant, Initial: time.Millisecond, Max: time.Millisecond},
	},
		func(ctx context.Context, e Event, hctx *Context) error { return errors.New("boom") })
	_ = Dispatch(context.Background(), h, ev, store)
	row, _ := store.GetRow(context.Background(), "h", "e-fail")
	if row == nil || row.Status != StatusFailedPermanent {
		t.Errorf("row=%+v", row)
	}
}

func businessDedupPath(t *testing.T, store LedgerStore) {
	ctx := context.Background()
	if err := store.MarkDone(ctx, MarkDoneInput{
		DedupKey: "credit_granted:user:u1:pro", SourceHandler: "h", SourceEventID: "e1",
	}); err != nil {
		t.Fatal(err)
	}
	done, err := store.AlreadyDone(ctx, "credit_granted:user:u1:pro")
	if err != nil || !done {
		t.Errorf("done=%v err=%v", done, err)
	}
	done, err = store.AlreadyDone(ctx, "credit_granted:user:u2:pro")
	if err != nil || done {
		t.Errorf("unrelated should be false: done=%v err=%v", done, err)
	}
}

func deleteUserPath(t *testing.T, store LedgerStore) {
	ctx := context.Background()
	for i, eid := range []string{"e1", "e2", "e3"} {
		ev := fakeEvent{id: eid, t: "org.created", uid: "u-delete", oid: "o"}
		h := Idempotent(IdempotentConfig{Handler: "h", Retry: NoRetry},
			func(ctx context.Context, e Event, hctx *Context) error { return nil })
		if err := Dispatch(ctx, h, ev, store); err != nil {
			t.Fatalf("dispatch[%d]: %v", i, err)
		}
	}
	rows, _ := store.QueryByUser(ctx, "u-delete", QueryOptions{Limit: 100})
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	n, err := store.DeleteUser(ctx, "u-delete")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("delete count = %d, want 3", n)
	}
	rows, _ = store.QueryByUser(ctx, "u-delete", QueryOptions{Limit: 100})
	if len(rows) != 0 {
		t.Errorf("after delete, found %d rows", len(rows))
	}
}

func secondAttemptCached(t *testing.T, store LedgerStore) {
	ev := fakeEvent{id: "e-twice", t: "org.created", uid: "u1"}
	calls := 0
	h := Idempotent(IdempotentConfig{Handler: "h", Retry: NoRetry},
		func(ctx context.Context, e Event, hctx *Context) error {
			calls++
			return nil
		})
	if err := Dispatch(context.Background(), h, ev, store); err != nil {
		t.Fatal(err)
	}
	if err := Dispatch(context.Background(), h, ev, store); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("inner called %d times (expected 1, cached on second)", calls)
	}
}

func TestConformance_InMemory(t *testing.T) {
	runConformance(t, "InMemory", func() LedgerStore { return NewInMemoryLedgerStore() })
}

// When Redis / DDB backends are wired, add:
//
//	func TestConformance_Redis(t *testing.T) {
//	    if testing.Short() { t.Skip("requires redis") }
//	    runConformance(t, "Redis", func() LedgerStore {
//	        return mustNewRedisLedgerStore(t, ...)
//	    })
//	}
