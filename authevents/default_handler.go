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

// CreditGrantHooks lets consumers observe the lifecycle. Optional.
type CreditGrantHooks struct {
	OnSkipped func(reason string, event Event)
	OnGranted func(req CreditGrantRequest)
	OnFailed  func(err error, event Event)
}

// BuildDefaultCreditGrantHandler returns the @idempotent-wrapped handler
// for org.created / user.org_assigned events. The handler:
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
			if deps.MemberJoinedEnabled && orgID != "" {
				onceKey = MemberJoinedGrantKey(orgID, userID) // the company = the org the event names
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
		// v0.3.0: once per person per company — FAIL CLOSED (an unreadable ledger is an error, so the
		// delivery is retried; nothing has been granted yet).
		if onceKey != "" {
			if hctx.Store == nil {
				return errors.New("credit grant: no ledger store to keep the gift once per person")
			}
			done, err := hctx.Store.AlreadyDone(ctx, onceKey)
			if err != nil {
				return fmt.Errorf("once-per-person check: %w", err)
			}
			if done {
				hooks.OnSkipped("already got this company's sign-up gift: "+onceKey, ev)
				return hctx.Skip("already got this company's sign-up gift")
			}
		}

		already, err := hctx.AlreadyDone(ctx)
		if err != nil {
			return fmt.Errorf("dedup check: %w", err)
		}
		if already {
			hooks.OnSkipped("business dedup hit: "+dedupKey, ev)
			return hctx.Skip("already granted: " + dedupKey)
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
			if onceKey != "" {
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

// RegisterDefaultCreditGrantHandler builds + registers the default handler
// on the events the credit-grant lifecycle observes.
func RegisterDefaultCreditGrantHandler(deps CreditGrantDeps) (*IdempotentHandler, error) {
	h, err := BuildDefaultCreditGrantHandler(deps)
	if err != nil {
		return nil, err
	}
	OnAuthEvent("org.created", h)
	OnAuthEvent("user.org_assigned", h)
	return h, nil
}
