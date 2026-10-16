// Package payment is the typed client for the ab0t payment service.
package payment

import "github.com/shopspring/decimal"

// CheckoutSession is a checkout session (VerifyCheckoutSession's reply).
type CheckoutSession struct {
	SessionID string `json:"session_id"`
	URL       string `json:"url"`
	Status    string `json:"status,omitempty"`
	// PaymentStatus (v0.1.10) is Stripe's payment_status ("paid" | "unpaid" |
	// "no_payment_required"). Fulfil on PaymentStatus == "paid", never on
	// Status == "complete" alone: an async payment method completes the session
	// before the money arrives.
	PaymentStatus string `json:"payment_status,omitempty"`
	// AmountTotal is in DOLLARS (major units): payment converts Stripe's cents
	// before answering (payment api/routes/checkout.py, verify). nil = not reported.
	AmountTotal *decimal.Decimal `json:"amount_total,omitempty"`
}

// CheckoutSessionRequest is the create-checkout body.
type CheckoutSessionRequest struct {
	OrgID      string       `json:"org_id"`
	PriceID    string       `json:"price_id"`
	SuccessURL string       `json:"success_url"`
	CancelURL  string       `json:"cancel_url"`
	Quantity   int          `json:"quantity,omitempty"`
	Topup      *TopupAmount `json:"topup,omitempty"`
}

// TopupAmount is the optional bundled top-up payload.
type TopupAmount struct {
	Amount   decimal.Decimal `json:"amount"`
	Currency string          `json:"currency,omitempty"`
}

// PortalSession is the response from POST /portal/{org_id}/session.
type PortalSession struct {
	URL string `json:"url"`
	ID  string `json:"id,omitempty"` // v0.1.10
}

// PaymentMethod is one stored payment method (PCI fields stripped by service).
type PaymentMethod struct {
	ID        string `json:"id"`
	Brand     string `json:"brand"`
	Last4     string `json:"last4"`
	ExpMonth  int    `json:"exp_month"`
	ExpYear   int    `json:"exp_year"`
	IsDefault bool   `json:"is_default,omitempty"`
	Type      string `json:"type,omitempty"` // v0.1.10
}
