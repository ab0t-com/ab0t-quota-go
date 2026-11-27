package quota

// v0.2.0 Setup wiring:
//   - billing wired + no outbox.sns_topic_arn + no Options.SettlementPublisher → lifecycle events go
//     to billing over HTTPS automatically (the chain is complete; before v0.2.0 Setup refused);
//   - a supplied publisher is used as before (transport "custom");
//   - the billing URL is read from BILLING_SERVICE_URL (canonical) or AB0T_QUOTA_BILLING_URL
//     (deprecated), and the capability report names the one actually read.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/outbox"
)

type billingRecorder struct {
	mu    sync.Mutex
	paths []string
	keys  []string
}

func (b *billingRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		b.mu.Lock()
		b.paths = append(b.paths, r.Method+" "+r.URL.Path)
		b.keys = append(b.keys, r.Header.Get("X-API-Key"))
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"accepted":true,"outcome":"processed"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (b *billingRecorder) snapshot() ([]string, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.paths...), append([]string(nil), b.keys...)
}

func clearBillingEnv(t *testing.T) {
	t.Helper()
	for _, n := range []string{"BILLING_SERVICE_URL", "BILLING_SERVICE_API_KEY", "PAYMENT_SERVICE_URL",
		"PAYMENT_SERVICE_API_KEY", "AB0T_QUOTA_BILLING_URL", "AB0T_QUOTA_BILLING_TOKEN", "AB0T_QUOTA_PAYMENT_URL",
		"AB0T_QUOTA_PAYMENT_TOKEN", "AB0T_QUOTA_SERVICE_TOKEN"} {
		t.Setenv(n, "")
	}
}

func TestSetup_HTTPSLifecycleTransportAutoWired_20261123(t *testing.T) {
	clearBillingEnv(t)
	rec := &billingRecorder{}
	srv := rec.server(t)
	t.Setenv("BILLING_SERVICE_URL", srv.URL)
	t.Setenv("BILLING_SERVICE_API_KEY", "ab0t_sk_billing_consumer")

	q, err := Setup(context.Background(), Options{ConfigOverride: durableRedisCfg(t), SkipBackgroundLoops: true})
	if err != nil {
		t.Fatalf("billing wired + durable outbox + no SNS topic must now start (HTTPS transport): %v", err)
	}
	defer q.Close(context.Background())
	cap := q.Capabilities()
	if !strings.HasPrefix(cap.BillingStatus, "ON") {
		t.Fatalf("BillingStatus = %q", cap.BillingStatus)
	}
	if got := cap.Resolved["lifecycle_transport"].Value; !strings.HasPrefix(got, "https") {
		t.Fatalf("lifecycle_transport = %q", got)
	}
	if got := cap.Resolved["billing_url"].Source; got != "env BILLING_SERVICE_URL" {
		t.Fatalf("billing_url source = %q", got)
	}
	if q.Outbox == nil || !q.Outbox.HasPublisher() {
		t.Fatal("the outbox must carry the HTTPS transport (meter.New accepts it)")
	}

	ev, _ := json.Marshal(map[string]any{"event_type": "resource.stopped", "org_id": "org-7", "reservation_id": "res-9"})
	delivered, err := q.Outbox.EmitViaOutbox(context.Background(), "res-9:resource.stopped",
		outbox.Record{Event: ev, EventType: "resource.stopped", ReservationID: "res-9"})
	if err != nil || !delivered {
		t.Fatalf("emit: delivered=%v err=%v", delivered, err)
	}
	paths, keys := rec.snapshot()
	found := false
	for i, p := range paths {
		if p == "POST /billing/org-7/lifecycle-events" {
			found = true
			if keys[i] != "ab0t_sk_billing_consumer" {
				t.Fatalf("the lifecycle call must carry the billing key, got %q", keys[i])
			}
		}
	}
	if !found {
		t.Fatalf("no lifecycle-events call reached billing: %v", paths)
	}
}

// A supplied publisher is used as before — HTTPS never replaces it.
func TestSetup_SuppliedPublisherKept_20261123(t *testing.T) {
	clearBillingEnv(t)
	rec := &billingRecorder{}
	srv := rec.server(t)
	t.Setenv("BILLING_SERVICE_URL", srv.URL)
	q, err := Setup(context.Background(), Options{
		ConfigOverride: durableRedisCfg(t), SettlementPublisher: fakeSettlementPublisher{}, SkipBackgroundLoops: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	if got := q.Capabilities().Resolved["lifecycle_transport"].Value; !strings.HasPrefix(got, "custom") {
		t.Fatalf("lifecycle_transport = %q", got)
	}
	ev, _ := json.Marshal(map[string]any{"event_type": "resource.stopped", "org_id": "org-7", "reservation_id": "res-9"})
	_, _ = q.Outbox.EmitViaOutbox(context.Background(), "res-9:resource.stopped",
		outbox.Record{Event: ev, EventType: "resource.stopped", ReservationID: "res-9"})
	paths, _ := rec.snapshot()
	for _, p := range paths {
		if strings.HasSuffix(p, "/lifecycle-events") {
			t.Fatalf("a supplied publisher must not be bypassed by HTTPS: %v", paths)
		}
	}
}

// Deprecated name still works; the report names it.
func TestSetup_DeprecatedBillingURLStillWorks_20261123(t *testing.T) {
	clearBillingEnv(t)
	rec := &billingRecorder{}
	srv := rec.server(t)
	t.Setenv("AB0T_QUOTA_BILLING_URL", srv.URL)
	q, err := Setup(context.Background(), Options{ConfigOverride: durableRedisCfg(t), SkipBackgroundLoops: true})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	cap := q.Capabilities()
	if !cap.Billing || cap.Resolved["billing_url"].Source != "env AB0T_QUOTA_BILLING_URL" {
		t.Fatalf("billing=%v source=%q", cap.Billing, cap.Resolved["billing_url"].Source)
	}
}

// Control: no billing wired → no HTTPS transport is invented; the paid chain still refuses on the
// missing publisher (the auto-wire needs a billing client to send to).
func TestSetup_NoBillingNoHTTPSTransport_20261123(t *testing.T) {
	clearBillingEnv(t)
	_, err := Setup(context.Background(), Options{ConfigOverride: durableRedisCfg(t), SkipBackgroundLoops: true})
	if err == nil || !strings.Contains(err.Error(), "publisher") {
		t.Fatalf("got %v", err)
	}
}
