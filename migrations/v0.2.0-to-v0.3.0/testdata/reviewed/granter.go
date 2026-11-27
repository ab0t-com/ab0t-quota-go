package example

// quotago:v0.3.0-reviewed-file — OrgID is whatever account the gift lands on (workspace or org).

import (
	"context"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
)

type myGranter struct{}

func (myGranter) GrantCredit(ctx context.Context, in authevents.CreditGrantRequest) error {
	_ = in.OrgID
	return nil
}
