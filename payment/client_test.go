package payment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/mesh"
)

func TestClient_CreateCheckoutSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/checkout/sessions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(CheckoutSession{SessionID: "s1", URL: "https://pay/x"})
	}))
	defer srv.Close()
	c, _ := New(mesh.URLs{Payment: srv.URL})
	resp, err := c.CreateCheckoutSession(context.Background(), CheckoutSessionRequest{
		OrgID: "o", PriceID: "p", SuccessURL: "s", CancelURL: "c",
	})
	if err != nil || resp.SessionID != "s1" {
		t.Errorf("err=%v resp=%+v", err, resp)
	}
}

func TestClient_VerifyUsesGET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %q (back_references C5 — must be GET)", r.Method)
		}
		if r.URL.Path != "/checkout/sessions/abc/verify" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(CheckoutSession{SessionID: "abc", Status: "complete"})
	}))
	defer srv.Close()
	c, _ := New(mesh.URLs{Payment: srv.URL})
	if _, err := c.VerifyCheckoutSession(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
}

func TestClient_ListPaymentMethods(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/customer/org1/payment_methods" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []PaymentMethod{{ID: "pm1", Brand: "visa", Last4: "4242"}},
		})
	}))
	defer srv.Close()
	c, _ := New(mesh.URLs{Payment: srv.URL})
	methods, err := c.ListPaymentMethods(context.Background(), "org1")
	if err != nil || len(methods) != 1 {
		t.Fatalf("err=%v methods=%v", err, methods)
	}
	if methods[0].Last4 != "4242" {
		t.Errorf("got %+v", methods[0])
	}
}
