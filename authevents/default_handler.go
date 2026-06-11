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
func BuildDefaultCreditGrantHandler(deps CreditGrantDeps) (*IdempotentHandler, error) {
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

		userID := ev.GetUserID()
		orgID := ev.GetOrgID()
		if userID == "" {
			hooks.OnSkipped("missing user_id", ev)
			return hctx.Skip("missing user_id")
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

		// Business dedup — per-tier policy (defaults to per_user_per_tier).
		policy := string(tier.CreditGrant.Dedup)
		if policy == "" {
			policy = string(config.DedupPerUserPerTier)
		}
		dedupKey := ComposeCreditDedupKey(policy, userID, orgID, tierID)
		hctx.DedupKey = dedupKey

		already, err := hctx.AlreadyDone(ctx)
		if err != nil {
			return fmt.Errorf("dedup check: %w", err)
		}
		if already {
			hooks.OnSkipped("business dedup hit: "+dedupKey, ev)
			return hctx.Skip("already granted: " + dedupKey)
		}

		req := CreditGrantRequest{
			UserID:    userID,
			OrgID:     orgID,
			TierID:    tierID,
			Amount:    tier.CreditGrant.AmountPerPeriod.Decimal,
			Currency:  tier.CreditGrant.Currency,
			EventID:   ev.GetEventID(),
			GrantedAt: ev.OccurredAt.UTC().Format("2006-01-02T15:04:05Z"),
			Source:    "auth_event",
			Trigger:   string(tier.CreditGrant.Trigger),
		}
		if err := granter.GrantCredit(ctx, req); err != nil {
			hooks.OnFailed(err, ev)
			return fmt.Errorf("grant credit: %w", err)
		}

		if err := hctx.MarkDone(ctx, ev.GetEventID()); err != nil {
			slog.Warn("mark business dedup failed", "key", dedupKey, "err", err)
		}

		hooks.OnGranted(req)
		slog.Info("credit granted via auth-event",
			"user_id", userID, "org_id", orgID, "tier_id", tierID,
			"amount", req.Amount.String(), "event_id", req.EventID)
		return hctx.Success(req.EventID)
	}

	wcfg := handlerledger.IdempotentConfig{
		Handler:      "default_credit_grant",
		Retry:        handlerledger.DefaultRetry(),
		LeaseSeconds: 60,
	}
	return Idempotent(wcfg, inner), nil
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
