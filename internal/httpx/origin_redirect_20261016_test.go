package httpx

// v0.1.11 (ticket .private/tickets/20261016_test_hardening_findings, Q-1): a
// redirect is followed only within the ORIGIN of the first request — scheme,
// host and port, default port filled in, case-insensitive host. The rule is the
// same as authsetup's same-origin guard. The end-to-end proof that the key never
// reaches a plaintext listener is TestKnownBug_SchemeDowngradeRedirectSendsKeyInCleartext.

import (
	"net/http"
	"net/url"
	"testing"
)

func TestSameOriginRedirectsOnly_Matrix_20261016(t *testing.T) {
	cases := []struct {
		from, to string
		follow   bool
	}{
		{"https://billing.example/a", "https://billing.example/b", true},
		{"https://billing.example/a", "https://BILLING.example:443/b", true}, // default port, host case
		{"http://billing:8002/a", "http://billing:8002/b?x=1", true},
		{"http://billing/a", "http://billing:80/b", true},
		{"https://billing.example/a", "http://billing.example/b", false},     // Q-1: downgrade
		{"https://billing.example/a", "http://billing.example:443/b", false}, // downgrade, same port number
		{"http://billing.example/a", "https://billing.example/b", true},      // upgrade on the same host: allowed (lead decision 2026-10-16 — the key already went out in plaintext; refusing would break http:// base URLs)
		{"http://billing:8002/a", "https://billing:8443/b", true},            // upgrade, other port, same host
		{"http://billing.example/a", "https://evil.example/b", false},        // an "upgrade" to ANOTHER host: refused
		{"http://billing:8002/a", "http://billing:8003/b", false},
		{"https://billing.example/a", "https://billing.example.evil/b", false},
		{"https://billing.example/a", "https://evil.example/b", false},
		{"https://billing.example/a", "ftp://billing.example/b", false},
	}
	for _, c := range cases {
		t.Run(c.from+" → "+c.to, func(t *testing.T) {
			first := mustReq(t, c.from)
			next := mustReq(t, c.to)
			err := sameOriginRedirectsOnly(next, []*http.Request{first})
			if c.follow && err != nil {
				t.Fatalf("same-origin redirect refused: %v", err)
			}
			if !c.follow && err == nil {
				t.Fatal("cross-origin redirect followed — it would carry the API key")
			}
		})
	}
}

// A same-origin hop must not launder a later hop: every hop is compared with the
// FIRST request's origin.
func TestSameOriginRedirectsOnly_ComparesWithTheFirstRequest_20261016(t *testing.T) {
	first := mustReq(t, "https://billing.example/a")
	hop := mustReq(t, "https://billing.example/b")
	down := mustReq(t, "http://billing.example/c")
	if err := sameOriginRedirectsOnly(down, []*http.Request{first, hop}); err == nil {
		t.Fatal("a downgrade after a same-origin hop was followed")
	}
}

// An upgrade cannot launder a later downgrade or a host change; after an upgrade, same-origin https hops are fine.
func TestSameOriginRedirectsOnly_UpgradeThenDowngradeRefused_20261016(t *testing.T) {
	first := mustReq(t, "http://billing.example/a")
	up := mustReq(t, "https://billing.example/b")
	if err := sameOriginRedirectsOnly(mustReq(t, "http://billing.example/c"), []*http.Request{first, up}); err == nil {
		t.Fatal("http → https → http: the downgrade after the upgrade was followed")
	}
	if err := sameOriginRedirectsOnly(mustReq(t, "https://evil.example/c"), []*http.Request{first, up}); err == nil {
		t.Fatal("http → https → another host was followed")
	}
	if err := sameOriginRedirectsOnly(mustReq(t, "https://billing.example/c"), []*http.Request{first, up}); err != nil {
		t.Fatalf("http → https → https (same origin) refused: %v", err)
	}
}

func mustReq(t *testing.T, raw string) *http.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Request{Method: http.MethodGet, URL: u, Header: http.Header{}}
}
