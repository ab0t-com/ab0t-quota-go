package payment

// Ticket 20261016 review B-08 (lane B, re-verified against payment
// app/main.py:128-139, api/routes/portal.py:32-40,81, api/payment_methods.py:47-63,258-259,
// api/routes/checkout.py:~399-406). v0.1.9 called routes payment does not serve
// (/customer/portal, /customer/{org}/payment_methods) and decoded the list under
// "items" (payment sends "payment_methods") — an empty list, no error; the
// verify response's payment_status was dropped (a "complete" session can still be
// unpaid for async methods). RED on v0.1.9.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/mesh"
)

func fakePayment(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /portal/{org}/session", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["return_url"] == "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "https://billing.stripe.example/p/" + r.PathValue("org"), "id": "bps_1"})
	})
	mux.HandleFunc("GET /payment-methods/{org}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"payment_methods": []map[string]any{
			{"id": "pm_1", "type": "card", "brand": "visa", "last4": "4242", "exp_month": 12, "exp_year": 2030, "is_default": true}}})
	})
	mux.HandleFunc("GET /checkout/sessions/{id}/verify", func(w http.ResponseWriter, r *http.Request) {
		// Exactly as payment answers: amount_total in DOLLARS as a float
		// (payment api/routes/checkout.py: session.amount_total / 100).
		_, _ = w.Write([]byte(`{"session_id":"` + r.PathValue("id") + `","status":"complete",` +
			`"payment_status":"unpaid","amount_total":29.99}`))
	})
	return httptest.NewServer(mux)
}

func TestPaymentClient_RealRoutesAndShapes_20261016(t *testing.T) {
	srv := fakePayment(t)
	defer srv.Close()
	c, _ := New(mesh.URLs{Payment: srv.URL, PaymentToken: "ab0t_sk_p"})
	ctx := context.Background()

	ps, err := c.CreatePortalSession(ctx, "org-1", "https://app.example/settings")
	if err != nil || ps.URL == "" {
		t.Fatalf("CreatePortalSession: %+v %v", ps, err)
	}
	pms, err := c.ListPaymentMethods(ctx, "org-1")
	if err != nil || len(pms) != 1 || pms[0].Last4 != "4242" {
		t.Fatalf("ListPaymentMethods: %+v %v (payment sends payment_methods, not items)", pms, err)
	}
	cs, err := c.VerifyCheckoutSession(ctx, "cs_123")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cs)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["payment_status"] != "unpaid" {
		t.Fatalf("VerifyCheckoutSession dropped payment_status: %s — a complete-but-unpaid session looks paid", b)
	}
	if cs.AmountTotal == nil || cs.AmountTotal.String() != "29.99" {
		t.Fatalf("amount_total = %v, want 29.99 (dollars, as payment sends it)", cs.AmountTotal)
	}
}
