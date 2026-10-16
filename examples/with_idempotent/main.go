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
		// Business dedup: one top-up per payment, however many times (and under
		// however many event ids) it is delivered. Return "" when the event
		// carries no payment id — an empty field must never become a shared key.
		Key: func(e handlerledger.Event) string {
			ev, _ := e.(authevents.Event)
			pid, _ := ev.Data["payment_id"].(string)
			if pid == "" {
				return ""
			}
			return "topup:" + pid
		},
		Retry: handlerledger.DefaultRetry(),
	}, func(ctx context.Context, e handlerledger.Event, hctx *handlerledger.Context) error {
		// Business dedup is explicit: check before the side effect, mark after.
		if done, err := hctx.AlreadyDone(ctx); err != nil {
			return err // retried
		} else if done {
			return hctx.Skip("already topped up")
		}
		slog.Info("topup handler", "event_id", e.GetEventID(), "user_id", e.GetUserID())
		// Real work goes here, with the SAME key as the money call's idempotency
		// key so the provider dedups too, e.g.
		//   q.Billing.GrantPromotionalCredit(ctx, e.GetOrgID(), billing.PromotionalCreditRequest{
		//       Amount: amount, Reason: "topup", IdempotencyKey: hctx.DedupKey})
		if err := hctx.MarkDone(ctx, e.GetEventID()); err != nil {
			slog.Warn("mark done failed (the provider's idempotency key still protects a retry)", "err", err)
		}
		return hctx.Success(e.GetEventID())
	})
	authevents.OnAuthEvent("payment.succeeded", h)

	mux := http.NewServeMux()
	// The path quotactl subscribe-events / AutoSubscribe register with auth.
	mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())
	srv := &http.Server{Addr: ":8080", Handler: mux}
	go func() {
		log.Println("listening on :8080")
		_ = srv.ListenAndServe()
	}()
	<-ctx.Done()
	_ = srv.Shutdown(context.Background())
}
