package quota

// Known-bug tests (2026-10-16) — FIXED in v0.1.11, so they always run now (the
// QUOTA_RUN_KNOWN_BUGS gate is gone). Each FAILED on v0.1.10.
// Ticket: .private/tickets/20261016_test_hardening_findings/TICKET.md (Q-2)

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
	"github.com/ab0t-com/ab0t-quota-go/config"
)

// Q-2: with AB0T_AUTH_WEBHOOK_SECRET unset but AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS
// set, Capabilities().WhyOff["auth_events_signed"] reported "receiver will reject
// all events with 401" — while the receiver ACCEPTS events signed with the
// previous secret. The capability report (what operators read at boot and in
// `quotactl capabilities`) stated the opposite of the behaviour.
func TestKnownBug_CapabilityClaimsRejectAllWhilePreviousSecretVerifies(t *testing.T) {
	authevents.ClearHandlers()
	defer authevents.ClearHandlers()
	srv, _ := newCreditLedger(t)
	billingEnv(t, srv.URL)
	t.Setenv("AB0T_AUTH_WEBHOOK_SECRET", "")
	t.Setenv(EnvWebhookSecretPrevious, "old")
	q := setupSignup(t, signupCfg(config.CreditDestCreditBalance, config.CreditLifecyclePersistent))
	defer q.Close(context.Background())
	why := q.Capabilities().WhyOff["auth_events_signed"]
	rec := postSigned(t, q.WebhookHandler(), authevents.WebhookPath, signupEvent("org.created", "e1", "u1", "o1"), "old")
	if rec.Code == http.StatusOK && strings.Contains(why, "reject all events") {
		t.Fatalf("capability says %q but an event signed with the previous secret was accepted (HTTP 200)", why)
	}
	// The report must still say the receiver is not fully signed-on, and name the
	// previous-secret variable that is doing the accepting.
	if !strings.Contains(why, "not set") || !strings.Contains(why, EnvWebhookSecretPrevious) {
		t.Fatalf("capability %q must say the current secret is not set and name %s", why, EnvWebhookSecretPrevious)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("an event signed with the previous secret: HTTP %d, want 200 (what the report now claims)", rec.Code)
	}
	if bad := postSigned(t, q.WebhookHandler(), authevents.WebhookPath, signupEvent("org.created", "e2", "u2", "o2"), "other"); bad.Code != http.StatusUnauthorized {
		t.Fatalf("an event signed with another secret: HTTP %d, want 401 (the report says the rest are rejected)", bad.Code)
	}
}

// Q-2 companion: with neither secret set, "reject all events" is still the report
// — and still true.
func TestCapabilityRejectAllIsTrueWithNoSecretAtAll_20261016(t *testing.T) {
	authevents.ClearHandlers()
	defer authevents.ClearHandlers()
	srv, _ := newCreditLedger(t)
	billingEnv(t, srv.URL)
	t.Setenv("AB0T_AUTH_WEBHOOK_SECRET", "")
	t.Setenv(EnvWebhookSecretPrevious, "")
	q := setupSignup(t, signupCfg(config.CreditDestCreditBalance, config.CreditLifecyclePersistent))
	defer q.Close(context.Background())
	why := q.Capabilities().WhyOff["auth_events_signed"]
	if !strings.Contains(why, "reject all events") {
		t.Fatalf("no secret at all: capability %q, want it to say the receiver rejects all events", why)
	}
	for _, secret := range []string{"", "old", "anything"} {
		if rec := postSigned(t, q.WebhookHandler(), authevents.WebhookPath, signupEvent("org.created", "e-"+secret, "u1", "o1"), secret); rec.Code != http.StatusUnauthorized {
			t.Fatalf("no secret configured, event signed with %q: HTTP %d, want 401", secret, rec.Code)
		}
	}
}
