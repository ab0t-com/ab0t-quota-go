package main

import (
	"context"
	"net/http"

	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/quota"
)

func wire(ctx context.Context, q *quota.Quota, bc *billing.Client, mux *http.ServeMux) {
	mux.Handle("/api/quotas/", http.StripPrefix("/api/quotas", q.WebhookHandler()))
	_, _ = bc.GrantCredit(ctx, billing.CreditGrantRequest{OrgID: "o"})
	_ = bc.RecordUsage(ctx, billing.RecordUsageRequest{OrgID: "o"})
	_ = "/billing/" + "o" + "/promotional-credit"
}
