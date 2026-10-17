package httpx

// Known-bug tests (2026-10-16) — FIXED in v0.1.11, so they always run now (the
// QUOTA_RUN_KNOWN_BUGS gate is gone). Each FAILED on v0.1.10.
// Ticket: .private/tickets/20261016_test_hardening_findings/TICKET.md (Q-1)

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Q-1: the redirect guard compared HOST only, so an https base URL that redirects
// to http:// on the same host was followed — and the X-API-Key went out over a
// plaintext connection. v0.1.11 compares the origin (scheme + host + port).
func TestKnownBug_SchemeDowngradeRedirectSendsKeyInCleartext(t *testing.T) {
	var plaintextKey atomic.Value
	plaintextKey.Store("")
	// The "port 80" of the same host: a plain listener that records what arrives in clear.
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plaintextKey.Store(r.Header.Get("X-API-Key"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer sink.Close()
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/downgraded", http.StatusFound) // same host, scheme http
	}))
	defer front.Close()

	c := New(front.URL, testKey)
	// Route by SCHEME to the two listeners while the URL host stays identical: TLS
	// dials reach the TLS front, plain dials reach the sink (as :443 / :80 would).
	c.http.Transport = &http.Transport{
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return tls.Dial(network, front.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test double
		},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, sink.Listener.Addr().String())
		},
	}
	err := c.GET(context.Background(), "/billing/o1/summary", nil)
	if k := plaintextKey.Load().(string); k != "" {
		t.Fatalf("the mesh API key %q was sent over plaintext HTTP after an https→http same-host redirect", k)
	}
	if err == nil || !strings.Contains(err.Error(), "another origin") {
		t.Fatalf("an https→http redirect must be refused with an error naming the origin rule, got %v", err)
	}
}
