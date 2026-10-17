package outbox_test

// hardening_settle_20261016_test.go — adversarial cases for the v0.1.10 settlement
// fixes (B-02 blank key, B-03 stop time), asserted as MONEY at the fake billing.
//
//	whitespace-only reservation id is "blank" too                TestHardening_WhitespaceReservationIDNeverSettles
//	an event's own stopped_at is never overridden                 TestHardening_PresentStoppedAtWins
//	fractional outbox timestamps price to the second              TestHardening_FractionalFirstTSStopTime
//	concurrent drain passes never double-charge (-race)           TestHardening_ConcurrentDrainsChargeOnce

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/outbox"
	"github.com/shopspring/decimal"
)

func putStopped(t *testing.T, store outbox.Store, key, rid string, ev map[string]any, firstTS float64) {
	t.Helper()
	b, _ := json.Marshal(ev)
	if _, err := store.PutIntent(context.Background(), outbox.Record{
		Key: key, Event: b, EventType: "resource.stopped", ResourceType: "sandbox", ReservationID: rid, FirstTS: firstTS,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHardening_WhitespaceReservationIDNeverSettles(t *testing.T) {
	fb := newFakeBilling()
	fb.seed("org-1", "100")
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()
	ids := []string{" ", "\t", "\n", "  \t "}
	for i, rid := range ids {
		ev := terminalEvent("org-1", rid, 1)
		var m map[string]any
		_ = json.Unmarshal(ev, &m)
		putStopped(t, store, fmt.Sprintf("ws-%d:resource.stopped", i), rid, m, float64(time.Now().Add(-24*time.Hour).Unix()))
	}
	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if fb.calls != 0 {
		t.Fatalf("billing called %d time(s) with a whitespace settlement_key", fb.calls)
	}
	if len(em.Voids()) != len(ids) {
		t.Fatalf("voids = %d, want %d (each one loud)", len(em.Voids()), len(ids))
	}
	if !fb.balanceOf("org-1").Equal(decimal.NewFromInt(100)) {
		t.Fatalf("balance moved: %s", fb.balanceOf("org-1"))
	}
}

func TestHardening_PresentStoppedAtWins(t *testing.T) {
	fb := newFakeBilling()
	fb.seed("org-1", "100")
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()
	stop := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	putStopped(t, store, "res-1:resource.stopped", "res-1", map[string]any{
		"event_type": "resource.stopped", "org_id": "org-1", "resource_id": "r", "reservation_id": "res-1",
		"hourly_rate": "1.00", "allocation_fee": "0",
		"started_at": stop.Add(-3 * time.Hour).Format(time.RFC3339), "stopped_at": stop.Format(time.RFC3339),
	}, float64(stop.Add(20*time.Hour).Unix())) // entered the outbox 20 h after the stop
	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if got := fb.payload()["stopped_at"]; got != stop.Format(time.RFC3339) {
		t.Fatalf("stopped_at sent = %v, want the event's own %s", got, stop.Format(time.RFC3339))
	}
	if !fb.balanceOf("org-1").Equal(decimal.NewFromInt(97)) {
		t.Fatalf("balance = %s, want 97 (3 h × $1)", fb.balanceOf("org-1"))
	}
}

func TestHardening_FractionalFirstTSStopTime(t *testing.T) {
	fb := newFakeBilling()
	fb.seed("org-1", "1000")
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()
	emitted := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	firstTS := float64(emitted.Unix()) + 0.75
	putStopped(t, store, "res-f:resource.stopped", "res-f", map[string]any{
		"event_type": "resource.stopped", "org_id": "org-1", "resource_id": "r", "reservation_id": "res-f",
		"hourly_rate": "3600", "allocation_fee": "0", // $1 per second → the price exposes the stop time
		"started_at": emitted.Add(-10 * time.Minute).Format(time.RFC3339),
	}, firstTS)
	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	sent, _ := fb.payload()["stopped_at"].(string)
	ts, err := time.Parse(time.RFC3339Nano, sent)
	if err != nil {
		t.Fatalf("stopped_at %q: %v", sent, err)
	}
	if d := ts.Sub(emitted.Add(750 * time.Millisecond)); d < -time.Millisecond || d > time.Millisecond {
		t.Fatalf("stopped_at = %s, want the outbox time %s (±1 ms)", sent, emitted.Add(750*time.Millisecond).Format(time.RFC3339Nano))
	}
	charged := decimal.NewFromInt(1000).Sub(fb.balanceOf("org-1"))
	if !charged.Equal(decimal.RequireFromString("600.75")) {
		t.Fatalf("charged %s, want 600.75 (10 min 0.75 s at $1/s)", charged)
	}
}

func TestHardening_ConcurrentDrainsChargeOnce(t *testing.T) {
	fb := newFakeBilling()
	fb.seed("org-1", "1000")
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()
	const n = 12
	for i := 0; i < n; i++ {
		ev := terminalEvent("org-1", fmt.Sprintf("res-%d", i), 1) // 1 h × $1 each
		var m map[string]any
		_ = json.Unmarshal(ev, &m)
		putStopped(t, store, fmt.Sprintf("res-%d:resource.stopped", i), fmt.Sprintf("res-%d", i), m,
			float64(time.Now().Add(-24*time.Hour).Unix()))
	}
	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 3; k++ {
				_, _ = em.Drain(context.Background(), 100)
			}
		}()
	}
	wg.Wait()
	if got := fb.balanceOf("org-1"); !got.Equal(decimal.NewFromInt(1000 - n)) {
		t.Fatalf("balance = %s after concurrent drains, want %d — a money event was charged twice or lost", got, 1000-n)
	}
	if left, _ := em.PendingCount(context.Background()); left != 0 {
		t.Fatalf("pending = %d, want 0", left)
	}
}
