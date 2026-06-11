package payment

import (
	"context"
	"errors"

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
	return &Client{http: httpx.New(u.Payment, u.Token)}, nil
}

// CreateCheckoutSession calls POST /checkout/sessions.
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
	if err := c.http.GET(ctx, "/checkout/sessions/"+sessionID+"/verify", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreatePortalSession calls POST /customer/portal.
func (c *Client) CreatePortalSession(ctx context.Context, orgID, returnURL string) (*PortalSession, error) {
	var out PortalSession
	if err := c.http.POST(ctx, "/customer/portal", map[string]string{
		"org_id":     orgID,
		"return_url": returnURL,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPaymentMethods calls GET /customer/{org_id}/payment_methods.
func (c *Client) ListPaymentMethods(ctx context.Context, orgID string) ([]PaymentMethod, error) {
	var out struct {
		Items []PaymentMethod `json:"items"`
	}
	if err := c.http.GET(ctx, "/customer/"+orgID+"/payment_methods", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}
