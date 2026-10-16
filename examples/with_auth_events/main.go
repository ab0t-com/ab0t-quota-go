// Auth-event integration: listen for org.created + user.org_assigned and grant
// the tier's sign-up credit (credit_grant with "trigger": "signup") exactly once.
//
// Env:
//
//	QUOTA_CONFIG_PATH            — quota-config.json with a signup credit_grant
//	AB0T_AUTH_WEBHOOK_SECRET     — HMAC secret shared with auth
//	AB0T_QUOTA_BILLING_URL       — billing (enables the built-in granter)
//	AB0T_QUOTA_BILLING_TOKEN     — your billing key (billing.write.transactions)
//	AB0T_AUTH_AUTH_URL           — auth service URL (for auto-subscribe)
//	AB0T_AUTH_ADMIN_TOKEN        — token for the subscribe call
//	AB0T_AUTH_WEBHOOK_PUBLIC_URL — your public URL
//
// Register the subscription once with `quotactl subscribe-events` (it fails
// loudly); AutoSubscribeAuthEvents re-checks it at every boot, best-effort.
package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
	"github.com/ab0t-com/ab0t-quota-go/quota"
)

// logOnlyGranter is used when billing is not configured, so the example runs
// without a billing key. It grants nothing.
type logOnlyGranter struct{}

func (logOnlyGranter) GrantCredit(ctx context.Context, in authevents.CreditGrantRequest) error {
	slog.Info("would grant credit (no AB0T_QUOTA_BILLING_URL)",
		"user_id", in.UserID, "org_id", in.OrgID, "tier_id", in.TierID,
		"amount", in.Amount.String(), "idempotency_key", in.IdempotencyKey)
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The built-in granter calls billing's real credit routes with a stable
	// idempotency key (v0.1.10). Sign-up credit alone is not paid metering, so
	// paid mode (which needs the durable outbox) is switched off explicitly.
	var granter authevents.CreditGranter = logOnlyGranter{}
	if os.Getenv("AB0T_QUOTA_BILLING_URL") != "" {
		granter = quota.BillingCreditGranter(nil) // = the billing client Setup builds
	}
	paid := false
	q, err := quota.Setup(ctx, quota.Options{
		AutoSubscribeAuthEvents: true,
		CreditGranter:           granter,
		EnablePaid:              &paid,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer q.Close(context.Background())

	mux := http.NewServeMux()
	// Mount the auth-event receiver at the path registered with auth
	// (<public url>/api/quotas/_webhooks/auth). Before v0.1.10 this mount
	// answered 404; it now works as written (http.StripPrefix also works).
	mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())

	srv := &http.Server{Addr: ":8080", Handler: mux}
	go func() {
		log.Println("listening on :8080")
		_ = srv.ListenAndServe()
	}()
	<-ctx.Done()
	_ = srv.Shutdown(context.Background())
}
