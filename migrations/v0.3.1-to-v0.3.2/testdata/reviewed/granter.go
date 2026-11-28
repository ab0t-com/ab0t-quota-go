package example

import (
	"context"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
)

// myGranter forwards the library's grant to billing; nothing else grants the gift.
type myGranter struct{}

// quotago:v0.3.2-reviewed — the only grant path; no second gift.
var _ authevents.CreditGranter = myGranter{}

func (myGranter) GrantCredit(ctx context.Context, in authevents.CreditGrantRequest) error {
	return nil
}
