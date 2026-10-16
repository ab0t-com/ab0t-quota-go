package authevents

// Ticket 20261016 review, finding REV-01 (money).
//
// The default credit-grant handler runs on org.created and user.org_assigned —
// i.e. it is the SIGN-UP grant. v0.1.9 granted any tier's credit_grant regardless
// of its trigger, so a paid tier whose credit_grant is the monthly refill
// (trigger "subscription_invoice_paid" — REQUIRED by billing_model
// "subscription_with_credits", config/tier.go:236-240) was ALSO granted as a
// free gift every time a user was assigned to an org on that tier (dedup
// per_user_per_tier → once per joining user). The Python runtime grants only
// trigger == "signup" (ab0t_quota/auth_events.py:608-610). RED on v0.1.9.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
	"github.com/shopspring/decimal"
)

func cfgWithTrigger(trigger config.CreditTrigger) *config.Config {
	return &config.Config{Tiers: []config.Tier{{
		TierID: "pro",
		CreditGrant: &config.CreditGrant{
			Trigger:         trigger,
			AmountPerPeriod: config.Decimal{Decimal: decimal.NewFromInt(10)},
			Currency:        "USD",
			Lifecycle:       config.CreditLifecycleUseItOrLoseIt,
			Destination:     config.CreditDestSubscriptionCredit,
			Dedup:           config.DedupPerUserPerTier,
		},
	}}}
}

func fireAuthEvent(t *testing.T, cfg *config.Config, eventType string) *stubGranter {
	t.Helper()
	store := handlerledger.NewInMemoryLedgerStore()
	gr := &stubGranter{}
	h, err := BuildDefaultCreditGrantHandler(CreditGrantDeps{
		Config: cfg, TierProvider: stubTierProvider{tier: "pro"}, Granter: gr, Ledger: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	reg.OnAuthEvent(eventType, h)
	srv := MakeRouter(ReceiverConfig{Secret: "shh", Registry: reg, LedgerStore: store})
	body := []byte(`{"event_type":"` + eventType + `","event_id":"e-` + eventType +
		`","data":{"user_id":"u-joiner","org_id":"o-paying"}}`)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, signedPost(t, WebhookPath, body, "shh"))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s", rec.Code, rec.Body.String())
	}
	return gr
}

func TestDefaultCreditGrant_OnlySignupTriggerGrants_20261016(t *testing.T) {
	nonSignup := []config.CreditTrigger{
		config.CreditTriggerSubscriptionInvoicePaid,
		config.CreditTriggerScheduledPeriodStart,
		config.CreditTriggerManual,
		config.CreditTriggerWebhookAdmin,
	}
	for _, trig := range nonSignup {
		for _, ev := range []string{"org.created", "user.org_assigned"} {
			if gr := fireAuthEvent(t, cfgWithTrigger(trig), ev); gr.calls.Load() != 0 {
				t.Errorf("trigger %q on %s: granted %d time(s) (amount %s) — an auth event is a SIGN-UP; "+
					"only trigger \"signup\" may grant here", trig, ev, gr.calls.Load(), gr.last.Amount)
			}
		}
	}
	// The sign-up grant still fires.
	if gr := fireAuthEvent(t, cfgWithTrigger(config.CreditTriggerSignup), "org.created"); gr.calls.Load() != 1 {
		t.Fatalf("trigger signup: calls = %d, want 1", gr.calls.Load())
	}
}
