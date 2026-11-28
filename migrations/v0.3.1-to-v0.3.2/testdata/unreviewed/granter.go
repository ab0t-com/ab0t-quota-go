package example

import (
	"context"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
)

// myGranter grants the sign-up gift itself (written while the library's gift never arrived).
type myGranter struct{}

var _ authevents.CreditGranter = myGranter{}

func (myGranter) GrantCredit(ctx context.Context, in authevents.CreditGrantRequest) error {
	return nil
}
