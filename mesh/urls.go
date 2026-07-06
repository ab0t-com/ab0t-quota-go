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
	// Per-service consumer keys. The mesh mints a SEPARATE X-API-Key per
	// provider org — billing's key is not valid at payment and vice-versa — so
	// each client needs its own token. Both fall back to EnvServiceTok (a single
	// key that legitimately spans both, or for backward compatibility).
	EnvBillingTok = "AB0T_QUOTA_BILLING_TOKEN"
	EnvPaymentTok = "AB0T_QUOTA_PAYMENT_TOKEN"
)

// URLs is the resolved mesh-side URLs.
type URLs struct {
	Billing string
	Payment string
	Token   string // shared/fallback token (AB0T_QUOTA_SERVICE_TOKEN)
	// Per-service tokens; each falls back to Token. The billing/payment clients
	// use these so a per-org mesh key reaches the right provider.
	BillingToken string
	PaymentToken string
}

// ErrMissing is returned when a required URL isn't set.
var ErrMissing = errors.New("mesh: required URL env var not set")

// Resolve reads URLs from env. Both billing and payment are optional —
// quota.Setup turns capabilities off when they're absent.
func Resolve() URLs {
	shared := os.Getenv(EnvServiceTok)
	return URLs{
		Billing:      strings.TrimRight(os.Getenv(EnvBillingURL), "/"),
		Payment:      strings.TrimRight(os.Getenv(EnvPaymentURL), "/"),
		Token:        shared,
		BillingToken: firstNonEmpty(os.Getenv(EnvBillingTok), shared),
		PaymentToken: firstNonEmpty(os.Getenv(EnvPaymentTok), shared),
	}
}

// firstNonEmpty returns the first non-empty string (per-service token, then the
// shared fallback).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
