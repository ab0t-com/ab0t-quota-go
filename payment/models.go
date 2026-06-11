// Package payment is the typed client for the ab0t payment service.
package payment

import "github.com/shopspring/decimal"

// CheckoutSession is the response from POST /checkout/sessions.
type CheckoutSession struct {
	SessionID string `json:"session_id"`
	URL       string `json:"url"`
	Status    string `json:"status,omitempty"`
}

// CheckoutSessionRequest is the create-checkout body.
type CheckoutSessionRequest struct {
	OrgID      string          `json:"org_id"`
	PriceID    string          `json:"price_id"`
	SuccessURL string          `json:"success_url"`
	CancelURL  string          `json:"cancel_url"`
	Quantity   int             `json:"quantity,omitempty"`
	Topup      *TopupAmount    `json:"topup,omitempty"`
}

// TopupAmount is the optional bundled top-up payload.
type TopupAmount struct {
	Amount   decimal.Decimal `json:"amount"`
	Currency string          `json:"currency,omitempty"`
}

// PortalSession is the response from POST /customer/portal.
type PortalSession struct {
	URL string `json:"url"`
}

// PaymentMethod is one stored payment method (PCI fields stripped by service).
type PaymentMethod struct {
	ID    string `json:"id"`
	Brand string `json:"brand"`
	Last4 string `json:"last4"`
	ExpMonth int `json:"exp_month"`
	ExpYear  int `json:"exp_year"`
	IsDefault bool `json:"is_default,omitempty"`
}
