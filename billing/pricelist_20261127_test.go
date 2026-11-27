package billing

// v0.3.0 — PUT /billing/price-list/{service}: the bytes go out unchanged, the billing key rides
// X-API-Key, and every answer is classified like the Python library (shared vectors
// testdata/price_list_golden_20261127.json "http_outcomes"). RED before v0.3.0: PublishPriceList /
// ClassifyPriceListStatus do not exist.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/mesh"
)

func TestPriceList_HTTPOutcomesMatchTheSharedVectors_20261127(t *testing.T) {
	raw, err := os.ReadFile("../testdata/price_list_golden_20261127.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		HTTPOutcomes []struct {
			Status  int    `json:"status"`
			Outcome string `json:"outcome"`
		} `json:"http_outcomes"`
	}
	if err := json.Unmarshal(raw, &g); err != nil || len(g.HTTPOutcomes) == 0 {
		t.Fatalf("golden: %v", err)
	}
	for _, row := range g.HTTPOutcomes {
		if got := ClassifyPriceListStatus(row.Status); got != row.Outcome {
			t.Errorf("status %d → %s, want %s", row.Status, got, row.Outcome)
		}
	}
}

func TestPublishPriceList_SendsTheBytesUnchangedWithTheBillingKey_20261127(t *testing.T) {
	body := []byte(`{"products":{"render_job":{"variants":{"cpu":{"allocation_price":"0","default":true,"price_per_hour":"0.5"}}}},"published_by":"ab0t-quota-go/t","schema":"ab0t.price_list.v1","service_name":"acme-render","version":"sha256:x","note":"<&>"}`)
	var gotMethod, gotPath, gotKey string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotKey = r.Method, r.URL.Path, r.Header.Get("X-API-Key")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"status":"stored"}`))
	}))
	defer srv.Close()
	c, err := New(mesh.URLs{Billing: srv.URL, BillingToken: "ab0t_sk_acme"})
	if err != nil {
		t.Fatal(err)
	}
	err = c.PublishPriceList(context.Background(), "acme-render", body)
	if outcome, _ := PriceListOutcome(err); outcome != PriceListPublished {
		t.Fatalf("outcome %s err %v", outcome, err)
	}
	if gotMethod != http.MethodPut || gotPath != "/billing/price-list/acme-render" || gotKey != "ab0t_sk_acme" {
		t.Fatalf("%s %s key %q", gotMethod, gotPath, gotKey)
	}
	if string(gotBody) != string(body) {
		t.Fatalf("the body was re-encoded:\n got %s\nwant %s", gotBody, body)
	}
}

func TestPriceListOutcome_StatusAndTransportErrors_20261127(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{{404, PriceListUnsupported}, {403, PriceListRefused}, {503, PriceListRetry}, {429, PriceListRetry}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status) }))
		c, _ := New(mesh.URLs{Billing: srv.URL, BillingToken: "ab0t_sk_x"})
		outcome, status := PriceListOutcome(c.PublishPriceList(context.Background(), "svc", []byte(`{}`)))
		srv.Close()
		if outcome != tc.want || status != tc.status {
			t.Errorf("status %d → %s/%d, want %s", tc.status, outcome, status, tc.want)
		}
	}
	// billing down (nothing listening) → retry, no status
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	c, _ := New(mesh.URLs{Billing: url, BillingToken: "ab0t_sk_x"})
	if outcome, status := PriceListOutcome(c.PublishPriceList(context.Background(), "svc", []byte(`{}`))); outcome != PriceListRetry || status != 0 {
		t.Fatalf("transport error → %s/%d, want retry/0", outcome, status)
	}
}
