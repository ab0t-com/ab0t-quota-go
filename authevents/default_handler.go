package authevents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
	"github.com/shopspring/decimal"
)

// CreditGrantDeps is what the default handler needs at construction time.
// All fields are required EXCEPT PinStore and Hooks.
//
// Wire-level parity note:
//
//	Python lib has Known Upstream Bug #1 — setup.py:938 NameError because
//	tier_provider isn't explicitly wired into the default handler. This Go
//	port REQUIRES TierProvider as a constructor argument so misconfiguration
//	surfaces at startup, not at first event.
type CreditGrantDeps struct {
	Config       *config.Config
	TierProvider TierProvider
	PinStore     PinStore
	Ledger       handlerledger.LedgerStore
	Granter      CreditGranter
	Hooks        *CreditGrantHooks // optional
	// MemberJoinedEnabled (v0.3.0) — the company opted in (auth_events.grant_on_member_joined): the
	// sign-up handler also checks and writes MemberJoinedGrantKey, so a sign-up and a join of the
	// same person give ONE gift. false = the v0.2.0 handler exactly.
	// v0.3.1: the sign-up handler ALWAYS keeps the gift once per person per company (SignupGiftOnceKeys),
	// so this field no longer changes it; quota.Setup still uses the opt-in to register the
	// member-joined handler.
	MemberJoinedEnabled bool
}

// EventMemberJoined (v0.3.0) is auth's webhook event for every NEW membership (data: user_id,
// org_id, role, join_method[, workspace_org_id]). auth.user.registered is sent only for a new
// ACCOUNT, so a person joining a second company never got that company's sign-up gift.
const EventMemberJoined = "org.member.added"

// MemberJoinedGrantKey (v0.3.0) is the durable ledger key of "this person already got this
// company's sign-up gift" — once per person per company, under any tier or dedup rule, whichever
// event arrives first. companyOrgID is the event's org (the org joined). Same key as the Python
// library (compose_member_joined_grant_key).
//
// v0.3.1: this is THE once-per-person-per-company key of the sign-up gift — the sign-up handler
// always checks and writes it, opted in or not (SignupGiftOnceKeys). The text keeps its v0.3.0 name
// so markers already written still count.
func MemberJoinedGrantKey(companyOrgID, userID string) string {
	return "member_joined_grant:" + companyOrgID + ":" + userID
}

// MemberJoinedPlan (v0.3.0) is what the member-joined handler does with one event. Shared vectors
// with the Python library: testdata/member_joined_golden_20261127.json.
type MemberJoinedPlan struct {
	Action         string `json:"action"` // "grant" | "skip"
	Reason         string `json:"reason"` // "" | not_enabled | missing_user_id | missing_org_id
	UserID         string `json:"user_id"`
	CompanyOrgID   string `json:"company_org_id"`
	BillingOrgID   string `json:"billing_org_id"`
	OnceKey        string `json:"once_key"`
	SourceEventKey string `json:"source_event_key"`
}

func dataString(ev Event, key string) string {
	v, _ := ev.Data[key].(string)
	return v
}

// PlanMemberJoinedGrant (v0.3.0) — pure, no I/O. user = data.user_id, else the envelope user_id;
// company = data.org_id, else data.organization_id, else the envelope tenant_id; the gift lands on
// data.workspace_org_id (non-blank), else on the company org. Same rule as the Python library
// (plan_member_joined_grant).
func PlanMemberJoinedGrant(ev Event, enabled bool) MemberJoinedPlan {
	user := firstNonEmpty(dataString(ev, "user_id"), ev.UserIDTop)
	company := firstNonEmpty(dataString(ev, "org_id"), dataString(ev, "organization_id"), ev.TenantID)
	p := MemberJoinedPlan{Action: "skip", UserID: user, CompanyOrgID: company}
	switch {
	case !enabled:
		p.Reason = "not_enabled"
	case user == "":
		p.Reason = "missing_user_id"
	case company == "":
		p.Reason = "missing_org_id"
	default:
		p.Action = "grant"
		p.BillingOrgID = firstNonEmpty(ev.GetWorkspaceOrgID(), company)
		p.OnceKey = MemberJoinedGrantKey(company, user)
		p.SourceEventKey = SourceEventGrantKey(ev.GetEventID())
	}
	return p
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// TierProvider resolves user → tier_id. Mirrors the Python ABC.
type TierProvider interface {
	GetTier(ctx context.Context, userID, orgID string) (tierID string, err error)
}

// CreditGranter applies a credit grant. The default handler calls this
// AFTER dedup + ledger checks pass.
type CreditGranter interface {
	GrantCredit(ctx context.Context, in CreditGrantRequest) error
}

// CreditGrantRequest carries the resolved grant to be applied.
type CreditGrantRequest struct {
	UserID    string
	OrgID     string
	TierID    string
	Amount    decimal.Decimal
	Currency  string
	EventID   string
	GrantedAt string
	Source    string // "auth_event"
	Trigger   string // CreditTrigger value

	// v0.1.10 — what a granter needs to move the money exactly once and into
	// the right bucket. All populated by the default handler.
	//
	// IdempotencyKey is the key to send to billing. It is derived from the
	// tier's dedup policy only (never the event id), so every redelivery,
	// replay or backfill of the same grant carries the SAME key and billing's
	// durable dedup refuses the duplicate. Shape (wire-compatible with the
	// Python runtime, ab0t_quota/auth_events.py:641-650):
	//
	//	per_user_per_tier → user:{user_id}:initial_credit:{tier_id}
	//	per_org_per_tier  → org:{org_id}:initial_credit:{tier_id}
	//	per_user_global   → user:{user_id}:initial_credit
	//	per_org_global    → org:{org_id}:initial_credit
	IdempotencyKey string
	// DedupKey is the library's business-dedup key (ComposeCreditDedupKey).
	DedupKey string
	// Destination / Lifecycle / RolloverMax are the tier's credit_grant
	// declaration (RolloverMax = rollover_max_periods × amount, set only for
	// lifecycle "rollover_capped").
	Destination string
	Lifecycle   string
	RolloverMax *decimal.Decimal
}

// SourceEventGrantKey (v0.2.0) is the durable ledger key of "this source EVENT already produced a
// sign-up grant" — independent of the dedup RULE. Same key as the Python library's
// compose_source_event_grant_key.
func SourceEventGrantKey(eventID string) string { return "credit_grant_event:" + eventID }

// BillingIdempotencyKey returns the billing idempotency key for a sign-up grant
// under the given dedup policy (see CreditGrantRequest.IdempotencyKey).
func BillingIdempotencyKey(policy, userID, orgID, tierID string) string {
	switch policy {
	case string(config.DedupPerOrgPerTier):
		return "org:" + orgID + ":initial_credit:" + tierID
	case string(config.DedupPerUserGlobal):
		return "user:" + userID + ":initial_credit"
	case string(config.DedupPerOrgGlobal):
		return "org:" + orgID + ":initial_credit"
	default: // per_user_per_tier
		return "user:" + userID + ":initial_credit:" + tierID
	}
}

// SignupGiftKeys (v0.3.1) — the keys that keep the sign-up gift once per person per company.
type SignupGiftKeys struct {
	// OnceKey is MemberJoinedGrantKey(company, user): the key the member-joined gift already uses,
	// so a sign-up and a join of one person still share it.
	OnceKey string `json:"once_key"`
	// BillingIdempotencyKey is what billing receives: the CURRENT rule's key, unchanged, so
	// billing's records for people gifted by an earlier release still match it.
	BillingIdempotencyKey string `json:"billing_idempotency_key"`
	// AlreadyGiftedKeys — a marker under ANY of these means "already gifted": the once-key, the
	// current rule's dedup and billing keys, and the keys a per-person rule wrote for this person
	// (this plan, or any plan).
	AlreadyGiftedKeys []string `json:"already_gifted_keys"`
}

// SignupGiftOnceKeys (v0.3.1) — the sign-up gift is once per person per company, whatever the
// company's rule says. Before, the gift's "already given" markers were derived from the rule
// (credit_grant.dedup) alone, so a company that changed its rule (or a person whose plan changed
// under a per-plan rule) got keys that matched none of the old markers, and an existing customer
// whose sign-up was handled again got the gift a second time.
//
// Pure (no I/O). orgID is the org the gift lands on; companyOrgID is the company the person signed
// up to or joined (the event's org; "" = orgID). Keys an earlier release wrote under a per-ORG rule
// are honoured only while that rule is in force: they name an org, not a person, and reading them
// as "this person was gifted" would deny the gift to new people in a shared org. Same vectors as the
// Python library (signup_gift_keys): testdata/signup_gift_key_golden_20261127.json.
func SignupGiftOnceKeys(policy, userID, orgID, tierID, companyOrgID string) SignupGiftKeys {
	company := firstNonEmpty(companyOrgID, orgID)
	k := SignupGiftKeys{
		OnceKey:               MemberJoinedGrantKey(company, userID),
		BillingIdempotencyKey: BillingIdempotencyKey(policy, userID, orgID, tierID),
	}
	candidates := []string{k.OnceKey, ComposeCreditDedupKey(policy, userID, orgID, tierID), k.BillingIdempotencyKey}
	for _, perPerson := range []string{string(config.DedupPerUserPerTier), string(config.DedupPerUserGlobal)} {
		candidates = append(candidates,
			ComposeCreditDedupKey(perPerson, userID, orgID, tierID),
			BillingIdempotencyKey(perPerson, userID, orgID, tierID))
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		if !seen[c] {
			seen[c] = true
			k.AlreadyGiftedKeys = append(k.AlreadyGiftedKeys, c)
		}
	}
	return k
}

// CreditGrantHooks lets consumers observe the lifecycle. Optional.
type CreditGrantHooks struct {
	OnSkipped func(reason string, event Event)
	OnGranted func(req CreditGrantRequest)
	OnFailed  func(err error, event Event)
}

// BuildDefaultCreditGrantHandler returns the @idempotent-wrapped handler
// for the sign-up event (auth.user.registered; v0.3.2 — see DefaultCreditGrantEventTypes). The handler:
//
//  1. resolves user/org from event (pin store fallback)
//  2. resolves tier via TierProvider (REQUIRED, explicit — no Python BUG #1)
//  3. looks up tier in Config; bails if no credit_grant or amount=0
//  4. composes business dedup key per tier.credit_grant.dedup policy
//  5. calls Granter.GrantCredit
//
// v0.3.0: the gift lands on data.workspace_org_id when the event names the person's own workspace
// (GetWorkspaceOrgID), else on the event org as before; with deps.MemberJoinedEnabled it also keeps
// the gift once per person per company (MemberJoinedGrantKey).
//
// v0.3.1: always once per person per company, whatever the rule or plan (SignupGiftOnceKeys): the
// once-key and every key an earlier release wrote for this gift are checked FAIL-CLOSED (an
// unreadable ledger is an error, so the delivery is retried, never a second gift); the key sent to
// billing is unchanged.
func BuildDefaultCreditGrantHandler(deps CreditGrantDeps) (*IdempotentHandler, error) {
	return buildCreditGrantHandler(deps, false)
}

// BuildMemberJoinedCreditGrantHandler (v0.3.0) returns the handler for org.member.added
// (EventMemberJoined): PlanMemberJoinedGrant decides; the gift is the tier's sign-up grant, on the
// person's workspace when the event names it, once per person per company (MemberJoinedGrantKey,
// checked FAIL-CLOSED: an unreadable ledger is an error, so the delivery is retried, never a
// second gift). Register it only for a company that opted in (auth_events.grant_on_member_joined).
func BuildMemberJoinedCreditGrantHandler(deps CreditGrantDeps) (*IdempotentHandler, error) {
	return buildCreditGrantHandler(deps, true)
}

func buildCreditGrantHandler(deps CreditGrantDeps, memberMode bool) (*IdempotentHandler, error) {
	if deps.Config == nil {
		return nil, errors.New("default credit-grant handler: Config required")
	}
	if deps.TierProvider == nil {
		return nil, errors.New("default credit-grant handler: TierProvider required (see Known Upstream Bug #1)")
	}
	if deps.Granter == nil {
		return nil, errors.New("default credit-grant handler: Granter required")
	}
	if deps.PinStore == nil {
		deps.PinStore = NoopPinStore{}
	}
	if deps.Hooks == nil {
		deps.Hooks = &CreditGrantHooks{}
	}
	if deps.Hooks.OnSkipped == nil {
		deps.Hooks.OnSkipped = func(string, Event) {}
	}
	if deps.Hooks.OnGranted == nil {
		deps.Hooks.OnGranted = func(CreditGrantRequest) {}
	}
	if deps.Hooks.OnFailed == nil {
		deps.Hooks.OnFailed = func(error, Event) {}
	}

	pinStore := deps.PinStore
	hooks := deps.Hooks
	tierProvider := deps.TierProvider
	granter := deps.Granter
	cfgPtr := deps.Config

	inner := func(ctx context.Context, event handlerledger.Event, hctx *handlerledger.Context) error {
		ev, ok := event.(Event)
		if !ok {
			return errors.New("default credit-grant handler: expected authevents.Event")
		}

		var userID, orgID, onceKey string
		if memberMode {
			// v0.3.0 — a person joined the company (any account, new or existing).
			plan := PlanMemberJoinedGrant(ev, true)
			if plan.Action != "grant" {
				hooks.OnSkipped("member-joined gift: "+plan.Reason, ev)
				return hctx.Skip(plan.Reason)
			}
			userID, orgID, onceKey = plan.UserID, plan.BillingOrgID, plan.OnceKey
		} else {
			userID = ev.GetUserID()
			orgID = ev.GetOrgID()
			if userID == "" {
				hooks.OnSkipped("missing user_id", ev)
				return hctx.Skip("missing user_id")
			}
			// v0.3.1: always — once per person per company (the company = the org the event names).
			if orgID != "" {
				onceKey = MemberJoinedGrantKey(orgID, userID)
			}
			// v0.3.0: auth names the person's own workspace — the account the company bills.
			if ws := ev.GetWorkspaceOrgID(); ws != "" {
				orgID = ws
			}

			// org_id fallback: pin store
			if orgID == "" {
				if pinned, err := pinStore.Get(userID); err == nil && pinned != "" {
					orgID = pinned
				}
			}
			if orgID != "" {
				_ = pinStore.Set(userID, orgID, "auto")
			}
		}

		// Tier — explicitly wired, no NameError fallback.
		tierID, err := tierProvider.GetTier(ctx, userID, orgID)
		if err != nil {
			hooks.OnFailed(err, ev)
			return fmt.Errorf("tier provider: %w", err)
		}
		if tierID == "" {
			hooks.OnSkipped("tier_provider returned empty tier_id", ev)
			return hctx.Skip("no tier for user")
		}

		tier := cfgPtr.TierByID(tierID)
		if tier == nil {
			hooks.OnSkipped("tier not in config: "+tierID, ev)
			return hctx.Skip("tier not in config: " + tierID)
		}
		if tier.CreditGrant == nil || tier.CreditGrant.AmountPerPeriod.Decimal.Sign() <= 0 {
			hooks.OnSkipped("no credit_grant for tier: "+tierID, ev)
			return hctx.Skip("no credit_grant for tier")
		}
		// v0.1.10 (ticket 20261016, REV-01): an auth event is a SIGN-UP. Only a
		// trigger "signup" grant may fire here — a paid tier's monthly refill
		// (subscription_invoice_paid) or a manual/scheduled grant must not be
		// handed out as a free gift every time someone joins an org on that
		// tier. Python parity (auth_events.py:608-610). An absent trigger is
		// still treated as sign-up (what v0.1.9 did for it), loudly.
		switch tier.CreditGrant.Trigger {
		case config.CreditTriggerSignup:
		case "":
			slog.Warn("credit_grant has no trigger — treating it as \"signup\"; declare "+
				"\"trigger\": \"signup\" explicitly (the Python runtime does NOT grant without it)",
				"tier_id", tierID)
		default:
			reason := "credit_grant trigger is " + string(tier.CreditGrant.Trigger) + ", not signup: " + tierID
			hooks.OnSkipped(reason, ev)
			return hctx.Skip(reason)
		}

		// Business dedup — per-tier policy (defaults to per_user_per_tier).
		policy := string(tier.CreditGrant.Dedup)
		if policy == "" {
			policy = string(config.DedupPerUserPerTier)
		}
		dedupKey := ComposeCreditDedupKey(policy, userID, orgID, tierID)
		hctx.DedupKey = dedupKey
		// v0.3.1: once per person per company, whatever the rule (SignupGiftOnceKeys). A pin-store
		// resolved org (no org in the event) is the company too.
		giftKeys := SignupGiftOnceKeys(policy, userID, orgID, tierID, "")
		if onceKey == "" {
			onceKey = giftKeys.OnceKey
		}
		alreadyGiftedKeys := []string{onceKey}
		for _, k := range giftKeys.AlreadyGiftedKeys[1:] {
			if k != onceKey {
				alreadyGiftedKeys = append(alreadyGiftedKeys, k)
			}
		}

		// v0.2.0: the grant is ALSO keyed on the SOURCE event — one event produces at most one
		// grant under any dedup rule, so a company that changes credit_grant.dedup and re-handles an
		// old event does not grant twice; a new event follows the new rule. Same key as Python
		// (compose_source_event_grant_key). The billing request is unchanged.
		eventKey := SourceEventGrantKey(ev.GetEventID())
		if hctx.Store != nil {
			done, err := hctx.Store.AlreadyDone(ctx, eventKey)
			if err != nil {
				return fmt.Errorf("source-event dedup check: %w", err)
			}
			if done {
				hooks.OnSkipped("source event already granted: "+ev.GetEventID(), ev)
				return hctx.Skip("source event already granted")
			}
		}
		// v0.3.0 / v0.3.1: once per person per company — the once-key, the current rule's key, or a
		// key an earlier release wrote for this person. FAIL CLOSED (an unreadable ledger is an error,
		// so the delivery is retried; nothing has been granted yet).
		if hctx.Store == nil {
			return errors.New("credit grant: no ledger store to keep the gift once per person")
		}
		for _, k := range alreadyGiftedKeys {
			done, err := hctx.Store.AlreadyDone(ctx, k)
			if err != nil {
				return fmt.Errorf("once-per-person check: %w", err)
			}
			if done && k == dedupKey { // the current rule's own key: same reasons as before
				hooks.OnSkipped("business dedup hit: "+dedupKey, ev)
				return hctx.Skip("already granted: " + dedupKey)
			}
			if done {
				hooks.OnSkipped("already got this company's sign-up gift: "+k, ev)
				return hctx.Skip("already got this company's sign-up gift")
			}
		}

		req := CreditGrantRequest{
			UserID:         userID,
			OrgID:          orgID,
			TierID:         tierID,
			Amount:         tier.CreditGrant.AmountPerPeriod.Decimal,
			Currency:       tier.CreditGrant.Currency,
			EventID:        ev.GetEventID(),
			GrantedAt:      ev.OccurredAt.UTC().Format("2006-01-02T15:04:05Z"),
			Source:         "auth_event",
			Trigger:        string(tier.CreditGrant.Trigger),
			IdempotencyKey: BillingIdempotencyKey(policy, userID, orgID, tierID),
			DedupKey:       dedupKey,
			Destination:    string(tier.CreditGrant.Destination),
			Lifecycle:      string(tier.CreditGrant.Lifecycle),
		}
		if tier.CreditGrant.Lifecycle == config.CreditLifecycleRolloverCapped && tier.CreditGrant.RolloverMaxPeriods != nil {
			m := tier.CreditGrant.AmountPerPeriod.Decimal.Mul(decimal.NewFromInt(int64(*tier.CreditGrant.RolloverMaxPeriods)))
			req.RolloverMax = &m
		}
		if err := granter.GrantCredit(ctx, req); err != nil {
			hooks.OnFailed(err, ev)
			return fmt.Errorf("grant credit: %w", err)
		}

		if err := hctx.MarkDone(ctx, ev.GetEventID()); err != nil {
			slog.Warn("mark business dedup failed", "key", dedupKey, "err", err)
		}
		if hctx.Store != nil {
			if err := hctx.Store.MarkDone(ctx, handlerledger.MarkDoneInput{
				DedupKey: eventKey, SourceHandler: hctx.HandlerName, SourceEventID: ev.GetEventID(),
				SideEffectID: req.IdempotencyKey,
			}); err != nil {
				slog.Warn("mark source-event dedup failed", "key", eventKey, "err", err)
			}
			if onceKey != "" { // always set since v0.3.1
				if err := hctx.Store.MarkDone(ctx, handlerledger.MarkDoneInput{
					DedupKey: onceKey, SourceHandler: hctx.HandlerName, SourceEventID: ev.GetEventID(),
					SideEffectID: req.IdempotencyKey,
				}); err != nil {
					slog.Warn("mark once-per-person key failed", "key", onceKey, "err", err)
				}
			}
		}

		hooks.OnGranted(req)
		slog.Info("credit granted via auth-event",
			"user_id", userID, "org_id", orgID, "tier_id", tierID,
			"amount", req.Amount.String(), "event_id", req.EventID)
		return hctx.Success(req.EventID)
	}

	name := "default_credit_grant"
	if memberMode {
		name = "member_joined_credit_grant"
	}
	wcfg := handlerledger.IdempotentConfig{
		Handler:      name,
		Retry:        handlerledger.DefaultRetry(),
		LeaseSeconds: 60,
	}
	return Idempotent(wcfg, inner), nil
}

// RegisterMemberJoinedCreditGrantHandler (v0.3.0) builds the member-joined handler and registers it
// on EventMemberJoined. quota.Setup calls it only when auth_events.grant_on_member_joined is true.
func RegisterMemberJoinedCreditGrantHandler(deps CreditGrantDeps) (*IdempotentHandler, error) {
	h, err := BuildMemberJoinedCreditGrantHandler(deps)
	if err != nil {
		return nil, err
	}
	OnAuthEvent(EventMemberJoined, h)
	return h, nil
}

// DefaultCreditGrantEventTypes (v0.3.2) are the event types RegisterDefaultCreditGrantHandler
// registers the sign-up handler on: auth.user.registered — the event auth delivers for a new
// account — and, for the receiver only, the pre-v0.3.2 names (NeverDeliveredLegacyGiftEvents) so a
// replay, a backfill or a custom relay that still posts them keeps working. Auth never delivers the
// old names, and the subscription defaults (SubscribeOnStartup, quotactl subscribe-events) never ask
// for them.
var DefaultCreditGrantEventTypes = append([]string{EventUserRegistered}, NeverDeliveredLegacyGiftEvents...)

// RegisterDefaultCreditGrantHandler builds + registers the default handler
// on the events the credit-grant lifecycle observes (DefaultCreditGrantEventTypes).
//
// v0.3.2: it now listens on auth.user.registered. Before, it listened only on "org.created" and
// "user.org_assigned", which auth never delivers to a webhook — the documented sign-up gift never
// arrived.
func RegisterDefaultCreditGrantHandler(deps CreditGrantDeps) (*IdempotentHandler, error) {
	h, err := BuildDefaultCreditGrantHandler(deps)
	if err != nil {
		return nil, err
	}
	for _, t := range DefaultCreditGrantEventTypes {
		OnAuthEvent(t, h)
	}
	return h, nil
}
