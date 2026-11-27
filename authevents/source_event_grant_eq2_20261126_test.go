package authevents

// Sign-up gift keyed on the source event (v0.2.0, MONEY): one auth event → at most one sign-up gift under ANY dedup rule.
// The default handler's business dedup was keyed on the RULE only; its delivery dedup rows expire (the
// Redis ledger keeps them 72 h) while business-dedup rows never do — so after a company changes
// credit_grant.dedup, an old event re-handled past the delivery window got a NEW rule key and a SECOND
// gift. Now the grant is also keyed on the source event (SourceEventGrantKey, written on success).
// Same key as the Python library (compose_source_event_grant_key).

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
)

// lostDeliveryRows keeps business-dedup rows in a shared store but delivery rows in its own store — a
// stand-in for the delivery window having passed (Redis delivery rows: 72 h; business rows: no TTL).
type lostDeliveryRows struct {
	handlerledger.LedgerStore // delivery rows (fresh per "redelivery")
	biz                       handlerledger.LedgerStore
}

func (s lostDeliveryRows) AlreadyDone(ctx context.Context, k string) (bool, error) {
	return s.biz.AlreadyDone(ctx, k)
}
func (s lostDeliveryRows) MarkDone(ctx context.Context, in handlerledger.MarkDoneInput) error {
	return s.biz.MarkDone(ctx, in)
}

func eq2Event(t *testing.T, body string) Event {
	t.Helper()
	var ev Event
	if err := json.Unmarshal([]byte(body), &ev); err != nil {
		t.Fatal(err)
	}
	ev.SetRaw([]byte(body))
	return ev
}

func eq2Handle(t *testing.T, dedup config.DedupPolicy, gr *stubGranter, biz handlerledger.LedgerStore, ev Event) {
	t.Helper()
	cfg := newCreditCfg(t)
	cfg.Tiers[0].CreditGrant.Dedup = dedup
	h, err := BuildDefaultCreditGrantHandler(CreditGrantDeps{Config: cfg, TierProvider: stubTierProvider{tier: "pro"}, Granter: gr})
	if err != nil {
		t.Fatal(err)
	}
	store := lostDeliveryRows{LedgerStore: handlerledger.NewInMemoryLedgerStore(), biz: biz}
	if err := handlerledger.Dispatch(context.Background(), h.Inner(), ev, store); err != nil {
		var skip *handlerledger.SkipError
		var ok *handlerledger.SuccessError
		if !errors.As(err, &skip) && !errors.As(err, &ok) {
			t.Fatalf("dispatch: %v", err)
		}
	}
}

const eq2Body = `{"event_type":"org.created","event_id":"evt-signup-1","data":{"user_id":"u1","org_id":"o1"}}`

func TestDefaultCreditGrant_RuleChangeDoesNotRegrantTheSameEvent_EQ2(t *testing.T) {
	gr := &stubGranter{}
	biz := handlerledger.NewInMemoryLedgerStore()
	eq2Handle(t, config.DedupPerUserPerTier, gr, biz, eq2Event(t, eq2Body))
	if gr.calls.Load() != 1 {
		t.Fatalf("first handling: %d grants", gr.calls.Load())
	}
	eq2Handle(t, config.DedupPerOrgGlobal, gr, biz, eq2Event(t, eq2Body)) // the rule changed; same event
	if gr.calls.Load() != 1 {
		t.Fatalf("RED before: the new rule's key granted the same event a second time (%d grants)", gr.calls.Load())
	}
	if done, _ := biz.AlreadyDone(context.Background(), SourceEventGrantKey("evt-signup-1")); !done {
		t.Fatal("the source-event row was not written")
	}
}

// Control: a NEW event follows the new rule. v0.3.1 (once per person per company, whatever the rule):
// a new event for the SAME person gives nothing more; a new event for a new person follows the new rule.
func TestDefaultCreditGrant_NewEventFollowsTheNewRule_EQ2(t *testing.T) {
	gr := &stubGranter{}
	biz := handlerledger.NewInMemoryLedgerStore()
	eq2Handle(t, config.DedupPerUserPerTier, gr, biz, eq2Event(t, eq2Body))
	eq2Handle(t, config.DedupPerOrgGlobal, gr, biz,
		eq2Event(t, `{"event_type":"org.created","event_id":"evt-signup-2","data":{"user_id":"u1","org_id":"o1"}}`))
	if gr.calls.Load() != 1 {
		t.Fatalf("v0.3.1: the same person got %d gifts, want 1", gr.calls.Load())
	}
	eq2Handle(t, config.DedupPerOrgGlobal, gr, biz,
		eq2Event(t, `{"event_type":"org.created","event_id":"evt-signup-3","data":{"user_id":"u2","org_id":"o1"}}`))
	if gr.calls.Load() != 2 || gr.last.IdempotencyKey != BillingIdempotencyKey(string(config.DedupPerOrgGlobal), "u2", "o1", "pro") {
		t.Fatalf("grants %d, last key %q", gr.calls.Load(), gr.last.IdempotencyKey)
	}
}

// Control: a failed grant does not mark the event — the corrected redelivery grants.
func TestDefaultCreditGrant_FailedGrantDoesNotMarkTheEvent_EQ2(t *testing.T) {
	gr := &stubGranter{err: errors.New("billing 503")}
	biz := handlerledger.NewInMemoryLedgerStore()
	cfg := newCreditCfg(t)
	h, err := BuildDefaultCreditGrantHandler(CreditGrantDeps{Config: cfg, TierProvider: stubTierProvider{tier: "pro"}, Granter: gr})
	if err != nil {
		t.Fatal(err)
	}
	h.Inner().Config.Retry = handlerledger.NoRetry // no backoff sleeps in the test
	_ = handlerledger.Dispatch(context.Background(), h.Inner(), eq2Event(t, eq2Body),
		lostDeliveryRows{LedgerStore: handlerledger.NewInMemoryLedgerStore(), biz: biz})
	if done, _ := biz.AlreadyDone(context.Background(), SourceEventGrantKey("evt-signup-1")); done {
		t.Fatal("a failed grant marked the source event")
	}
	gr.err = nil
	eq2Handle(t, config.DedupPerUserPerTier, gr, biz, eq2Event(t, eq2Body))
	if gr.calls.Load() != 1 {
		t.Fatalf("corrected redelivery: %d grants, want 1", gr.calls.Load())
	}
}

func TestSourceEventGrantKey_Shape_EQ2(t *testing.T) {
	if SourceEventGrantKey("evt-1") != "credit_grant_event:evt-1" {
		t.Fatal(SourceEventGrantKey("evt-1"))
	}
}
