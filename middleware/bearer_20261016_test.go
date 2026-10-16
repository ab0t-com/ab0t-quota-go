package middleware

// Ticket 20261016 review A-24: the Authorization scheme is case-insensitive
// (RFC 9110 §11.1). "bearer <token>" was ignored, so the jwt tier provider saw
// no token and the request fell to the tier-unresolved path. RED on v0.1.9.

import (
	"net/http/httptest"
	"testing"
)

func TestBearerToken_SchemeIsCaseInsensitive_20261016(t *testing.T) {
	for _, h := range []string{"Bearer abc", "bearer abc", "BEARER abc", "Bearer  abc "} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", h)
		if got := bearerToken(r); got != "abc" {
			t.Errorf("%q → %q, want abc", h, got)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Basic abc")
	if got := bearerToken(r); got != "" {
		t.Errorf("Basic → %q, want empty", got)
	}
}
