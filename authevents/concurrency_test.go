package authevents

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
)

// TestReceiver_100ParallelPostsRunHandlerOnce stresses the delivery-dedup
// path. The lock-based memory ledger should make 100 concurrent POSTs of
// the same event_id resolve to exactly one inner-handler invocation; the
// other 99 see an in-progress / completed row and return cached.
func TestReceiver_100ParallelPostsRunHandlerOnce(t *testing.T) {
	registry := NewRegistry()
	store := handlerledger.NewInMemoryLedgerStore()
	srv := MakeRouter(ReceiverConfig{Secret: "shh", Registry: registry, LedgerStore: store})

	calls := atomic.Int32{}
	h := Idempotent(handlerledger.IdempotentConfig{
		Handler: "concurrent",
		Retry:   handlerledger.NoRetry,
	}, func(ctx context.Context, e handlerledger.Event, hctx *handlerledger.Context) error {
		calls.Add(1)
		// Slow the handler a touch so concurrent POSTs really pile up.
		time.Sleep(2 * time.Millisecond)
		return nil
	})
	registry.OnAuthEvent("org.created", h)

	body := []byte(`{"event_type":"org.created","event_id":"e-race","data":{"user_id":"u1"}}`)
	sig := SignBody(body, "shh")

	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("POST", WebhookPath, bytes.NewReader(body))
			req.Header.Set("X-Event-Signature", sig)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d", rec.Code)
			}
		}()
	}
	wg.Wait()

	got := calls.Load()
	if got != 1 {
		t.Errorf("inner handler should run exactly once under 100x concurrent dispatch, got %d", got)
	}
}

// TestReceiver_DifferentEventsAllProcess confirms only same-id dedup
// applies; 50 distinct events all run.
func TestReceiver_DifferentEventsAllProcess(t *testing.T) {
	registry := NewRegistry()
	store := handlerledger.NewInMemoryLedgerStore()
	srv := MakeRouter(ReceiverConfig{Secret: "shh", Registry: registry, LedgerStore: store})

	calls := atomic.Int32{}
	h := Idempotent(handlerledger.IdempotentConfig{
		Handler: "distinct",
		Retry:   handlerledger.NoRetry,
	}, func(ctx context.Context, e handlerledger.Event, hctx *handlerledger.Context) error {
		calls.Add(1)
		return nil
	})
	registry.OnAuthEvent("org.created", h)

	var wg sync.WaitGroup
	const N = 50
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			body := []byte(`{"event_type":"org.created","event_id":"e-` +
				itoa(i) + `","data":{"user_id":"u1"}}`)
			req := httptest.NewRequest("POST", WebhookPath, bytes.NewReader(body))
			req.Header.Set("X-Event-Signature", SignBody(body, "shh"))
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d", rec.Code)
			}
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != N {
		t.Errorf("expected %d calls for distinct events, got %d", N, got)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
