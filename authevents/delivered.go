package authevents

import (
	"sort"
	"strings"
)

// v0.3.2 — the event types the ab0t auth service actually delivers to a webhook subscription.
//
// Why: before v0.3.2 the default sign-up gift listened on "org.created" and "user.org_assigned".
// Auth sends "org.created" to its audit trail only and has no "user.org_assigned" event type, so a
// gift wired exactly as documented never arrived — and nothing said so. These lists are the contract
// the handler, the subscription defaults and the capability report are checked against. Shared with
// the Python library (testdata/money_safety_golden_20261128.json).

// EventUserRegistered is auth's webhook event for a NEW account (data: user_id, email, org_id, role,
// registration_method[, workspace_org_id]) — the sign-up gift's event.
const EventUserRegistered = "auth.user.registered"

// DeliveredEventTypes are the only event types auth delivers to a webhook subscription (both auth
// engines), sorted. A handler on any other type never runs from a real delivery.
var DeliveredEventTypes = []string{
	"auth.agent.suspended", "auth.user.login", EventUserRegistered, EventMemberJoined, "org.member.removed",
}

// NeverDeliveredLegacyGiftEvents are the names the default sign-up gift listened on before v0.3.2.
// The receiver still accepts them (replays, backfills and ledger rows written under them keep
// working); auth never delivers them, so no subscription default names them.
var NeverDeliveredLegacyGiftEvents = []string{"org.created", "user.org_assigned"}

// SignupGiftEventTypes are the delivered types the sign-up gift listens on — what subscribe-events
// subscribes to by default: auth.user.registered, plus org.member.added when the company opted in
// (auth_events.grant_on_member_joined).
func SignupGiftEventTypes(memberJoined bool) []string {
	if memberJoined {
		return []string{EventUserRegistered, EventMemberJoined}
	}
	return []string{EventUserRegistered}
}

// IsDelivered reports whether auth delivers eventType to webhook subscriptions.
func IsDelivered(eventType string) bool {
	for _, t := range DeliveredEventTypes {
		if t == eventType {
			return true
		}
	}
	return false
}

// UndeliveredEventTypes returns the entries of types auth never delivers (sorted, unique).
func UndeliveredEventTypes(types []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range types {
		if !IsDelivered(t) && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// DeliveredOnly returns the entries of types auth delivers, in order.
func DeliveredOnly(types []string) []string {
	var out []string
	for _, t := range types {
		if IsDelivered(t) {
			out = append(out, t)
		}
	}
	return out
}

// UndeliveredSubscriptionWarning is the text both libraries print for a subscription or handler on
// types auth never delivers.
func UndeliveredSubscriptionWarning(types []string) string {
	return "auth does not deliver " + strings.Join(UndeliveredEventTypes(types), ", ") +
		" to webhook subscriptions — a handler on it never runs from a real delivery (delivered: " +
		strings.Join(DeliveredEventTypes, ", ") + ")"
}
