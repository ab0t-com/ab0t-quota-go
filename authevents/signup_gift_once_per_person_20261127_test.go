package authevents

// v0.3.1 — the sign-up gift is once per person, per company, whatever the company's rule (MONEY).
// The gift's "already given" markers (the ledger row and the key sent to billing) were derived from
// the rule (credit_grant.dedup) and the person's plan alone, so a company that changed its rule — or a
// person whose plan changed under a per-plan rule — got keys that matched none of the old markers, and
// an existing customer whose sign-up was handled again got the gift a SECOND time.
//
// Vectors: testdata/signup_gift_key_golden_20261127.json, a byte-identical copy of the Python
// library's (the key vectors: signup_gift_keys_20261127_test.go). RED before v0.3.1: the scenarios
// the_rule_changes…, the_plan_changes…, gifted_by_an_earlier_release_then_the_rule_changes,
// signed_up_before_the_company_opted_in… and two_companies… grant twice. CONTROLS (green before and
// after): gifted_by_an_earlier_release_under_the_same_rule, a_join_then_the_sign_up…, every new
// person's step.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
)

type signupGiftStep struct {
	Company        string `json:"company"`
	Handler        string `json:"handler"` // "signup" | "joined"
	EventID        string `json:"event_id"`
	UserID         string `json:"user_id"`
	OrgID          string `json:"org_id"`
	WorkspaceOrgID string `json:"workspace_org_id"`
	Tier           string `json:"tier"`
	Policy         string `json:"policy"`
	OptedIn        bool   `json:"opted_in"`
	Gift           bool   `json:"gift"`
	BillingOrg     string `json:"billing_org"`
	BillingKey     string `json:"billing_key"`
}

// signupGiftScenarios reads only the scenarios (this file uses no v0.3.1 API, so it also runs, RED,
// against the v0.3.0 handler — see the keys file for the v0.3.1 helper's vectors).
type signupGiftScenarios struct {
	Scenarios []struct {
		Name    string `json:"name"`
		Earlier []struct {
			Company    string `json:"company"`
			Policy     string `json:"policy"`
			UserID     string `json:"user_id"`
			BillingOrg string `json:"billing_org"`
			Tier       string `json:"tier"`
		} `json:"gifted_by_an_earlier_release"`
		Steps []signupGiftStep `json:"steps"`
	} `json:"scenarios"`
}

func loadSignupGiftScenarios(t *testing.T) signupGiftScenarios {
	t.Helper()
	raw, err := os.ReadFile("../testdata/signup_gift_key_golden_20261127.json")
	if err != nil {
		t.Fatal(err)
	}
	var g signupGiftScenarios
	if err := json.Unmarshal(raw, &g); err != nil || len(g.Scenarios) == 0 {
		t.Fatalf("golden: %v", err)
	}
	return g
}

func signupGiftCfg(t *testing.T, policy string) *config.Config {
	t.Helper()
	cfg := twoTierCfg(t) // tiers "pro" and "free", each with its own sign-up grant
	for i := range cfg.Tiers {
		cfg.Tiers[i].CreditGrant.Dedup = config.DedupPolicy(policy)
	}
	return cfg
}

func TestSignupGift_SharedScenariosThroughTheLibrarysHandlers_20261127(t *testing.T) {
	ctx := context.Background()
	for _, sc := range loadSignupGiftScenarios(t).Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			gr := &stubGranter{}
			biz := map[string]handlerledger.LedgerStore{} // one durable ledger per company
			ledger := func(company string) handlerledger.LedgerStore {
				if biz[company] == nil {
					biz[company] = handlerledger.NewInMemoryLedgerStore()
				}
				return biz[company]
			}
			for _, e := range sc.Earlier {
				// What v0.3.0 left after a successful gift: the ledger row under the rule's dedup key.
				if err := ledger(e.Company).MarkDone(ctx, handlerledger.MarkDoneInput{
					DedupKey:      ComposeCreditDedupKey(e.Policy, e.UserID, e.BillingOrg, e.Tier),
					SourceHandler: "default_credit_grant", SourceEventID: "earlier-release",
				}); err != nil {
					t.Fatal(err)
				}
			}
			for i, st := range sc.Steps {
				tier := st.Tier
				deps := CreditGrantDeps{Config: signupGiftCfg(t, st.Policy), TierProvider: tieredStub{tier: &tier},
					Granter: gr, MemberJoinedEnabled: st.OptedIn}
				build, eventType := BuildDefaultCreditGrantHandler, "org.created"
				if st.Handler == "joined" {
					build, eventType = BuildMemberJoinedCreditGrantHandler, EventMemberJoined
				}
				h, err := build(deps)
				if err != nil {
					t.Fatal(err)
				}
				h.Inner().Config.Retry = handlerledger.NoRetry
				before := gr.calls.Load()
				// A fresh delivery store each step: the sign-up is handled again past the delivery window.
				store := lostDeliveryRows{LedgerStore: handlerledger.NewInMemoryLedgerStore(), biz: ledger(st.Company)}
				err = handlerledger.Dispatch(ctx, h.Inner(), mjEvent(t, eventType, st.EventID, st.UserID, st.OrgID, st.WorkspaceOrgID), store)
				var skip *handlerledger.SkipError
				var ok *handlerledger.SuccessError
				if err != nil && !errors.As(err, &skip) && !errors.As(err, &ok) {
					t.Fatalf("step %d (%s): %v", i, st.EventID, err)
				}
				if st.Gift {
					if gr.calls.Load() != before+1 {
						t.Fatalf("step %d (%s): no gift for a new person", i, st.EventID)
					}
					if gr.last.OrgID != st.BillingOrg || gr.last.IdempotencyKey != st.BillingKey {
						t.Fatalf("step %d: gift on %q key %q, want %q %q", i, gr.last.OrgID, gr.last.IdempotencyKey, st.BillingOrg, st.BillingKey)
					}
				} else if gr.calls.Load() != before {
					t.Fatalf("step %d (%s): a SECOND gift for %s at %s under rule %s / plan %s",
						i, st.EventID, st.UserID, st.Company, st.Policy, st.Tier)
				}
			}
		})
	}
}

// failingGiftChecks answers the source-event check but cannot read any "already gifted" marker.
type failingGiftChecks struct{ handlerledger.LedgerStore }

func (f failingGiftChecks) AlreadyDone(ctx context.Context, k string) (bool, error) {
	if strings.HasPrefix(k, "credit_grant_event:") {
		return f.LedgerStore.AlreadyDone(ctx, k)
	}
	return false, errors.New("ledger unavailable (simulated)")
}

func TestSignupGift_UnreadableLedgerFailsClosed_20261127(t *testing.T) {
	gr := &stubGranter{}
	tier := "free"
	h, err := BuildDefaultCreditGrantHandler(CreditGrantDeps{Config: signupGiftCfg(t, "per_user_per_tier"),
		TierProvider: tieredStub{tier: &tier}, Granter: gr})
	if err != nil {
		t.Fatal(err)
	}
	h.Inner().Config.Retry = handlerledger.NoRetry
	store := lostDeliveryRows{LedgerStore: handlerledger.NewInMemoryLedgerStore(),
		biz: failingGiftChecks{handlerledger.NewInMemoryLedgerStore()}}
	err = handlerledger.Dispatch(context.Background(), h.Inner(), mjEvent(t, "org.created", "evt_1", "u_leo", "org_acme", ""), store)
	var ok *handlerledger.SuccessError
	if err == nil || errors.As(err, &ok) {
		t.Fatalf("an unreadable once-check must be an error (the delivery is retried), got %v", err)
	}
	if gr.calls.Load() != 0 {
		t.Fatal("a gift was granted although the once-only check could not be read")
	}
}
