package billing

// Org-level billing.funds_low (v0.2.0): billing may send funds_low with trigger=balance_threshold
// and NO reservation_id (an org crossed its low-balance threshold). It must parse, reach OnLow with
// an empty reservation id, dedup on the org key (the Python dedup_key's org fallback) and never be an
// error or "malformed".

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
)

// billing's alert-worker payload shape (no reservation_id key at all).
const orgLow = `{"event_type":"billing.funds_low","trigger":"balance_threshold","org_id":"org-1",` +
	`"timestamp":"2026-11-23T10:00:00+00:00","state":"low","available":"4.50","threshold":"5.00",` +
	`"balance":"4.50","credit_balance":"0","subscription_credit":"0","reserved":"0","balance_source":"redis"}`

func TestParseFundsEvent_OrgLevelFundsLow_20261123(t *testing.T) {
	for name, msg := range map[string][]byte{"raw": []byte(orgLow), "sns": snsEnvelope(orgLow), "sqs": sqsEntry(orgLow)} {
		ev, err := ParseFundsEvent(msg)
		if err != nil || ev == nil {
			t.Fatalf("%s: an org-level funds_low must parse, got %v %v", name, ev, err)
		}
		if ev.Kind != FundsLow || ev.Trigger != TriggerBalanceThreshold || ev.OrgID != "org-1" ||
			ev.ReservationID != "" || !ev.IsOrgLevel() || ev.IsRunningResource() || ev.IsDebt() {
			t.Fatalf("%s: %+v", name, ev)
		}
		if ev.Available == nil || !ev.Available.Equal(decimal.RequireFromString("4.50")) ||
			ev.Threshold == nil || !ev.Threshold.Equal(decimal.RequireFromString("5")) {
			t.Fatalf("%s: available/threshold: %+v", name, ev)
		}
		if ev.DedupKey() != "org:org-1|billing.funds_low|balance_threshold" {
			t.Fatalf("%s: dedup key %q — the org fallback of billing's (reservation_id, event_type, trigger)", name, ev.DedupKey())
		}
	}
	// Control: a resource funds_low is NOT org level and has no threshold.
	ev, err := ParseFundsEvent([]byte(hbLow))
	if err != nil || ev.IsOrgLevel() || ev.Threshold != nil {
		t.Fatalf("control: heartbeat funds_low read as org level: %+v %v", ev, err)
	}
}

func TestFundsEventHandler_OrgLevelReachesOnLow_20261123(t *testing.T) {
	var got []FundsEvent
	h := &FundsEventHandler{OnLow: func(_ context.Context, ev FundsEvent) error { got = append(got, ev); return nil }}
	res, err := h.Handle(context.Background(), snsEnvelope(orgLow))
	if err != nil || res != DispatchHandled {
		t.Fatalf("org-level funds_low: %s %v", res, err)
	}
	if len(got) != 1 || got[0].ReservationID != "" || got[0].Trigger != TriggerBalanceThreshold {
		t.Fatalf("hook got %+v", got)
	}
	// at-least-once redelivery of the same crossing → deduped
	if res, _ := h.Handle(context.Background(), snsEnvelope(orgLow)); res != DispatchDuplicate || len(got) != 1 {
		t.Fatalf("redelivery: %s (hook calls %d)", res, len(got))
	}
	// the org key is per org: another org's crossing is delivered
	other := `{"event_type":"billing.funds_low","trigger":"balance_threshold","org_id":"org-2","available":"1","threshold":"2"}`
	if res, _ := h.Handle(context.Background(), []byte(other)); res != DispatchHandled || len(got) != 2 {
		t.Fatalf("other org: %s (hook calls %d)", res, len(got))
	}
	// with no OnLow hook: loud log, DispatchNoHook, never an error
	bare := &FundsEventHandler{}
	if res, err := bare.Handle(context.Background(), []byte(orgLow)); err != nil || res != DispatchNoHook {
		t.Fatalf("no hook: %s %v", res, err)
	}
}
