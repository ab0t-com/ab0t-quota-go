package outbox_test

// D-12, the CALLER leg (Go runtime) — an undeliverable money event is SETTLED, not lost.
//
// Ticket: billing/output/tickets/20260712_revenue_chain_integrity.
// Companion: the Python suite `tests/test_d12_cross_house_settlement_20260712.py`, which runs
// the SAME scenarios against billing's REAL FastAPI route on real DynamoDB Local + real Redis.
// Conformance: ST-SETTLE-1 in conformance/scenarios.json holds BOTH runtimes to one spec.
//
// WHAT IS AND IS NOT PROVEN HERE (be honest about it — DECISIONS B-D1)
// --------------------------------------------------------------------
// Go cannot import billing's Python. So these tests drive the REAL emitter, the REAL drain, the
// REAL settle-or-void logic, the REAL observation and a REAL HTTP round-trip (httptest) against a
// server that speaks billing's contract — but that server is a STAND-IN, and a stand-in can
// only ever prove what MY code does with a contract. It can NEVER prove the other house
// behaves as I modelled it.
//
// ⚠️ WHAT CHANGED, AND WHY IT MATTERS HERE (B-D13). The library no longer computes cost: `/settle`
// takes the INPUTS and billing prices them. So **Go cannot get the money law wrong any more,
// because Go no longer has one** — the arithmetic in `fakeBilling.price` belongs to the STAND-IN
// (i.e. to billing), not to the code under test. The only thing Go can still get wrong is the
// PAYLOAD, and that is what `TestBD13_*` pins.
//
// The other house is pinned in two places that are NOT doubles:
//   - the request contract: `conformance/testdata/settlement_contract_vectors_20260712.json` —
//     the exact bodies billing's REAL pydantic model accepts, and the cost its REAL `price_usage`
//     computes for each, both derived by EXECUTING billing (from Python, which can import it);
//   - the settlement end-to-end: the Python cross-house suite, against billing's real route on
//     real DynamoDB Local + real Redis.
//
// A Go <-> real-billing test remains **OWED** (DECISIONS B-D1). The vector table is a tripwire,
// not a certifier — do not let it quietly become one.
//
// The stand-in's status codes are transcribed from billing's REAL handler
// (app/api/billing.py -> app/core/reservation.py::settle_activation), and the mapping under test
// lives in quota/settler.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/observation"
	"github.com/ab0t-com/ab0t-quota-go/outbox"
	"github.com/shopspring/decimal"
)

// --- A stand-in for billing's /settle, with billing's REAL semantics ----------------------

// fakeBilling stands in for billing's /settle.
//
// ⚠️ NOTE WHAT MOVED (B-D13). It is BILLING that prices now, so it is THIS FAKE that owns the
// arithmetic — the library under test has none. That is the whole point: Go cannot get the money
// law wrong any more, because Go no longer has one. The only thing Go can still get wrong is the
// PAYLOAD, and that is what `lastPayload` exists to assert.
//
// Cost correctness against the REAL law is proven where it can be: Python's cross-house suite,
// against billing's REAL /settle on real infrastructure.
type fakeBilling struct {
	mu sync.Mutex
	// the DURABLE, NO-TTL dedup marker — billing's DynamoDB conditional write
	settled map[string]decimal.Decimal
	// per-org balance, so a double-charge is VISIBLE as money, not just as a call count
	balance map[string]decimal.Decimal
	calls   int
	// the last request body billing actually received — the contract under test
	lastPayload map[string]any
	// failure injection
	status  int  // if non-zero, always answer with this
	dropAck bool // land the settlement, then kill the response (the lost-ack case)
}

// price mirrors billing's price_usage: allocation_fee + ROUND_UP(rate * max(elapsed,60s)/3600).
// A MISSING RATE PRICES RUNTIME AT ZERO — billing does not invent $0.10/hr, and neither does its
// stand-in (B-D14 / D-36).
func (f *fakeBilling) price(started, stopped time.Time, rate, fee string) decimal.Decimal {
	elapsed := stopped.Sub(started).Seconds()
	if elapsed < 60 {
		elapsed = 60
	}
	r := decimal.Zero
	if rate != "" {
		r = decimal.RequireFromString(rate)
	}
	fe := decimal.Zero
	if fee != "" {
		fe = decimal.RequireFromString(fee)
	}
	hours := decimal.NewFromFloat(elapsed).DivRound(decimal.NewFromInt(3600), 24)
	return fe.Add(r.Mul(hours).RoundUp(6))
}

func newFakeBilling() *fakeBilling {
	return &fakeBilling{
		settled: map[string]decimal.Decimal{},
		balance: map[string]decimal.Decimal{},
	}
}

// payload returns the last request body billing received.
func (f *fakeBilling) payload() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPayload
}

func (f *fakeBilling) seed(org string, amount string) {
	f.balance[org] = decimal.RequireFromString(amount)
}

func (f *fakeBilling) balanceOf(org string) decimal.Decimal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.balance[org]
}

func (f *fakeBilling) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/billing/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++

		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"detail":"injected"}`))
			return
		}

		var raw map[string]any
		_ = json.NewDecoder(r.Body).Decode(&raw)
		f.lastPayload = raw

		var req struct {
			SettlementKey string `json:"settlement_key"`
			StartedAt     string `json:"started_at"`
			StoppedAt     string `json:"stopped_at"`
			HourlyRate    string `json:"hourly_rate"`
			AllocationFee string `json:"allocation_fee"`
			ReservationID string `json:"reservation_id"`
			UsageRecordID string `json:"usage_record_id"`
		}
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &req)

		// ⚠️ THE CALLER MUST NOT SEND A COST. If it ever does again, this fake refuses it —
		// a regression back to three implementations of one money law would otherwise be silent.
		if _, leaked := raw["actual_cost"]; leaked {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"detail":"actual_cost is not accepted; send the inputs (B-D13)"}`))
			return
		}

		// org is the path segment: /billing/{org}/settle
		org := ""
		if parts := splitPath(r.URL.Path); len(parts) >= 2 {
			org = parts[1]
		}

		// billing 404s an org with no billing account
		if _, ok := f.balance[org]; !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Billing account not found"}`))
			return
		}

		started, ok1 := observation.ParseTime(req.StartedAt)
		stopped, ok2 := observation.ParseTime(req.StoppedAt)
		if !ok1 || !ok2 || stopped.Before(started) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":"Settlement lifetime is invalid"}`))
			return
		}
		// BILLING prices it. The caller sent only what it observed.
		cost := f.price(started, stopped, req.HourlyRate, req.AllocationFee)
		if cost.IsNegative() {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":"Settlement cost cannot be negative"}`))
			return
		}

		replayed := false
		if _, dup := f.settled[req.SettlementKey]; dup {
			// THE DURABLE CONDITIONAL WRITE. A replay at ANY horizon moves NO money and
			// returns the original result. This is billing's exactly-once guarantee, and it
			// is what makes a client-side retry safe.
			replayed = true
		} else {
			f.settled[req.SettlementKey] = cost
			f.balance[org] = f.balance[org].Sub(cost)
		}

		if f.dropAck {
			// The settlement LANDED (above) and the response is now lost on the way back.
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
			}
			panic("cannot hijack to simulate a lost response")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":                         "settled",
			"settlement_key":                 req.SettlementKey,
			"org_id":                         org,
			"actual_cost":                    cost.String(),
			"new_balance":                    f.balance[org].String(),
			"spent_from_subscription_credit": "0",
			"spent_from_credit_balance":      "0",
			"spent_from_balance":             cost.String(),
			"settled_at":                     time.Now().UTC().Format(time.RFC3339),
			"replayed":                       replayed,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func splitPath(p string) []string {
	out := []string{}
	cur := ""
	for _, c := range p {
		if c == '/' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// --- The Settler under test: the SAME mapping quota/settler.go performs -------------------
//
// (quota/ cannot be imported from outbox_test without a cycle, so the classification is
// re-expressed here. quota/settler_test.go asserts the real one agrees with this table.)

type httpSettler struct {
	baseURL string
	http    *http.Client
}

func (s httpSettler) SettleActivation(ctx context.Context, in outbox.SettleRequest) (string, bool, error) {
	pl := in.Observation.Payload()
	m := map[string]string{
		"settlement_key":  in.SettlementKey,
		"started_at":      pl.StartedAt,
		"stopped_at":      pl.StoppedAt,
		"reservation_id":  in.ReservationID,
		"usage_record_id": in.UsageRecordID,
	}
	// Absence is sent as ABSENCE, never as null (B-D14).
	if pl.HourlyRate != "" {
		m["hourly_rate"] = pl.HourlyRate
	}
	if pl.AllocationFee != "" {
		m["allocation_fee"] = pl.AllocationFee
	}
	body, _ := json.Marshal(m)
	req, _ := http.NewRequestWithContext(ctx, "POST",
		s.baseURL+"/billing/"+in.OrgID+"/settle", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		// Connection refused / timeout / lost response — TRANSIENT. Never void.
		return "", false, outbox.ErrSettleTransient
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == 409:
		return "", false, outbox.ErrSettleRefused409
	case resp.StatusCode == 400, resp.StatusCode == 401, resp.StatusCode == 403,
		resp.StatusCode == 404, resp.StatusCode == 422:
		return "", false, outbox.ErrSettlePermanent
	case resp.StatusCode >= 500 || resp.StatusCode == 408 || resp.StatusCode == 429:
		return "", false, outbox.ErrSettleTransient
	}

	var out struct {
		ActualCost string `json:"actual_cost"`
		Replayed   bool   `json:"replayed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", false, outbox.ErrSettleTransient
	}
	// The cost is BILLING'S answer, read back. We did not compute it.
	return out.ActualCost, out.Replayed, nil
}

// --- Harness ------------------------------------------------------------------------------

func terminalEvent(org, rid string, hours int) []byte {
	now := time.Now().UTC()
	ev := map[string]any{
		"event_type":     "resource.stopped",
		"org_id":         org,
		"resource_id":    "sbx-1",
		"reservation_id": rid,
		"hourly_rate":    "1.00",
		"allocation_fee": "0",
		"started_at":     now.Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339),
		"stopped_at":     now.Format(time.RFC3339),
	}
	b, _ := json.Marshal(ev)
	return b
}

// pastHorizon builds an emitter whose every pending intent is already past its retry horizon —
// the scenario: the event outlived its window.
func pastHorizon(t *testing.T, store outbox.Store, s outbox.Settler) *outbox.Emitter {
	t.Helper()
	em := outbox.NewEmitter(store, nil, 0.0001, "void_and_alert")
	if s != nil {
		em.SetSettler(s)
	}
	return em
}

func stalePending(t *testing.T, store outbox.Store, rid, org string, hours int, eventType string) {
	t.Helper()
	ev := terminalEvent(org, rid, hours)
	if eventType != "resource.stopped" {
		var m map[string]any
		_ = json.Unmarshal(ev, &m)
		m["event_type"] = eventType
		ev, _ = json.Marshal(m)
	}
	_, err := store.PutIntent(context.Background(), outbox.Record{
		Key:           rid + ":" + eventType,
		Event:         ev,
		EventType:     eventType,
		ResourceType:  "sandbox",
		ReservationID: rid,
		FirstTS:       float64(time.Now().Add(-24 * time.Hour).Unix()), // a day old
	})
	if err != nil {
		t.Fatalf("put intent: %v", err)
	}
}

// =========================================================================================
// 1. THE RED TEST — the money is LOST today
// =========================================================================================

func TestD12_RED_todayThePastHorizonEventIsVoidedAndTheMoneyIsLost(t *testing.T) {
	fb := newFakeBilling()
	fb.seed("org-1", "100")
	store := outbox.NewInMemoryStore()
	stalePending(t, store, "res-1", "org-1", 3, "resource.stopped")

	// The emitter as it shipped BEFORE this ticket: no settler.
	em := pastHorizon(t, store, nil)
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(em.Voids()) != 1 {
		t.Fatalf("expected the event to be VOIDED (this IS today's path), got %d voids", len(em.Voids()))
	}
	if !fb.balanceOf("org-1").Equal(decimal.RequireFromString("100")) {
		t.Fatal("the fake billing should not even have been called")
	}
	if fb.calls != 0 {
		t.Fatalf("RED: the library never called billing at all — 3h x $1.00 of real usage was "+
			"voided and charged to nobody (calls=%d)", fb.calls)
	}
}

// =========================================================================================
// 2. THE FIX
// =========================================================================================

func TestD12_theSameEventNowSettles(t *testing.T) {
	fb := newFakeBilling()
	fb.seed("org-1", "100")
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()
	stalePending(t, store, "res-1", "org-1", 3, "resource.stopped")

	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(em.Voids()) != 0 {
		t.Fatalf("a settleable event must NEVER be voided: %+v", em.Voids())
	}
	settled := em.Settled()
	if len(settled) != 1 || settled[0].ReservationID != "res-1" {
		t.Fatalf("expected exactly one settlement, got %+v", settled)
	}
	if !fb.balanceOf("org-1").Equal(decimal.RequireFromString("97")) {
		t.Fatalf("3h x $1.00 must debit $3.00: balance=%s", fb.balanceOf("org-1"))
	}
	if n, _ := em.PendingCount(context.Background()); n != 0 {
		t.Fatalf("the durable intent must be DELIVERED, not left churning (pending=%d)", n)
	}
}

// =========================================================================================
// 3. EXACTLY ONCE
// =========================================================================================

func TestD12_aRedeliveredEventSettlesExactlyOnce(t *testing.T) {
	fb := newFakeBilling()
	fb.seed("org-1", "100")
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()

	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	for i := 0; i < 3; i++ {
		stalePending(t, store, "res-1", "org-1", 2, "resource.stopped") // redelivery
		if _, err := em.Drain(context.Background(), 10); err != nil {
			t.Fatalf("drain: %v", err)
		}
	}

	if !fb.balanceOf("org-1").Equal(decimal.RequireFromString("98")) {
		t.Fatalf("2h x $1.00 charged ONCE across three drains; balance=%s (a second debit here "+
			"is a customer double-charge)", fb.balanceOf("org-1"))
	}
	if len(em.Voids()) != 0 {
		t.Fatalf("unexpected voids: %+v", em.Voids())
	}
}

// =========================================================================================
// 4. THE FAIL DIRECTION
// =========================================================================================

func TestD12_billingUnreachableLeavesTheEventPendingNeverVoided(t *testing.T) {
	store := outbox.NewInMemoryStore()
	stalePending(t, store, "res-1", "org-1", 3, "resource.stopped")

	// A settler pointed at a dead address: connection refused.
	dead := httpSettler{
		baseURL: "http://127.0.0.1:1",
		http:    &http.Client{Timeout: 500 * time.Millisecond},
	}
	em := pastHorizon(t, store, dead)
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(em.Voids()) != 0 {
		t.Fatalf("FAIL DIRECTION VIOLATED: a transient outage VOIDED a money event — a 503 must "+
			"never consume revenue. voids=%+v", em.Voids())
	}
	if em.SettleFailures() != 1 {
		t.Fatalf("expected 1 transient failure, got %d", em.SettleFailures())
	}
	if n, _ := em.PendingCount(context.Background()); n != 1 {
		t.Fatalf("the event must be RETAINED for retry, not discarded (pending=%d)", n)
	}

	// Billing recovers → the SAME retained event settles. Nothing was lost.
	fb := newFakeBilling()
	fb.seed("org-1", "100")
	srv := fb.server(t)
	em2 := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	if _, err := em2.Drain(context.Background(), 10); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !fb.balanceOf("org-1").Equal(decimal.RequireFromString("97")) {
		t.Fatalf("the event should have survived the outage and settled: balance=%s",
			fb.balanceOf("org-1"))
	}
}

func TestD12_a5xxIsRetriedNotVoided(t *testing.T) {
	fb := newFakeBilling()
	fb.seed("org-1", "100")
	fb.status = 500
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()
	stalePending(t, store, "res-1", "org-1", 3, "resource.stopped")

	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(em.Voids()) != 0 {
		t.Fatalf("a 5xx must NOT void a money event: %+v", em.Voids())
	}
	if n, _ := em.PendingCount(context.Background()); n != 1 {
		t.Fatalf("a 5xx must leave the event PENDING for retry (pending=%d)", n)
	}
}

func TestD12_aLostResponseWhoseSettlementLandedDoesNotDoubleCharge(t *testing.T) {
	// The cruellest case: the request REACHES billing, the settlement REALLY LANDS, and the
	// response is lost. The caller sees a transport error and CANNOT KNOW.
	fb := newFakeBilling()
	fb.seed("org-1", "100")
	fb.dropAck = true
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()
	stalePending(t, store, "res-1", "org-1", 3, "resource.stopped")

	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// The library believes it failed, and correctly RETAINS the event rather than voiding it.
	if len(em.Voids()) != 0 {
		t.Fatalf("a lost response must not void: %+v", em.Voids())
	}
	if n, _ := em.PendingCount(context.Background()); n != 1 {
		t.Fatalf("the event must be retained (pending=%d)", n)
	}
	// But the money DID move — once.
	if !fb.balanceOf("org-1").Equal(decimal.RequireFromString("97")) {
		t.Fatalf("the settlement landed server-side: balance=%s", fb.balanceOf("org-1"))
	}

	// The retry now runs against a healthy billing. It must NOT charge a second time.
	fb.dropAck = false
	em2 := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	if _, err := em2.Drain(context.Background(), 10); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !fb.balanceOf("org-1").Equal(decimal.RequireFromString("97")) {
		t.Fatalf("DOUBLE CHARGE on a retry after a lost response: balance=%s. The durable "+
			"marker must have recognised the replay.", fb.balanceOf("org-1"))
	}
	if s := em2.Settled(); len(s) != 1 || !s[0].Replayed {
		t.Fatalf("billing must report this as a REPLAY, not a fresh settlement: %+v", s)
	}
}

// =========================================================================================
// 5. THE TERMINAL-EVENT GATE
// =========================================================================================

func TestD12_aNonTerminalEventIsVoidedNeverSettled(t *testing.T) {
	for _, et := range []string{"resource.started", "resource.heartbeat"} {
		t.Run(et, func(t *testing.T) {
			fb := newFakeBilling()
			fb.seed("org-1", "100")
			srv := fb.server(t)
			store := outbox.NewInMemoryStore()
			stalePending(t, store, "res-1", "org-1", 3, et)

			em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
			if _, err := em.Drain(context.Background(), 10); err != nil {
				t.Fatalf("drain: %v", err)
			}

			if len(em.Settled()) != 0 {
				t.Fatalf("%s must NEVER settle — it has no final cost", et)
			}
			if len(em.Voids()) != 1 {
				t.Fatalf("%s past its horizon must be VOIDED, got %d voids", et, len(em.Voids()))
			}
			if !fb.balanceOf("org-1").Equal(decimal.RequireFromString("100")) {
				t.Fatalf("no money may move for %s: balance=%s", et, fb.balanceOf("org-1"))
			}

			// THE KEY WAS NOT BURNED: the real terminal event still settles in full.
			stalePending(t, store, "res-1", "org-1", 3, "resource.stopped")
			if _, err := em.Drain(context.Background(), 10); err != nil {
				t.Fatalf("drain: %v", err)
			}
			if !fb.balanceOf("org-1").Equal(decimal.RequireFromString("97")) {
				t.Fatalf("the terminal settlement was REFUSED as a duplicate — the %s event "+
					"burned the settlement key. balance=%s", et, fb.balanceOf("org-1"))
			}
		})
	}
}

// =========================================================================================
// 6. THE VOID PATH SURVIVES, for what genuinely cannot settle
// =========================================================================================

func TestD12_anEventWithNoOrgIdIsStillVoidedAndAlerted(t *testing.T) {
	fb := newFakeBilling()
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()

	ev, _ := json.Marshal(map[string]any{
		"event_type": "resource.stopped", "org_id": "", "resource_id": "sbx-1",
		"reservation_id": "res-1", "hourly_rate": "1.00", "allocation_fee": "0",
		"started_at": time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339),
		"stopped_at": time.Now().UTC().Format(time.RFC3339),
	})
	_, _ = store.PutIntent(context.Background(), outbox.Record{
		Key: "res-1:resource.stopped", Event: ev, EventType: "resource.stopped",
		ResourceType: "sandbox", ReservationID: "res-1",
		FirstTS: float64(time.Now().Add(-24 * time.Hour).Unix()),
	})

	var alerted []outbox.VoidEntry
	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	em.OnVoid = func(v outbox.VoidEntry) { alerted = append(alerted, v) }
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(em.Settled()) != 0 {
		t.Fatal("unattributable usage must never settle")
	}
	if len(em.Voids()) != 1 {
		t.Fatalf("genuinely unsettleable → voided, got %d", len(em.Voids()))
	}
	if len(alerted) != 1 {
		t.Fatal("A HUMAN MUST HEAR IT: the void alert sink was not called")
	}
}

func TestD12_anUnknownOrg404IsVoidedNotRetriedForever(t *testing.T) {
	fb := newFakeBilling() // no account seeded → billing 404s
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()
	stalePending(t, store, "res-1", "org-ghost", 3, "resource.stopped")

	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(em.Voids()) != 1 {
		t.Fatalf("a 404 is PERMANENT: void + alert, do not churn. voids=%d", len(em.Voids()))
	}
	if em.SettleFailures() != 0 {
		t.Fatalf("a 404 must NOT be counted as a transient failure (got %d)", em.SettleFailures())
	}
}

// TestD12_a409IsAMBIGUOUS_andMustNOTbeAckedAsSettled
//
// ⚠️ THIS TEST WAS INVERTED ON 2026-07-12, AND THE INVERSION IS THE POINT.
//
// It was originally `TestD12_a409AlreadyAccountedIsSuccessNotALoss`, and it asserted that a 409
// was acked as success ("the books are correct"). **That was a revenue-loss bug**, and this test
// was pinning it. Billing returns ONE OPAQUE 409 by design — distinct codes would build a
// cross-tenant enumeration oracle, because its precheck reads Redis BEFORE it checks tenancy —
// and that single code covers:
//
//   - "reservation_still_live:use_commit"   -> THE MONEY IS NOT TAKEN
//   - "org_mismatch"                        -> not ours; nothing settled
//   - "already_committed:ledger_row_exists" -> the money IS booked
//
// Two of the three mean the settlement did NOT land. Acking them retired the durable outbox row
// and DISCARDED the revenue — D-12's loss, re-entering through the ERROR CONTRACT.
//
// Ambiguity is not success (D-49). The event must RETRY. Retrying a genuinely-settled event is
// free — billing's dedup is a durable conditional write.
func TestD12_a409IsAMBIGUOUS_andMustNOTbeAckedAsSettled(t *testing.T) {
	fb := newFakeBilling()
	fb.seed("org-1", "100")
	fb.status = 409 // billing: "not eligible" — could be ANY of the three worlds above
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()
	stalePending(t, store, "res-1", "org-1", 3, "resource.stopped")

	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if n, _ := em.PendingCount(context.Background()); n != 1 {
		t.Fatalf("REVENUE LOST: a 409 was acked as 'settled' and the durable intent was retired "+
			"(pending=%d). That 409 may have meant 'the reservation is still live' — the money "+
			"has NOT been taken. If the commit never lands (D-12's whole premise), this usage is "+
			"billed to NOBODY and the row that would have recovered it is gone.", n)
	}
	if len(em.Voids()) != 0 {
		t.Fatalf("a 409 must not be VOIDED either — the money may yet be owed; we simply cannot "+
			"confirm. voids=%+v", em.Voids())
	}
	if len(em.Settled()) != 0 {
		t.Fatal("nothing settled — billing refused it")
	}
	if em.SettleFailures() != 1 {
		t.Fatalf("the ambiguous refusal must be counted as a failure, not a success (got %d)",
			em.SettleFailures())
	}
}

// =========================================================================================
// 7. The sub-minute floor, through the settle path (the vector NC-5 proved a suite can miss)
// =========================================================================================

func TestD12_aSubMinuteLifetimeSettlesAtBillings60SecondFloor(t *testing.T) {
	fb := newFakeBilling()
	fb.seed("org-1", "100")
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()

	now := time.Now().UTC()
	ev, _ := json.Marshal(map[string]any{
		"event_type": "resource.stopped", "org_id": "org-1", "resource_id": "sbx-1",
		"reservation_id": "res-1", "hourly_rate": "1.00", "allocation_fee": "0",
		"started_at": now.Add(-30 * time.Second).Format(time.RFC3339),
		"stopped_at": now.Format(time.RFC3339),
	})
	_, _ = store.PutIntent(context.Background(), outbox.Record{
		Key: "res-1:resource.stopped", Event: ev, EventType: "resource.stopped",
		ResourceType: "sandbox", ReservationID: "res-1",
		FirstTS: float64(time.Now().Add(-24 * time.Hour).Unix()),
	})

	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// 60s floor at $1.00/h, ROUND_UP to 1e-6 → 0.016667. NOT the literal 30s (0.008333).
	want := decimal.RequireFromString("100").Sub(decimal.RequireFromString("0.016667"))
	if !fb.balanceOf("org-1").Equal(want) {
		t.Fatalf("a 30s lifetime must settle at billing's 60-SECOND FLOOR: balance=%s want=%s. "+
			"Charging literal elapsed time UNDERCHARGES and disagrees with what /commit would "+
			"have taken for identical usage.", fb.balanceOf("org-1"), want)
	}
}

// =========================================================================================
// 8. B-D13 — THE LIBRARY SENDS THE INPUTS AND COMPUTES NO COST
// =========================================================================================

// TestBD13_theLibrarySendsINPUTSandNeverACost is the regression guard for the whole B-D13
// migration. The library used to send a pre-computed `actual_cost`, which forced it to carry a
// port of billing's proration. If a cost ever reappears on this wire, three implementations of
// one money law are back — silently.
func TestBD13_theLibrarySendsINPUTSandNeverACost(t *testing.T) {
	fb := newFakeBilling()
	fb.seed("org-1", "100")
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()
	stalePending(t, store, "res-1", "org-1", 3, "resource.stopped")

	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatalf("drain: %v", err)
	}

	pl := fb.payload()
	if pl == nil {
		t.Fatal("billing received no request at all")
	}
	if _, leaked := pl["actual_cost"]; leaked {
		t.Fatal("THE LIBRARY SENT A COST. B-D13 is regressed: billing prices usage, the caller " +
			"reports it. A caller that can compute a cost can compute it WRONG.")
	}
	for _, required := range []string{"started_at", "stopped_at", "settlement_key", "reservation_id"} {
		if _, ok := pl[required]; !ok {
			t.Fatalf("the settlement payload is missing %q — billing cannot price what it is not told", required)
		}
	}
	if pl["hourly_rate"] != "1.00" {
		t.Fatalf("the observed rate must be forwarded verbatim, got %v", pl["hourly_rate"])
	}
	// And the cost we recorded is BILLING'S answer, not ours.
	settled := em.Settled()
	if len(settled) != 1 || settled[0].ActualCost != "3" {
		t.Fatalf("the settled mirror must record what BILLING charged, got %+v", settled)
	}
}

// TestBD13_aMissingRateIsOMITTED_notNull_andNeverInvented guards BOTH traps at once:
//
//	(1) the library must not INVENT a price for a rate-less event (a fabricated price is worse
//	    than no price — billing prices runtime at ZERO and alerts, in exactly one place);
//	(2) it must send the absence as an ABSENT KEY, not an explicit null — B-D14 was an
//	    always-present key whose value was sometimes null, against a `.get(k, default)` that
//	    only defaults on an absent key. Do not build the next landmine.
func TestBD13_aMissingRateIsOMITTED_notNull_andNeverInvented(t *testing.T) {
	fb := newFakeBilling()
	fb.seed("org-1", "100")
	srv := fb.server(t)
	store := outbox.NewInMemoryStore()

	now := time.Now().UTC()
	ev, _ := json.Marshal(map[string]any{
		"event_type": "resource.stopped", "org_id": "org-1", "resource_id": "sbx-1",
		"reservation_id": "res-1",
		"hourly_rate":    nil, // the rate-less event: pricing config has a hole
		"allocation_fee": nil,
		"started_at":     now.Add(-3 * time.Hour).Format(time.RFC3339),
		"stopped_at":     now.Format(time.RFC3339),
	})
	_, _ = store.PutIntent(context.Background(), outbox.Record{
		Key: "res-1:resource.stopped", Event: ev, EventType: "resource.stopped",
		ResourceType: "sandbox", ReservationID: "res-1",
		FirstTS: float64(time.Now().Add(-24 * time.Hour).Unix()),
	})

	em := pastHorizon(t, store, httpSettler{baseURL: srv.URL, http: srv.Client()})
	if _, err := em.Drain(context.Background(), 10); err != nil {
		t.Fatalf("drain: %v", err)
	}

	pl := fb.payload()
	if v, present := pl["hourly_rate"]; present {
		t.Fatalf("a missing rate must be OMITTED, not sent as %v (%T). An always-present key "+
			"whose value is sometimes null is exactly the B-D14 landmine, against any "+
			"`.get(k, default)` on the other side.", v, v)
	}
	// It still SETTLES — a rate-less event is a pricing-config gap, not an unsettleable event.
	// The runtime prices at ZERO (billing does not invent $0.10/hr) and billing alerts.
	if len(em.Voids()) != 0 {
		t.Fatalf("a rate-less event must still SETTLE (at zero runtime), not be voided: %+v", em.Voids())
	}
	if !fb.balanceOf("org-1").Equal(decimal.RequireFromString("100")) {
		t.Fatalf("runtime with no rate costs ZERO — the library must NOT have invented a rate. "+
			"balance=%s", fb.balanceOf("org-1"))
	}
}

// TestBD13_theLibraryHasNoProrationLeft is a structural guard: if anything in the library starts
// computing money again, it will need the arithmetic, and the arithmetic is gone.
func TestBD13_theLibraryHasNoProrationLeft(t *testing.T) {
	// observation.Observe returns no cost — it cannot, there is no field for one.
	obs, err := observation.Observe(observation.Event{
		OrgID: "o", ResourceID: "r", ReservationID: "res",
		HourlyRate: "1.00", AllocationFee: "0",
		StartedAt: "2026-07-01T12:00:00Z", StoppedAt: "2026-07-01T15:00:00Z",
	})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	pl := obs.Payload()
	if pl.StartedAt == "" || pl.StoppedAt == "" || pl.HourlyRate != "1.00" {
		t.Fatalf("the observation must carry the INPUTS verbatim: %+v", pl)
	}
	// There is no Cost field to assert on. That is the guarantee.
}
