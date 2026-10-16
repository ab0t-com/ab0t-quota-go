package outbox_test

// Ticket 20261016 review (lane B, re-verified), money findings in the settlement
// fallback. RED on v0.1.9.
//
// B-02: a money event with no reservation id was settled under settlement_key "".
//       Billing accepts a blank key (app/models/billing.py:705, plain str) and
//       dedups it per org FOREVER — so the org's first such settlement landed and
//       every later one came back 200 replayed=true: marked delivered, no money
//       moved, no alert. Never send a blank key: void + alert, loudly.
// B-03: a terminal event with no stopped_at was priced to time.Now() AT SETTLE
//       TIME — which for the fallback is past the retry horizon (minutes to days
//       after the stop). The customer was billed for the outage. The fallback is
//       the time the event entered the outbox (what Python stamps at emit).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/outbox"
	"github.com/shopspring/decimal"
)

func TestSettle_NeverSendsABlankSettlementKey_20261016(t *testing.T) {
	fb := newFakeBilling()
	fb.seed("org-1", "100")
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()
	for i, rid := range []string{"", ""} {
		ev := terminalEvent("org-1", rid, 1)
		_, err := store.PutIntent(context.Background(), outbox.Record{
			Key: "act-" + string(rune('a'+i)) + ":resource.stopped", Event: ev, EventType: "resource.stopped",
			ResourceType: "sandbox", ReservationID: rid, FirstTS: float64(time.Now().Add(-24 * time.Hour).Unix()),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if fb.calls != 0 {
		t.Fatalf("billing was called %d time(s) with a blank settlement_key — the 2nd+ settlement for the "+
			"org is silently 'replayed' and its money lost", fb.calls)
	}
	if len(em.Voids()) != 2 {
		t.Fatalf("each keyless money event must be voided + alerted (loud), got voids=%+v", em.Voids())
	}
}

func TestSettle_MissingStoppedAtUsesOutboxTimeNotSettleTime_20261016(t *testing.T) {
	fb := newFakeBilling()
	fb.seed("org-1", "100")
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()
	emitted := time.Now().Add(-24 * time.Hour) // stopped (and emitted) a day ago; billing was down since
	ev := map[string]any{
		"event_type": "resource.stopped", "org_id": "org-1", "resource_id": "sbx-1", "reservation_id": "res-1",
		"hourly_rate": "1.00", "allocation_fee": "0",
		"started_at": emitted.Add(-2 * time.Hour).UTC().Format(time.RFC3339), // ran 2 hours
		// no stopped_at
	}
	b, _ := json.Marshal(ev)
	if _, err := store.PutIntent(context.Background(), outbox.Record{
		Key: "res-1:resource.stopped", Event: b, EventType: "resource.stopped", ResourceType: "sandbox",
		ReservationID: "res-1", FirstTS: float64(emitted.Unix()),
	}); err != nil {
		t.Fatal(err)
	}
	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	// 2h × $1.00 = $2. v0.1.9 priced 26h (to "now") = $26.
	if got := fb.balanceOf("org-1"); !got.Equal(decimal.RequireFromString("98")) {
		t.Fatalf("balance = %s, want 98 (2h × $1): the outage was billed to the customer", got)
	}
}
