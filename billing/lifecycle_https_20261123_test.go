package billing

// HTTPS lifecycle transport (v0.2.0; the Python emitter's HTTPS contract): POST
// /billing/{org}/lifecycle-events with the client's key; 408 / 429 / 5xx / transport failures retried
// in-call with backoff (0.5 s, 1 s) then kept PENDING for the drain; any other 4xx, a 2xx without
// accepted:true, or an event without org_id is REFUSED (outbox.ErrPublishRefused) and the emitter
// settles-or-voids it at once; the event_id rides every retry unchanged. Never dropped silently.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/ab0t-com/ab0t-quota-go/outbox"
)

type lcSrv struct {
	mu       sync.Mutex
	statuses []int // answered in order; the last one repeats
	replies  []string
	calls    int
	paths    []string
	keys     []string
	bodies   []map[string]any
}

func (s *lcSrv) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	s.keys = append(s.keys, r.Header.Get("X-API-Key"))
	raw, _ := io.ReadAll(r.Body)
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	s.bodies = append(s.bodies, m)
	status, reply := 200, `{"accepted":true,"outcome":"processed"}`
	if len(s.statuses) > 0 {
		status = s.statuses[min(i, len(s.statuses)-1)]
	}
	if len(s.replies) > 0 {
		reply = s.replies[min(i, len(s.replies)-1)]
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, reply)
}

func (s *lcSrv) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *lcSrv) snapshot() ([]string, []string, []map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...), append([]string(nil), s.keys...), append([]map[string]any(nil), s.bodies...)
}

func lcPublisher(t *testing.T, s *lcSrv) (*LifecyclePublisher, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	c, err := New(mesh.URLs{Billing: srv.URL, BillingToken: "ab0t_sk_consumer"})
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	p := NewLifecyclePublisher(c, LifecyclePublisherOptions{Sleep: func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}})
	return p, &waits
}

func lcEvent(org, eventType, eventID string) outbox.Record {
	ev := map[string]any{"event_type": eventType, "org_id": org, "reservation_id": "res-1",
		"resource_id": "box-1", "hourly_rate": "0.25", "started_at": "2026-11-23T06:00:00.000000Z",
		"emitted_at": "2026-11-23T07:00:00.000000Z"}
	if eventID != "" {
		ev["event_id"] = eventID
	}
	raw, _ := json.Marshal(ev)
	return outbox.Record{Key: "res-1:" + eventType, Event: raw, EventType: eventType, ReservationID: "res-1"}
}

func lcRecord(org string) outbox.Record { return lcEvent(org, "resource.heartbeat", "evt_fixed") }

func TestLifecyclePublisher_Delivers_20261123(t *testing.T) {
	s := &lcSrv{}
	p, waits := lcPublisher(t, s)
	if err := p.Publish(context.Background(), lcRecord("org-1")); err != nil {
		t.Fatal(err)
	}
	paths, keys, bodies := s.snapshot()
	if len(paths) != 1 || paths[0] != "POST /billing/org-1/lifecycle-events" || keys[0] != "ab0t_sk_consumer" {
		t.Fatalf("paths=%v keys=%v", paths, keys)
	}
	if b := bodies[0]; b["event_type"] != "resource.heartbeat" || b["reservation_id"] != "res-1" ||
		b["hourly_rate"] != "0.25" || b["event_id"] != "evt_fixed" {
		t.Fatalf("the event must be sent as the outbox stored it: %v", b)
	}
	if len(*waits) != 0 {
		t.Fatalf("no backoff on success: %v", *waits)
	}
}

func TestLifecyclePublisher_RetryableStatusesWithBackoff_20261123(t *testing.T) {
	for _, status := range []int{503, 500, 502, 504, 408, 429} {
		s := &lcSrv{statuses: []int{status, status, 200}, replies: []string{`{"detail":"Service temporarily unavailable"}`,
			`{"detail":"Service temporarily unavailable"}`, `{"accepted":true,"outcome":"processed"}`}}
		p, waits := lcPublisher(t, s)
		if err := p.Publish(context.Background(), lcRecord("org-1")); err != nil {
			t.Fatalf("%d that clears within the attempts must deliver: %v", status, err)
		}
		if s.count() != 3 || len(*waits) != 2 || (*waits)[0] != 500*time.Millisecond || (*waits)[1] != time.Second {
			t.Fatalf("%d: calls=%d waits=%v — want 3 attempts, backoff 0.5 s then 1 s", status, s.count(), *waits)
		}
		// the SAME event_id on every attempt (billing's exactly-once applies to retries)
		_, _, bodies := s.snapshot()
		for _, b := range bodies {
			if b["event_id"] != "evt_fixed" {
				t.Fatalf("%d: a retry changed the event_id: %v", status, b)
			}
		}
	}
	// still 503 after every attempt → a plain error (NOT a refusal): the outbox keeps the event
	s2 := &lcSrv{statuses: []int{503}, replies: []string{`{"detail":"x"}`}}
	p2, _ := lcPublisher(t, s2)
	err := p2.Publish(context.Background(), lcRecord("org-1"))
	if err == nil || errors.Is(err, outbox.ErrPublishRefused) || s2.count() != 3 {
		t.Fatalf("persistent 503: err=%v calls=%d", err, s2.count())
	}
}

func TestLifecyclePublisher_RefusalsNotRetried_20261123(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 422} {
		s := &lcSrv{statuses: []int{status}, replies: []string{`{"detail":"Request refused"}`}}
		p, waits := lcPublisher(t, s)
		err := p.Publish(context.Background(), lcRecord("org-1"))
		if !errors.Is(err, outbox.ErrPublishRefused) || s.count() != 1 || len(*waits) != 0 {
			t.Errorf("%d: err=%v calls=%d waits=%v — a refusal is returned at once (ErrPublishRefused), never retried",
				status, err, s.count(), *waits)
		}
	}
	// 2xx that does not accept → refused (only an affirmative answer retires an event)
	s := &lcSrv{replies: []string{`{"accepted":false,"outcome":"unknown_reservation"}`}}
	p, _ := lcPublisher(t, s)
	err := p.Publish(context.Background(), lcRecord("org-1"))
	if !errors.Is(err, outbox.ErrPublishRefused) || !errors.Is(err, ErrLifecycleRejected) || s.count() != 1 {
		t.Fatalf("accepted=false: err=%v calls=%d", err, s.count())
	}
	// an event without org_id cannot be routed → refused, nothing sent
	s3 := &lcSrv{}
	p3, _ := lcPublisher(t, s3)
	if err := p3.Publish(context.Background(), lcRecord("")); !errors.Is(err, outbox.ErrPublishRefused) || s3.count() != 0 {
		t.Fatalf("no org_id: err=%v calls=%d", err, s3.count())
	}
}

func TestLifecyclePublisher_TransportFailureRetried_20261123(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()
	c, _ := New(mesh.URLs{Billing: url})
	sleeps := 0
	p := NewLifecyclePublisher(c, LifecyclePublisherOptions{Attempts: 2, Sleep: func(context.Context, time.Duration) error {
		sleeps++
		return nil
	}})
	err := p.Publish(context.Background(), lcRecord("org-1"))
	if err == nil || errors.Is(err, outbox.ErrPublishRefused) || sleeps != 1 {
		t.Fatalf("transport failure: err=%v sleeps=%d — want retried once then a plain (retryable) error", err, sleeps)
	}
}

// An intent written without an event_id gets a STABLE derived one — the Python emitter's
// uuid5(<library event-id namespace>, "{key}:{emitted_at}") — identical on every retry.
func TestLifecyclePublisher_DerivedEventIDStable_20261123(t *testing.T) {
	s := &lcSrv{statuses: []int{503, 200}}
	p, _ := lcPublisher(t, s)
	r := lcEvent("org-1", "resource.stopped", "")
	if err := p.Publish(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	_, _, bodies := s.snapshot()
	// reference value computed with Python's uuid.uuid5(uuid.NAMESPACE_URL, name).hex
	want := "evt_75a975c1680554a08fb8af157b1db21b"
	if len(bodies) != 2 || bodies[0]["event_id"] != want || bodies[1]["event_id"] != want {
		t.Fatalf("derived event_id: %v / %v, want %s on both attempts", bodies[0]["event_id"], bodies[1]["event_id"], want)
	}
	if strings.Contains(string(r.Event), "event_id") {
		t.Fatal("the stored intent must not be rewritten")
	}
}

// Through the durable outbox: billing down → the event is kept PENDING (never dropped); the drain
// delivers it once billing answers again.
func TestLifecyclePublisher_ThroughOutbox_NeverDropped_20261123(t *testing.T) {
	s := &lcSrv{statuses: []int{503, 503, 503, 200}}
	p, _ := lcPublisher(t, s)
	em := outbox.NewEmitter(outbox.NewInMemoryStore(), p, 900, "")
	if !em.HasPublisher() {
		t.Fatal("the HTTPS publisher is a transport")
	}
	r := lcRecord("org-1")
	delivered, err := em.EmitViaOutbox(context.Background(), r.Key, r)
	if err != nil || delivered {
		t.Fatalf("billing 503: delivered=%v err=%v — want retained", delivered, err)
	}
	if n, _ := em.PendingCount(context.Background()); n != 1 || len(em.Voids()) != 0 {
		t.Fatalf("pending=%d voids=%v — an undelivered event must stay in the outbox, not be voided", n, em.Voids())
	}
	if got, err := em.Drain(context.Background(), 10); err != nil || got != 1 {
		t.Fatalf("drain: delivered=%d err=%v", got, err)
	}
	if n, _ := em.PendingCount(context.Background()); n != 0 || len(em.Voids()) != 0 {
		t.Fatalf("after drain: pending=%d voids=%v", n, em.Voids())
	}
	if s.count() != 4 {
		t.Fatalf("billing calls=%d, want 3 (emit attempts) + 1 (drain)", s.count())
	}
}

// A refusal goes down the undeliverable path at once: with no settler the event is VOIDED and
// alerted (OnVoid) — loud, never a silent drop, and never retried forever.
func TestLifecyclePublisher_RefusedIsVoidedLoudly_20261123(t *testing.T) {
	s := &lcSrv{statuses: []int{422}, replies: []string{`{"detail":"Request refused"}`}}
	p, _ := lcPublisher(t, s)
	em := outbox.NewEmitter(outbox.NewInMemoryStore(), p, 900, "")
	var alerted []outbox.VoidEntry
	em.OnVoid = func(v outbox.VoidEntry) { alerted = append(alerted, v) }
	r := lcRecord("org-1")
	delivered, err := em.EmitViaOutbox(context.Background(), r.Key, r)
	if err != nil || delivered {
		t.Fatalf("delivered=%v err=%v", delivered, err)
	}
	if n, _ := em.PendingCount(context.Background()); n != 0 {
		t.Fatalf("a refused event must not stay pending forever (pending=%d)", n)
	}
	if len(alerted) != 1 || alerted[0].Reason != "publish_refused" || alerted[0].ReservationID != "res-1" {
		t.Fatalf("void alert: %+v", alerted)
	}
	// Control: the SAME emitter with a plain (retryable) failure keeps the event pending, unvoided.
	s.mu.Lock()
	s.statuses = []int{503}
	s.mu.Unlock()
	r2 := lcEvent("org-1", "resource.started", "evt_2")
	_, _ = em.EmitViaOutbox(context.Background(), r2.Key, r2)
	if n, _ := em.PendingCount(context.Background()); n != 1 || len(alerted) != 1 {
		t.Fatalf("control: pending=%d alerts=%d", n, len(alerted))
	}
}
