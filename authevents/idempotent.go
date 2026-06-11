package authevents

import (
	"context"

	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
)

// IdempotentHandler is the public wrapper around the ledger machinery.
//
// It implements Handler, and the receiver detects it via type assertion to
// route through the ledger dispatcher (delivery dedup + retry + outcome
// persistence). Bare *IdempotentHandler.Handle calls bypass the ledger
// and dispatch directly — used only in tests where no store is in scope.
type IdempotentHandler struct {
	inner *handlerledger.IdempotentHandler
}

// Inner returns the underlying handlerledger handler. Exposed so the
// receiver can pass it to handlerledger.Dispatch with its configured store.
func (h *IdempotentHandler) Inner() *handlerledger.IdempotentHandler { return h.inner }

// Handle satisfies the Handler interface. Dispatches via the ledger with
// a per-call in-memory store (a Dispatch nil store falls back to memory).
// In production, the receiver type-switches on *IdempotentHandler before
// reaching this path — so the configured store is used.
func (h *IdempotentHandler) Handle(ctx context.Context, event Event) error {
	return handlerledger.Dispatch(ctx, h.inner, event, nil)
}

// Idempotent wraps an inner handler in delivery-dedup + business-dedup +
// retry + outcome-persistence semantics. Returns the public type registered
// against an event_type.
//
// Use like Python's @idempotent:
//
//	h := authevents.Idempotent(handlerledger.IdempotentConfig{
//	    Handler: "credit_grant",
//	    Key: func(e handlerledger.Event) string {
//	        return authevents.ComposeCreditDedupKey("per_user_per_tier",
//	            e.GetUserID(), e.GetOrgID(), "tier_id_from_lookup")
//	    },
//	}, func(ctx context.Context, ev handlerledger.Event, hctx *handlerledger.Context) error {
//	    // your work
//	    return nil
//	})
//	authevents.OnAuthEvent("org.created", h)
func Idempotent(cfg handlerledger.IdempotentConfig, inner handlerledger.InnerHandler) *IdempotentHandler {
	return &IdempotentHandler{inner: handlerledger.Idempotent(cfg, inner)}
}
