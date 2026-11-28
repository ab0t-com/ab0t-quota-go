package main

// v0.3.2 — `quotactl subscribe-events` subscribes to the events auth DELIVERS for the sign-up gift:
// auth.user.registered, + org.member.added when the config opts in. It defaulted to
// "org.created,user.org_assigned", which auth never delivers. These pins use only the pre-v0.3.2 API
// (they compile against v0.3.1 and go RED there by assertion).
// RED on v0.3.1: the default subscribes to the old names; --config is an unknown flag; an old-name
// subscription reads as success.
// Run: go test ./cmd/quotactl -run NeverAstrayBehaviour -count=1

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeQuotaConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "quota-config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const giftConfig = `{"storage": {"redis_url": "memory://"}, "enforcement": {"enabled": true},
  "tier_provider": {"type": "static", "default_tier": "free"},
  "tiers": [{"tier_id": "free", "display_name": "Free",
             "credit_grant": {"trigger": "signup", "amount_per_period": "5", "lifecycle": "persistent",
                              "destination": "credit_balance", "reset_on_downgrade": false}}]`

func TestNeverAstrayBehaviour_SubscribeDefaultsToTheGiftEvents_20261128(t *testing.T) {
	t.Setenv("QUOTA_CONFIG_PATH", "")
	f := &fakeAuth{token: "good"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	if _, err := runSubscribe(t, srv, "good", "s1"); err != nil {
		t.Fatal(err)
	}
	if len(f.subs) != 1 || strings.Join(f.subs[0].EventTypes, ",") != "auth.user.registered" {
		t.Fatalf("subscribed %+v — the gift needs auth.user.registered", f.subs)
	}
}

func TestNeverAstrayBehaviour_SubscribeAddsMemberJoinedWhenTheConfigOptsIn_20261128(t *testing.T) {
	p := writeQuotaConfig(t, giftConfig+`, "auth_events": {"grant_on_member_joined": true}}`)
	f := &fakeAuth{token: "good"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	if out, err := runSubscribe(t, srv, "good", "s1", "--config", p); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(f.subs) != 1 || strings.Join(f.subs[0].EventTypes, ",") != "auth.user.registered,org.member.added" {
		t.Fatalf("subscribed %+v", f.subs)
	}
}

// A subscription created from the old docs (the old names) is reported, not accepted as done.
func TestNeverAstrayBehaviour_AnOldSubscriptionIsNotDone_20261128(t *testing.T) {
	t.Setenv("QUOTA_CONFIG_PATH", "")
	f := &fakeAuth{token: "good", subs: []*fakeAuthSub{{ID: "sub_old", Endpoint: testEndpoint,
		EventTypes: []string{"org.created", "user.org_assigned"}, Secret: "s1", IsActive: true}}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	_, err := runSubscribe(t, srv, "good", "s1")
	if err == nil || !strings.Contains(err.Error(), "--rotate-secret") {
		t.Fatalf("err = %v — an old-name subscription delivers no gift and must not read as success", err)
	}
	// The remedy works: rotate re-creates it on the gift's events.
	if out, err := runSubscribe(t, srv, "good", "s1", "--rotate-secret"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(f.subs) != 1 || strings.Join(f.subs[0].EventTypes, ",") != "auth.user.registered" {
		t.Fatalf("after rotate %+v", f.subs)
	}
}

// CONTROL: an explicit --events list is honoured, with a warning for a type auth never delivers.
func TestNeverAstrayBehaviour_ExplicitEventsAreHonouredWithAWarning_20261128(t *testing.T) {
	f := &fakeAuth{token: "good"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	out, err := runSubscribe(t, srv, "good", "s1", "--events", "org.created")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.subs[0].EventTypes, ",") != "org.created" || !strings.Contains(out, "auth does not deliver org.created") {
		t.Fatalf("subs %+v out %s", f.subs, out)
	}
}
