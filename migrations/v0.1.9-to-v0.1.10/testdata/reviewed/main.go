package main

// quotago:v0.1.10-reviewed-file — granter kept on purpose; GrantCredit/RecordUsage checked.

import (
	"context"
	"net/http"

	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/quota"
)

func wire(ctx context.Context, q *quota.Quota, bc *billing.Client, mux *http.ServeMux) {
	mux.Handle("/api/quotas/_webhooks/auth", q.WebhookHandler())
	_, _ = bc.GrantCredit(ctx, billing.CreditGrantRequest{OrgID: "o"})
	_ = bc.RecordUsage(ctx, billing.RecordUsageRequest{OrgID: "o", Cost: "0"})
}
