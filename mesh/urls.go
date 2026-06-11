// Package mesh resolves service URLs from env vars. Lives between
// consumer code and the billing/payment clients. Same env-var names as
// the Python lib so a Python→Go switch is a no-op for ops.
package mesh

import (
	"errors"
	"os"
	"strings"
)

// AB0T env-var names. Mirror Python lib.
const (
	EnvBillingURL = "AB0T_QUOTA_BILLING_URL"
	EnvPaymentURL = "AB0T_QUOTA_PAYMENT_URL"
	EnvServiceTok = "AB0T_QUOTA_SERVICE_TOKEN"
)

// URLs is the resolved mesh-side URLs.
type URLs struct {
	Billing string
	Payment string
	Token   string
}

// ErrMissing is returned when a required URL isn't set.
var ErrMissing = errors.New("mesh: required URL env var not set")

// Resolve reads URLs from env. Both billing and payment are optional —
// quota.Setup turns capabilities off when they're absent.
func Resolve() URLs {
	return URLs{
		Billing: strings.TrimRight(os.Getenv(EnvBillingURL), "/"),
		Payment: strings.TrimRight(os.Getenv(EnvPaymentURL), "/"),
		Token:   os.Getenv(EnvServiceTok),
	}
}
