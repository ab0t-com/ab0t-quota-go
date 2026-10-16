package authevents

// Ticket 20261016_signup_credit_receiver_404_and_client_gaps, finding 1.
//
// v0.1.9: MakeRouter's inner ServeMux matched only "POST /_webhooks/auth", so the
// DOCUMENTED mount — mux.Handle("/api/quotas"+WebhookPath, q.WebhookHandler()), the
// form in examples/with_auth_events and examples/with_idempotent, and the endpoint
// SubscribeOnStartup registers with auth (PublicURL + "/api" + "/quotas" + WebhookPath)
// — answered "404 page not found" to every event. No sign-up credit was ever granted
// and nothing errored loudly. Only http.StripPrefix("/api/quotas", ...) worked.
//
// These tests mount the receiver EXACTLY as a consumer does (on their own ServeMux)
// and prove a signed event reaches the handler and grants credit. RED on v0.1.9.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
)

const documentedMount = "/api/quotas" + WebhookPath // "/api/quotas/_webhooks/auth"

func signedPost(t *testing.T, url string, body []byte, secret string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Event-Signature", "sha256="+SignBody(body, secret))
	return req
}

// The documented full-path mount receives a signed org.created and the default
// credit-grant handler grants exactly once.
func TestReceiver_DocumentedFullPathMount_GrantsCredit_20261016(t *testing.T) {
	store := handlerledger.NewInMemoryLedgerStore()
	gr := &stubGranter{}
	h, err := BuildDefaultCreditGrantHandler(CreditGrantDeps{
		Config:       newCreditCfg(t),
		TierProvider: stubTierProvider{tier: "pro"},
		Granter:      gr,
		Ledger:       store,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	reg.OnAuthEvent("org.created", h)
	receiver := MakeRouter(ReceiverConfig{Secret: "shh", Registry: reg, LedgerStore: store})

	mux := http.NewServeMux()
	mux.Handle(documentedMount, receiver) // the documented, un-stripped mount

	body := []byte(`{"event_type":"org.created","event_id":"evt-mount-1","data":{"user_id":"u1","org_id":"o1"}}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, signedPost(t, documentedMount, body, "shh"))

	if rec.Code != http.StatusOK {
		t.Fatalf("documented mount %s: got HTTP %d %q, want 200 (v0.1.9 answered 404 to every event)",
			documentedMount, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if !strings.Contains(rec.Body.String(), `"ran":1`) {
		t.Fatalf("handler did not run: body=%s", rec.Body.String())
	}
	if gr.calls.Load() != 1 {
		t.Fatalf("credit granted %d times, want 1", gr.calls.Load())
	}
}

// Every mount shape a consumer uses must reach the handler: the full path, a
// subtree mount, and the StripPrefix form the v0.1.9 workaround documents (it must
// keep working so nobody has to change a working deployment).
func TestReceiver_AllMountShapesReachHandler_20261016(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		handler func(http.Handler) http.Handler
		url     string
	}{
		{"full path (documented)", documentedMount, func(h http.Handler) http.Handler { return h }, documentedMount},
		{"subtree /api/quotas/", "/api/quotas/", func(h http.Handler) http.Handler { return h }, documentedMount},
		{"StripPrefix (v0.1.9 workaround)", "/api/quotas/", func(h http.Handler) http.Handler {
			return http.StripPrefix("/api/quotas", h)
		}, documentedMount},
		{"bare (receiver as the whole server)", "/", func(h http.Handler) http.Handler { return h }, WebhookPath},
		{"other prefix /v1/hooks/quotas", "/v1/hooks/quotas" + WebhookPath, func(h http.Handler) http.Handler { return h },
			"/v1/hooks/quotas" + WebhookPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			var calls atomic.Int32
			reg.OnAuthEvent("org.created", HandlerFunc(func(context.Context, Event) error {
				calls.Add(1)
				return nil
			}))
			mux := http.NewServeMux()
			mux.Handle(tc.pattern, tc.handler(MakeRouter(ReceiverConfig{Secret: "shh", Registry: reg})))
			body := []byte(`{"event_type":"org.created","event_id":"e1","data":{"user_id":"u1","org_id":"o1"}}`)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, signedPost(t, tc.url, body, "shh"))
			if rec.Code != http.StatusOK || calls.Load() != 1 {
				t.Fatalf("%s: HTTP %d body=%q calls=%d, want 200 and 1 call",
					tc.name, rec.Code, strings.TrimSpace(rec.Body.String()), calls.Load())
			}
		})
	}
}

// The receiver still answers ONLY its own path and ONLY POST: an unrelated path
// under a subtree mount is a 404, and GET on the webhook path is a 405 with Allow.
func TestReceiver_StillScopedToWebhookPathAndPOST_20261016(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/api/quotas/", MakeRouter(ReceiverConfig{Secret: "shh", Registry: NewRegistry()}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/quotas/something-else", strings.NewReader(`{}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unrelated path: got %d, want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/quotas/x_webhooks/auth", strings.NewReader(`{}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("look-alike path without a segment boundary: got %d, want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, documentedMount, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: got %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("GET: Allow = %q, want POST", allow)
	}
}
