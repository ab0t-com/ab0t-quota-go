package payment

import (
	"context"
	"errors"
	"net/url"

	"github.com/ab0t-com/ab0t-quota-go/internal/httpx"
	"github.com/ab0t-com/ab0t-quota-go/mesh"
)

// Client is the typed payment client.
type Client struct {
	http *httpx.Client
}

// New constructs a Client. Returns an error if the payment URL is missing.
func New(u mesh.URLs) (*Client, error) {
	if u.Payment == "" {
		return nil, errors.New("payment: AB0T_QUOTA_PAYMENT_URL not set")
	}
	return &Client{http: httpx.New(u.Payment, u.PaymentToken)}, nil
}

// CreateCheckoutSession calls POST /checkout/sessions.
//
// WARNING (ticket 20261016, B-08): the payment service serves no
// POST /checkout/sessions — this call answers 404. Payment's checkout routes are
// POST /checkout/{org_id}/session (an amount) and POST /checkout/{org_id}/plan/{plan_id}
// (a plan); their bodies differ from CheckoutSessionRequest, so this method is
// not silently re-pointed. Call those routes directly until a typed method ships.
func (c *Client) CreateCheckoutSession(ctx context.Context, in CheckoutSessionRequest) (*CheckoutSession, error) {
	var out CheckoutSession
	if err := c.http.POST(ctx, "/checkout/sessions", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// VerifyCheckoutSession calls GET /checkout/sessions/{session_id}/verify.
// Per back_references C5 — GET, not POST.
func (c *Client) VerifyCheckoutSession(ctx context.Context, sessionID string) (*CheckoutSession, error) {
	var out CheckoutSession
	if err := c.http.GET(ctx, "/checkout/sessions/"+url.PathEscape(sessionID)+"/verify", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreatePortalSession calls POST /portal/{org_id}/session (payment
// api/routes/portal.py) and returns the Stripe Customer Portal URL to redirect to.
// v0.1.10 bug fix: v0.1.9 posted to /customer/portal, which payment never served.
// The caller needs payment.read for the org.
func (c *Client) CreatePortalSession(ctx context.Context, orgID, returnURL string) (*PortalSession, error) {
	var out PortalSession
	if err := c.http.POST(ctx, "/portal/"+url.PathEscape(orgID)+"/session", map[string]string{
		"return_url": returnURL,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPaymentMethods calls GET /payment-methods/{org_id} (payment
// api/payment_methods.py). v0.1.10 bug fix: v0.1.9 called
// /customer/{org_id}/payment_methods (never served) and read the list under
// "items" — payment answers {"payment_methods": [...]}.
func (c *Client) ListPaymentMethods(ctx context.Context, orgID string) ([]PaymentMethod, error) {
	var out struct {
		PaymentMethods []PaymentMethod `json:"payment_methods"`
		Items          []PaymentMethod `json:"items"` // tolerated legacy shape
	}
	if err := c.http.GET(ctx, "/payment-methods/"+url.PathEscape(orgID), &out); err != nil {
		return nil, err
	}
	if out.PaymentMethods != nil {
		return out.PaymentMethods, nil
	}
	return out.Items, nil
}
