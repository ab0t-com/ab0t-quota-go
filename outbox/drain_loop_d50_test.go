package outbox

// D-50 — NEVER test a background worker by calling the function it calls.
// Python's _drain_loop referenced a deleted attribute and raised every pass,
// swallowed into a permanent backoff; the outbox never drained behind 700
// passing tests because every test called drain() directly. Here we DRIVE THE
// REAL LOOP (RunDrainLoop in a goroutine) and assert delivery AT THE SINK.
//
// Negative control: a KILLED loop delivers nothing even though a pending intent
// exists — proving the test observes the actual loop, not Drain().

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type recordingPublisher struct {
	mu    sync.Mutex
	count int
}

func (p *recordingPublisher) Publish(context.Context, Record) error {
	p.mu.Lock()
	p.count++
	p.mu.Unlock()
	return nil
}
func (p *recordingPublisher) delivered() int { p.mu.Lock(); defer p.mu.Unlock(); return p.count }

func seedPending(t *testing.T, store Store) {
	t.Helper()
	// EmitViaOutbox with a failing publisher leaves one PENDING intent.
	e := NewEmitter(store, &flakyPublisher{failFirst: true}, 900, "")
	if d, _ := e.EmitViaOutbox(context.Background(), "resv-loop:stopped", Record{
		Key: "resv-loop:stopped", Event: json.RawMessage(`{"r":"1"}`), EventType: "stopped",
		ResourceType: "sandbox", ReservationID: "resv-loop", FirstTS: nowEpoch(),
	}); d {
		t.Fatal("setup: publish should have failed to leave a pending intent")
	}
}

func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func TestDrainLoop_RealWorkerDeliversToSink_D50(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	store := NewRedisStore(c, "outbox")
	seedPending(t, store)

	pub := &recordingPublisher{}
	e := NewEmitter(store, pub, 900, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Drive the REAL loop (fast tick), do NOT call e.Drain directly.
	go e.RunDrainLoop(ctx, 10*time.Millisecond, 100)

	if !waitFor(func() bool { return pub.delivered() == 1 }, 3*time.Second) {
		t.Fatalf("the real drain loop never delivered to the sink (delivered=%d) — a dead worker inside a healthy process", pub.delivered())
	}
	// And the delivered intent left the durable store.
	if !waitFor(func() bool { n, _ := e.PendingCount(ctx); return n == 0 }, 2*time.Second) {
		t.Error("delivered intent should be gone from the store")
	}
}

// NEGATIVE CONTROL: a killed loop delivers nothing → the harness is observing
// the real loop (if it were calling Drain() directly, this would deliver).
func TestDrainLoop_KilledWorker_DeliversNothing_D50(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	store := NewRedisStore(c, "outbox")
	seedPending(t, store)

	pub := &recordingPublisher{}
	e := NewEmitter(store, pub, 900, "")
	e.Kill() // dead worker
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.RunDrainLoop(ctx, 10*time.Millisecond, 100)

	time.Sleep(300 * time.Millisecond)
	if pub.delivered() != 0 {
		t.Errorf("killed loop must deliver nothing, got %d (harness not observing the real loop)", pub.delivered())
	}
	// The intent is still pending (nothing drained it).
	if n, _ := e.PendingCount(ctx); n != 1 {
		t.Errorf("pending intent should remain under a killed loop, got %d", n)
	}
}
