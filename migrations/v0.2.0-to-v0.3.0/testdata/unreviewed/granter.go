package example

import (
	"context"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
)

type myGranter struct{}

func (myGranter) GrantCredit(ctx context.Context, in authevents.CreditGrantRequest) error {
	_ = in.OrgID // assumed to be the org the person joined
	return nil
}
