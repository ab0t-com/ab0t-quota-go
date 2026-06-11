package handlerledger

import (
	"context"
	"encoding/json"
)

// Event is the minimal event shape this package needs. The full Event
// struct lives in authevents; we accept any value with these methods to
// avoid an import cycle.
type Event interface {
	GetEventID() string
	GetEventType() string
	GetUserID() string
	GetOrgID() string
	Raw() json.RawMessage
}

// IdempotentConfig wires policy into Idempotent().
type IdempotentConfig struct {
	Handler      string             // stable handler name (used as part of ledger key)
	Key          func(Event) string // optional business dedup key fn
	Retry        *RetryConfig       // nil = default; NoRetry = single attempt
	LeaseSeconds int                // default 60
}

// Context is the per-invocation handler context.
type Context struct {
	HandlerName  string
	EventID      string
	EventType    string
	EventPayload json.RawMessage
	Store        LedgerStore
	DedupKey     string // composed by IdempotentConfig.Key
}

// AlreadyDone reports whether the business dedup key was previously marked.
// Returns false (no error) if no dedup key was configured.
func (c *Context) AlreadyDone(ctx context.Context) (bool, error) {
	if c.DedupKey == "" {
		return false, nil
	}
	return c.Store.AlreadyDone(ctx, c.DedupKey)
}

// MarkDone records the business dedup row. No-op if no dedup key.
func (c *Context) MarkDone(ctx context.Context, sideEffectID string) error {
	if c.DedupKey == "" {
		return nil
	}
	return c.Store.MarkDone(ctx, MarkDoneInput{
		DedupKey:      c.DedupKey,
		SourceHandler: c.HandlerName,
		SourceEventID: c.EventID,
		SideEffectID:  sideEffectID,
	})
}

// Skip returns a SkipError; the dispatcher records status=skipped.
func (c *Context) Skip(reason string) error { return &SkipError{Reason: reason} }

// Success returns a SuccessError; the dispatcher records status=success.
func (c *Context) Success(sideEffectID string) error {
	return &SuccessError{SideEffectID: sideEffectID}
}

// InnerHandler is the user's handler body. Takes the Event + per-invocation
// Context. Return nil for plain success, or one of the sentinel errors via
// ctx.Skip / ctx.Success, or any other error to mark as failed (and retry).
type InnerHandler func(ctx context.Context, event Event, hctx *Context) error

// IdempotentHandler is the concrete type the receiver dispatcher
// type-switches on. Exported so external dispatchers can detect it.
type IdempotentHandler struct {
	Config IdempotentConfig
	Inner  InnerHandler
}

// Idempotent wraps inner with delivery-dedup + business-dedup + retry
// machinery and returns a concrete *IdempotentHandler.
//
// Type-assertion-free detection: the receiver type-switches on
// *IdempotentHandler. Plain handlers (HandlerFunc) follow the other
// branch and run as v0.5.1.
func Idempotent(cfg IdempotentConfig, inner InnerHandler) *IdempotentHandler {
	if cfg.LeaseSeconds == 0 {
		cfg.LeaseSeconds = 60
	}
	if cfg.Retry == nil {
		cfg.Retry = DefaultRetry()
	}
	return &IdempotentHandler{Config: cfg, Inner: inner}
}
