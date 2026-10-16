package handlerledger

// Ticket 20261016 review B-15 (lane B, re-verified at retry.go:107-112): the
// backoff `select` used `break` on ctx.Done(), which leaves only the select —
// the loop then ran the handler again (with a dead context) for every remaining
// attempt. The receiver runs handlers on auth's request context, so a dropped
// delivery kept hitting billing. A cancelled context must stop the retries.
// RED on v0.1.9 (3 calls).

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type cancelEvent struct{}

func (cancelEvent) GetEventID() string   { return "e-cancel" }
func (cancelEvent) GetEventType() string { return "org.created" }
func (cancelEvent) GetUserID() string    { return "u" }
func (cancelEvent) GetOrgID() string     { return "o" }
func (cancelEvent) Raw() json.RawMessage { return json.RawMessage(`{}`) }

func TestRetry_StopsWhenContextIsCancelled_20261016(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	h := Idempotent(IdempotentConfig{
		Handler: "cancel_test",
		Retry:   &RetryConfig{Attempts: 3, Backoff: BackoffConstant, Initial: 200 * time.Millisecond},
	}, func(ctx context.Context, e Event, hctx *Context) error {
		calls.Add(1)
		cancel() // the caller went away during attempt 1
		return errors.New("billing unavailable")
	})
	_ = Dispatch(ctx, h, cancelEvent{}, NewInMemoryLedgerStore())
	if n := calls.Load(); n != 1 {
		t.Fatalf("handler ran %d times after its context was cancelled, want 1", n)
	}
}
