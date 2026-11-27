package authevents

// v0.3.0 — the sign-up gift for a person who JOINS a company with an account they already have
// (auth's org.member.added), opt-in per company, once per person per company, on the person's own
// workspace when the event names it. Vectors: testdata/member_joined_golden_20261127.json, a
// byte-identical copy of the Python library's. RED before v0.3.0: PlanMemberJoinedGrant /
// BuildMemberJoinedCreditGrantHandler / MemberJoinedGrantKey do not exist.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
)

func TestPlanMemberJoinedGrant_MatchesTheSharedVectors_20261127(t *testing.T) {
	raw, err := os.ReadFile("../testdata/member_joined_golden_20261127.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		EventType string `json:"event_type"`
		Cases     []struct {
			Name    string           `json:"name"`
			Enabled bool             `json:"enabled"`
			Event   json.RawMessage  `json:"event"`
			Plan    MemberJoinedPlan `json:"plan"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &g); err != nil || len(g.Cases) == 0 {
		t.Fatalf("golden: %v", err)
	}
	if g.EventType != EventMemberJoined {
		t.Fatalf("event type %q", g.EventType)
	}
	for _, c := range g.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var ev Event
			if err := json.Unmarshal(c.Event, &ev); err != nil {
				t.Fatal(err)
			}
			ev.SetRaw(c.Event)
			if got := PlanMemberJoinedGrant(ev, c.Enabled); got != c.Plan {
				t.Fatalf("plan\n got %+v\nwant %+v", got, c.Plan)
			}
		})
	}
}

// tieredStub returns whatever tier the test sets (a company upgrade between two joins).
type tieredStub struct{ tier *string }

func (s tieredStub) GetTier(ctx context.Context, userID, orgID string) (string, error) {
	return *s.tier, nil
}

func twoTierCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := newCreditCfg(t) // tier "pro", sign-up grant 25, per_user_per_tier
	free := cfg.Tiers[0]
	free.TierID = "free"
	g := *cfg.Tiers[0].CreditGrant
	free.CreditGrant = &g
	cfg.Tiers = append(cfg.Tiers, free)
	return cfg
}

func mjEvent(t *testing.T, eventType, id, user, company, workspace string) Event {
	t.Helper()
	data := map[string]any{"user_id": user, "org_id": company, "role": "member", "join_method": "primitive_insert"}
	if workspace != "" {
		data["workspace_org_id"] = workspace
	}
	b, _ := json.Marshal(map[string]any{"event_type": eventType, "event_id": id, "tenant_id": company, "data": data})
	var ev Event
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	ev.SetRaw(b)
	return ev
}

type mjRig struct {
	gr     *stubGranter
	store  handlerledger.LedgerStore
	signup *IdempotentHandler
	joined *IdempotentHandler
	tier   *string
}

func newMJRig(t *testing.T, enabled bool) *mjRig {
	t.Helper()
	tier := "free"
	r := &mjRig{gr: &stubGranter{}, store: handlerledger.NewInMemoryLedgerStore(), tier: &tier}
	deps := CreditGrantDeps{Config: twoTierCfg(t), TierProvider: tieredStub{tier: &tier}, Granter: r.gr, MemberJoinedEnabled: enabled}
	var err error
	if r.signup, err = BuildDefaultCreditGrantHandler(deps); err != nil {
		t.Fatal(err)
	}
	if r.joined, err = BuildMemberJoinedCreditGrantHandler(deps); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *mjRig) deliver(t *testing.T, h *IdempotentHandler, ev Event) error {
	t.Helper()
	h.Inner().Config.Retry = handlerledger.NoRetry
	err := handlerledger.Dispatch(context.Background(), h.Inner(), ev, r.store)
	var skip *handlerledger.SkipError
	var ok *handlerledger.SuccessError
	if err != nil && (errors.As(err, &skip) || errors.As(err, &ok)) {
		return nil
	}
	return err
}

const company, workspace = "org_brightline_customers", "org_ws_maya_brightline"

func TestMemberJoined_ExistingAccountGetsTheGiftOnHerOwnWorkspace_20261127(t *testing.T) {
	r := newMJRig(t, true)
	if err := r.deliver(t, r.joined, mjEvent(t, EventMemberJoined, "evt_1", "u_maya", company, workspace)); err != nil {
		t.Fatal(err)
	}
	if r.gr.calls.Load() != 1 || r.gr.last.OrgID != workspace {
		t.Fatalf("grants %d on %q, want 1 on her workspace", r.gr.calls.Load(), r.gr.last.OrgID)
	}
	if done, _ := r.store.AlreadyDone(context.Background(), MemberJoinedGrantKey(company, "u_maya")); !done {
		t.Fatal("the once-per-person key was not written")
	}
}

func TestMemberJoined_RedeliveryAndRejoinOnAnotherTierGiveNothingMore_20261127(t *testing.T) {
	r := newMJRig(t, true)
	_ = r.deliver(t, r.joined, mjEvent(t, EventMemberJoined, "evt_1", "u_maya", company, workspace))
	_ = r.deliver(t, r.joined, mjEvent(t, EventMemberJoined, "evt_1", "u_maya", company, workspace)) // redelivery
	*r.tier = "pro"                                                                                  // removed, upgraded, joins again
	_ = r.deliver(t, r.joined, mjEvent(t, EventMemberJoined, "evt_2", "u_maya", company, workspace))
	if r.gr.calls.Load() != 1 {
		t.Fatalf("RED before: %d gifts for one person at one company", r.gr.calls.Load())
	}
}

// CONTROL (until v0.3.0): the v0.2.0 sign-up handler (option off) keyed on the tier — the same scenario
// granted twice. v0.3.1: the sign-up handler always carries the once-per-person key (opted in or not),
// so it gives ONE gift — see signup_gift_once_per_person_20261127_test.go.
func TestMemberJoined_WithoutTheOnceKeyARejoinOnAnotherTierGrantsAgain_20261127(t *testing.T) {
	r := newMJRig(t, false)
	_ = r.deliver(t, r.signup, mjEvent(t, "org.created", "evt_a", "u_maya", company, workspace))
	*r.tier = "pro"
	_ = r.deliver(t, r.signup, mjEvent(t, "org.created", "evt_b", "u_maya", company, workspace))
	if r.gr.calls.Load() != 1 {
		t.Fatalf("v0.3.1: %d grants, want 1 (once per person per company)", r.gr.calls.Load())
	}
}

func TestMemberJoined_SignUpAndJoinOfOnePersonGiveOneGift_20261127(t *testing.T) {
	for _, order := range []string{"signup-first", "join-first"} {
		t.Run(order, func(t *testing.T) {
			r := newMJRig(t, true)
			signup := mjEvent(t, "org.created", "evt_reg", "u_maya", company, workspace)
			joined := mjEvent(t, EventMemberJoined, "evt_join", "u_maya", company, workspace)
			first, second := r.signup, r.joined
			e1, e2 := signup, joined
			if order == "join-first" {
				first, second, e1, e2 = r.joined, r.signup, joined, signup
			}
			_ = r.deliver(t, first, e1)
			*r.tier = "pro" // even under another tier
			_ = r.deliver(t, second, e2)
			if r.gr.calls.Load() != 1 {
				t.Fatalf("%d gifts for one person", r.gr.calls.Load())
			}
		})
	}
}

func TestMemberJoined_NotEnabledSignUpHandlerIsV020_20261127(t *testing.T) {
	r := newMJRig(t, false)
	_ = r.deliver(t, r.signup, mjEvent(t, "org.created", "evt_1", "u_ken", "org_flat", ""))
	// v0.3.1: the sign-up gift always writes the once-per-person key (opted in or not).
	if done, _ := r.store.AlreadyDone(context.Background(), MemberJoinedGrantKey("org_flat", "u_ken")); !done {
		t.Fatal("v0.3.1: the once-per-person row was not written with the option off")
	}
	if r.gr.last.OrgID != "org_flat" {
		t.Fatalf("gift on %q", r.gr.last.OrgID)
	}
}

func TestMemberJoined_SignUpGiftLandsOnTheWorkspaceAuthNames_20261127(t *testing.T) {
	r := newMJRig(t, false)
	_ = r.deliver(t, r.signup, mjEvent(t, "org.created", "evt_1", "u_maya", company, workspace))
	if r.gr.last.OrgID != workspace {
		t.Fatalf("gift on %q, want the workspace", r.gr.last.OrgID)
	}
	_ = r.deliver(t, r.signup, mjEvent(t, "org.created", "evt_2", "u_ken", company, "")) // CONTROL: no field
	if r.gr.last.OrgID != company {
		t.Fatalf("without workspace_org_id the gift goes to the event org, got %q", r.gr.last.OrgID)
	}
}

type brokenOnceStore struct{ handlerledger.LedgerStore }

func (b brokenOnceStore) AlreadyDone(ctx context.Context, k string) (bool, error) {
	if len(k) > len("member_joined_grant:") && k[:len("member_joined_grant:")] == "member_joined_grant:" {
		return false, errors.New("ledger unavailable (simulated)")
	}
	return b.LedgerStore.AlreadyDone(ctx, k)
}

func TestMemberJoined_UnreadableLedgerFailsClosed_20261127(t *testing.T) {
	r := newMJRig(t, true)
	r.store = brokenOnceStore{handlerledger.NewInMemoryLedgerStore()}
	if err := r.deliver(t, r.joined, mjEvent(t, EventMemberJoined, "evt_1", "u_maya", company, workspace)); err == nil {
		t.Fatal("an unreadable once-check must be an error (the delivery is retried)")
	}
	if r.gr.calls.Load() != 0 {
		t.Fatal("a gift was granted although the once-per-person check could not be read")
	}
}

func TestMemberJoinedGrantKey_SameAsPython_20261127(t *testing.T) {
	if MemberJoinedGrantKey("org_x", "u_1") != "member_joined_grant:org_x:u_1" {
		t.Fatal(MemberJoinedGrantKey("org_x", "u_1"))
	}
}

func TestAuthEventsBlock_TypoIsRefused_20261127(t *testing.T) {
	for _, bad := range []string{`{"grant_on_member_join": true}`, `{"grant_on_member_joined": "true"}`,
		`{"grant_on_member_joined": null}`, `"on"`} {
		if err := config.ValidateAuthEventsBlock(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if err := config.ValidateAuthEventsBlock(json.RawMessage(`{"grant_on_member_joined": true, "$comment": "gift on join"}`)); err != nil {
		t.Fatalf("CONTROL: the documented block was refused: %v", err)
	}
}
