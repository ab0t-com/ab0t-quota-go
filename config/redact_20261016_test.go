package config

// Ticket 20261016 review C-06 (lane C, re-verified at resolve.go:104-106): a
// redis_url that does not parse — e.g. a base64 password containing "/" — was
// returned VERBATIM by RedactURL, i.e. with the password, into the startup log,
// `quotactl capabilities` (Resolved.redis_url) and the error text. RED on v0.1.9.

import (
	"strings"
	"testing"
)

func TestRedactURL_NeverReturnsCredentialsOfAnUnparseableURL_20261016(t *testing.T) {
	for _, raw := range []string{
		"redis://:s3cr/et+base64==@cache.internal:6379/0",
		"redis://user:pa ss@cache.internal:6379",
		"rediss://default:p%zz@cache.internal:6380",
	} {
		got := RedactURL(raw)
		for _, frag := range []string{"s3cr", "pa ss", "p%zz"} {
			if strings.Contains(got, frag) {
				t.Errorf("RedactURL(%q) = %q — leaks the password", raw, got)
			}
		}
		if !strings.Contains(got, "cache.internal") && !strings.Contains(got, "unparseable") {
			t.Errorf("RedactURL(%q) = %q — should keep the host or say it is unparseable", raw, got)
		}
	}
	if got := RedactURL("redis://cache.internal:6379/0"); got != "redis://cache.internal:6379/0" {
		t.Errorf("credential-free URL changed: %q", got)
	}
}
