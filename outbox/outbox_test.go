package outbox

// Tests for the durable outbox (D-44/D-29/D-30/D-32/D-12).
//
// THE test that distinguishes a durable outbox from a queue (D-29): discard
// the emitter AND the store object, and prove a fresh process resumes delivery
// FROM THE EXTERNAL STORE. Python shipped an in-memory queue that passed every
// test it had because no test ever restarted the process. This one does — and
// its NEGATIVE CONTROL proves an in-memory store fails it.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type flakyPublisher struct {
	failFirst bool
	calls     int
}

func (p *flakyPublisher) Publish(_ context.Context, _ Record) error {
	p.calls++
	if p.failFirst {
		return errors.New("SNS blip")
	}
	return nil
}

func rec1() Record {
	return Record{Key: "resv-1:stopped", Event: json.RawMessage(`{"resource_id":"r1"}`),
		EventType: "stopped", ResourceType: "sandbox", ReservationID: "resv-1", FirstTS: nowEpoch()}
}

// D-29: Redis outbox survives a process restart and resumes delivery.
func TestOutbox_RedisSurvivesRestart_ResumesDelivery_D29(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()

	// "Process 1": publish fails → intent stays PENDING in the external store.
	c1 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	e1 := NewEmitter(NewRedisStore(c1, "outbox"), &flakyPublisher{failFirst: true}, 900, "")
	delivered, err := e1.EmitViaOutbox(ctx, "resv-1:stopped", rec1())
	if err != nil || delivered {
		t.Fatalf("emit should not deliver (publish fails): delivered=%v err=%v", delivered, err)
	}
	// DISCARD the emitter AND the store object AND the client — simulate a crash.
	_ = c1.Close()
	e1 = nil

	// "Process 2": brand-new client/store/emitter against the SAME server, a
	// working publisher. It must resume from the store.
	c2 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer c2.Close()
	e2 := NewEmitter(NewRedisStore(c2, "outbox"), &flakyPublisher{failFirst: false}, 900, "")
	if n, _ := e2.PendingCount(ctx); n != 1 {
		t.Fatalf("fresh process should see 1 pending intent in the durable store, got %d", n)
	}
	n, err := e2.Drain(ctx, 100)
	if err != nil || n != 1 {
		t.Fatalf("fresh process must resume delivery from the store: delivered=%d err=%v", n, err)
	}
	if p, _ := e2.PendingCount(ctx); p != 0 {
		t.Errorf("nothing should remain pending after drain, got %d", p)
	}
}

// NEGATIVE CONTROL: an in-memory store does NOT survive a restart — the fresh
// process sees an empty store and the money event is lost. Proves the D-29
// test above has teeth (it would pass vacuously if "restart" didn't matter).
func TestOutbox_InMemoryDoesNotSurviveRestart_NegativeControl_D29(t *testing.T) {
	ctx := context.Background()
	store1 := NewInMemoryStore()
	if store1.Durable() {
		t.Fatal("in-memory store must report Durable()=false")
	}
	e1 := NewEmitter(store1, &flakyPublisher{failFirst: true}, 900, "")
	_, _ = e1.EmitViaOutbox(ctx, "resv-1:stopped", rec1())
	if n, _ := e1.PendingCount(ctx); n != 1 {
		t.Fatalf("intent should be pending in store1, got %d", n)
	}
	// "Restart": a fresh in-memory store is EMPTY — the event evaporated.
	store2 := NewInMemoryStore()
	e2 := NewEmitter(store2, &flakyPublisher{failFirst: false}, 900, "")
	if n, _ := e2.PendingCount(ctx); n != 0 {
		t.Fatalf("negative control broken: fresh in-memory store should be empty, got %d", n)
	}
	n, _ := e2.Drain(ctx, 100)
	if n != 0 {
		t.Errorf("in-memory 'restart' cannot resume — expected 0 delivered, got %d (would-be false green)", n)
	}
}

// D-12: past-horizon events are VOIDED + alerted, never silently dropped.
func TestOutbox_VoidPastHorizon_D12(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	// horizon 0 → everything is instantly past-horizon.
	e := NewEmitter(store, &flakyPublisher{failFirst: false}, 0.0001, "")
	r := rec1()
	r.FirstTS = nowEpoch() - 10 // 10s old, well past a ~0s horizon
	_, _ = store.PutIntent(ctx, r)
	n, err := e.Drain(ctx, 100)
	if err != nil || n != 0 {
		t.Fatalf("past-horizon event must not be 'delivered': delivered=%d err=%v", n, err)
	}
	voids := e.Voids()
	if len(voids) != 1 || voids[0].Reason != "past_retry_horizon" {
		t.Fatalf("expected one past_retry_horizon void, got %+v", voids)
	}
	if p, _ := e.PendingCount(ctx); p != 0 {
		t.Errorf("voided event must leave the pending set, %d remain", p)
	}
}

// nil publisher → the event is fundamentally undeliverable → voided, not dropped.
func TestOutbox_NoPublisherVoids_D12(t *testing.T) {
	ctx := context.Background()
	e := NewEmitter(NewInMemoryStore(), nil, 900, "")
	delivered, _ := e.EmitViaOutbox(ctx, "resv-1:stopped", rec1())
	if delivered {
		t.Error("no publisher → cannot be delivered")
	}
	if v := e.Voids(); len(v) != 1 || v[0].Reason != "no_publisher_configured" {
		t.Errorf("expected a no_publisher void, got %+v", v)
	}
}

// D-32: the durability decision. Includes the negative control — a genuinely
// durable config must NOT be flagged (else the check is uselessly paranoid).
func TestEvaluateDurability_D32(t *testing.T) {
	cases := []struct {
		name                               string
		policy, appendonly, save           string
		configUnavailable, confirmed, want bool
	}{
		{"evicting_allkeys_lru", "allkeys-lru", "yes", "", false, false, false},
		{"evicting_allkeys_random", "allkeys-random", "yes", "3600 1", false, false, false},
		{"no_persistence", "noeviction", "no", "", false, false, false},
		{"durable_appendonly", "noeviction", "yes", "", false, false, true},
		{"durable_rdb_save", "noeviction", "no", "3600 1", false, false, true},
		{"config_unavailable_unconfirmed", "", "", "", true, false, false},
		{"config_unavailable_confirmed", "", "", "", true, true, true},
		// negative control: a durable config must be accepted, not false-flagged.
		{"durable_not_false_flagged", "volatile-lru", "yes", "", false, false, true},
	}
	for _, c := range cases {
		got, reason := EvaluateDurability(c.policy, c.appendonly, c.save, c.configUnavailable, c.confirmed)
		if got != c.want {
			t.Errorf("%s: durable=%v want %v (%s)", c.name, got, c.want, reason)
		}
	}
}
