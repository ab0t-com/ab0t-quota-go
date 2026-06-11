package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/shopspring/decimal"
)

func TestClient_CheckQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/billing/quota/check" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(QuotaCheckResponse{Allowed: true, Used: 5})
	}))
	defer srv.Close()
	c, _ := New(mesh.URLs{Billing: srv.URL})
	resp, err := c.CheckQuota(context.Background(), QuotaCheckRequest{UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Allowed || resp.Used != 5 {
		t.Errorf("got %+v", resp)
	}
}

func TestClient_GrantCredit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/billing/credits/grant" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(CreditGrantResponse{
			GrantID: "g-1",
			Balance: decimal.NewFromInt(25),
		})
	}))
	defer srv.Close()
	c, _ := New(mesh.URLs{Billing: srv.URL})
	resp, err := c.GrantCredit(context.Background(), CreditGrantRequest{
		UserID: "u", TierID: "pro", Amount: decimal.NewFromInt(25),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GrantID != "g-1" || !resp.Balance.Equal(decimal.NewFromInt(25)) {
		t.Errorf("got %+v", resp)
	}
}

func TestClient_CancelSubscriptionUsesDELETE(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != "DELETE" {
			t.Errorf("method = %q (back_references C5 — must be DELETE)", r.Method)
		}
		if r.URL.Path != "/subscriptions/o1/s1" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	c, _ := New(mesh.URLs{Billing: srv.URL})
	if err := c.CancelSubscription(context.Background(), "o1", "s1"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("server not called")
	}
}

func TestClient_NewRequiresURL(t *testing.T) {
	if _, err := New(mesh.URLs{}); err == nil {
		t.Error("expected error without URL")
	}
}

func TestClient_GetUsageSummaryUsesCorrectPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/billing/usage/org-1/summary" {
			t.Errorf("path = %q (back_references C5 — /summary)", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(UsageSummary{OrgID: "org-1", Period: "2026-06"})
	}))
	defer srv.Close()
	c, _ := New(mesh.URLs{Billing: srv.URL})
	resp, err := c.GetUsageSummary(context.Background(), "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.OrgID != "org-1" {
		t.Errorf("got %+v", resp)
	}
}
