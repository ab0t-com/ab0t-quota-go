// Auth-event integration: configure ab0t-quota to listen for org.created
// + user.org_assigned, grant a credit to new users automatically.
//
// Env required:
//
//	AB0T_QUOTA_CONFIG_PATH       — quota-config.json with credit_grant
//	AB0T_AUTH_WEBHOOK_SECRET     — HMAC secret shared with auth
//	AB0T_AUTH_AUTH_URL           — auth service URL (for auto-subscribe)
//	AB0T_AUTH_ADMIN_TOKEN        — admin token for the subscribe POST
//	AB0T_AUTH_WEBHOOK_PUBLIC_URL — your public URL
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

// myCreditGranter is what the lib calls when a credit grant fires.
// In production this is your billing client's "grant credit" method.
type myCreditGranter struct{}

func (myCreditGranter) GrantCredit(ctx context.Context, in authevents.CreditGrantRequest) error {
	slog.Info("granting credit (stub)",
		"user_id", in.UserID, "org_id", in.OrgID, "tier_id", in.TierID,
		"amount", in.Amount.String())
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	q, err := quota.Setup(ctx, quota.Options{
		AutoSubscribeAuthEvents: true,
		CreditGranter:           myCreditGranter{},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer q.Close(context.Background())

	mux := http.NewServeMux()
	// Mount the auth-event receiver. Auth POSTs to this path; the lib
	// dispatches to registered handlers (including default credit grant).
	mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())

	srv := &http.Server{Addr: ":8080", Handler: mux}
	go func() {
		log.Println("listening on :8080")
		_ = srv.ListenAndServe()
	}()
	<-ctx.Done()
	_ = srv.Shutdown(context.Background())
}
