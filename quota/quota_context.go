package quota

import (
	"context"
	"net/http"

	"github.com/ab0t-com/ab0t-quota-go/engine"
	"github.com/ab0t-com/ab0t-quota-go/middleware"
)

// Check is a pass-through to Engine.Check. Useful when consumers want to
// quota-check outside the middleware boundary (e.g., before a background
// job runs). Also fans Warn/Critical decisions to the alerts manager.
func (q *Quota) Check(ctx context.Context, in engine.CheckInput) (engine.Result, error) {
	res, err := q.Engine.Check(ctx, in)
	if err == nil && q.Alerts != nil {
		q.Alerts.Notify(ctx, res)
	}
	return res, err
}

// Spend records a spend. Pair with Check.
func (q *Quota) Spend(ctx context.Context, in engine.CheckInput) (float64, error) {
	return q.Engine.Spend(ctx, in)
}

// Release decrements a gauge (e.g., on sandbox shutdown).
func (q *Quota) Release(ctx context.Context, in engine.CheckInput) error {
	return q.Engine.Release(ctx, in)
}

// MiddlewareDeps configures the HTTP guard.
type MiddlewareDeps struct {
	Identity   middleware.Identity
	Router     middleware.ResourceRouter
	Exempt     []string
	FailOpen   bool
	OnWarn     func(*http.Request, engine.Result)
	OnDecision func(*http.Request, engine.Result)
}

// Middleware returns the HTTP guard. Wrap your handler with it.
//
// Usage:
//
//	mux.Handle("/api/", q.Middleware(quota.MiddlewareDeps{
//	    Identity: identityFn,
//	    Router:   routerFn,
//	})(yourHandler))
func (q *Quota) Middleware(deps MiddlewareDeps) func(http.Handler) http.Handler {
	return middleware.Guard(middleware.GuardConfig{
		Engine:     q.Engine,
		Identity:   deps.Identity,
		Router:     deps.Router,
		Exempt:     deps.Exempt,
		FailOpen:   deps.FailOpen,
		OnWarn:     deps.OnWarn,
		OnDecision: deps.OnDecision,
	})
}

// WebhookHandler returns the auth-event receiver. Mount at /api/quotas;
// the receiver internally routes to <prefix>/_webhooks/auth.
func (q *Quota) WebhookHandler() http.Handler {
	// Re-export the receiver assembled at Setup time. Lives in authevents.
	return q.webhookHandler
}
