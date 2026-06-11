// Custom @idempotent handler: register a non-default handler for an
// event_type with delivery-dedup + retry + ledger persistence.
//
// Pattern: a Stripe-webhook-style payment.succeeded handler that mutates
// downstream state; retries on transient failure; only runs once per
// event_id.
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
	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
	"github.com/ab0t-com/ab0t-quota-go/quota"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	q, err := quota.Setup(ctx, quota.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer q.Close(context.Background())

	// Register a custom handler with retry + dedup.
	h := authevents.Idempotent(handlerledger.IdempotentConfig{
		Handler: "payment_credit_topup",
		Key: func(e handlerledger.Event) string {
			// Business-dedup: one top-up per (user, payment_id).
			return "topup:" + e.GetUserID() + ":" + e.GetEventID()
		},
		Retry: handlerledger.DefaultRetry(),
	}, func(ctx context.Context, e handlerledger.Event, hctx *handlerledger.Context) error {
		slog.Info("topup handler",
			"event_id", e.GetEventID(), "user_id", e.GetUserID())
		// Real work would go here (call billing.GrantCredit, etc).
		return hctx.Success(e.GetEventID())
	})
	authevents.OnAuthEvent("payment.succeeded", h)

	mux := http.NewServeMux()
	mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())
	srv := &http.Server{Addr: ":8080", Handler: mux}
	go func() {
		log.Println("listening on :8080")
		_ = srv.ListenAndServe()
	}()
	<-ctx.Done()
	_ = srv.Shutdown(context.Background())
}
