package counters

import (
	"errors"
	"log/slog"
	"os"
	"regexp"
	"strings"
)

// Idempotency-key contract guard (ticket 20260810, P1.1/P1.5 — Go parity with
// Python ab0t_quota/counters/gauge.py).
//
// THE BUG this defends against: a caller keys a gauge acquire/decrement on a
// bare RECYCLABLE resource/container id — e.g. `counter:lifecycle:end:{id}`
// with no per-activation component. Warm-pool reuse means a SECOND, genuinely
// DISTINCT lifecycle event (a different activation that happens to reuse the
// same container id) computes the IDENTICAL key. The claim-then-mutate is
// CORRECTLY doing its job when it treats that as a retry of the FIRST event
// and no-ops — the defect is the key, not the claim. Over many reused ids this
// silently drops the second activation (an undercount) or a decrement (gauge
// sticks high forever — the live incident this ticket fixes: org cd790b95
// "5/1" with zero running sandboxes).
//
// The fix is a caller discipline: every idempotency key must carry a component
// that CANNOT repeat across two distinct activations of the same recyclable
// id — an activation/claim GENERATION bumped on every reactivation (a trailing
// `:<int>`), a fresh UUID per event (a `:evt:<uuid4>` segment), a raw UUID, or
// an opaque high-entropy token (the library's OWN `release:{activation_id}`
// key is exactly this shape). This guard cannot enforce CORRECTNESS of that
// discipline — it can only catch the SHAPE of the mistake: a key with no such
// component at all, which is exactly the collision-bug shape. Defense-in-depth,
// not a proof.
//
// Default is WARN (many existing consumers must not break at runtime because
// of a heuristic); set AB0T_QUOTA_STRICT_IDEMPOTENCY_KEYS=1 to make a bad key a
// hard refusal instead.
var (
	uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	evtRe  = regexp.MustCompile(`(?:^|:)evt:`)
	// A colon-delimited, purely-numeric segment ANYWHERE in the key — not just
	// at the end. The engine may suffix a consumer key with `:{resource_key}`
	// before it reaches this guard (bundle acquire), so the generation a caller
	// embeds lands in the MIDDLE of the final key, not its tail.
	genSegmentRe = regexp.MustCompile(`(?:^|:)(?:gen:)?[0-9]+(?::|$)`)
	// An opaque HIGH-ENTROPY token: a run of >= 24 hex chars. This is what
	// crypto/rand hex(16) (32 hex) and a dash-less UUID (32 hex) produce — and,
	// critically, exactly the shape of the library's OWN release key
	// `release:{activation_id}` where activation_id = MintActivationID() =
	// "act_" + 32 hex (activations.MintActivationID). Such a token is minted
	// FRESH per activation and never reused, so it is the strongest possible
	// unique component — yet the three patterns above (dashed-UUID / `:evt:` /
	// numeric-generation) all MISS it: no dashes, no `evt:`, no purely-numeric
	// segment. Without this pattern the guard would raise on the library's own
	// `release:act_<hex>` key in strict mode (breaking a decrement AFTER the
	// ledger row is marked RELEASED but BEFORE the gauge decrement → the exact
	// stuck-high drift this ticket exists to prevent) and warn-spam on every
	// release. 24 hex = 96 bits of entropy: far too long to be a reused
	// resource/pool id (e.g. `desktop-abc123` has only a 6-hex run), so
	// accepting it does not weaken detection of the genuine bare-id bug.
	opaqueTokenRe = regexp.MustCompile(`[0-9a-fA-F]{24,}`)
)

// StrictIdempotencyEnv is the env var that upgrades the guard from WARN to a
// hard error. Exported so consumers/tests can reference the exact name.
const StrictIdempotencyEnv = "AB0T_QUOTA_STRICT_IDEMPOTENCY_KEYS"

// ErrRecyclableIdempotencyKey is returned by GuardIdempotencyKey in strict mode
// when an idempotency key looks like a bare recyclable resource/container id
// with no unique-per-event component.
var ErrRecyclableIdempotencyKey = errors.New("quota: idempotency key looks like a bare recyclable resource id (no activation-generation/:evt:/uuid/opaque-token component) -- exactly the shape that causes stuck-gauge collisions (ticket 20260810); key on a unique-per-lifecycle-event id instead")

// IdempotencyKeyHasUniqueComponent reports whether key carries something that
// cannot repeat across two distinct activations of the same recyclable
// resource id: a UUID, an explicit `:evt:` marker, a numeric generation, or an
// opaque high-entropy token (a >= 24-hex run — a dash-less UUID or a
// crypto/rand hex id, e.g. the library's own `release:act_<hex>` key). An
// empty key means no idempotency was requested at all — a separate, valid
// choice — and reports true. Exported for parity with Python's
// idempotency_key_has_unique_component and for consumers that want to
// pre-validate a key before calling Acquire.
func IdempotencyKeyHasUniqueComponent(key string) bool {
	if key == "" {
		return true
	}
	return uuidRe.MatchString(key) ||
		evtRe.MatchString(key) ||
		genSegmentRe.MatchString(key) ||
		opaqueTokenRe.MatchString(key)
}

// StrictIdempotencyEnforcement reports whether strict mode is enabled via the
// AB0T_QUOTA_STRICT_IDEMPOTENCY_KEYS env var.
func StrictIdempotencyEnforcement() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(StrictIdempotencyEnv))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// GuardIdempotencyKey is the contract guard (P1.5): reject-or-warn an
// idempotency key shaped like a bare recyclable id. Call on the acquire path
// before the key reaches the atomic claim. An empty key (no idempotency
// requested) and a well-shaped key both pass silently. In the default WARN
// mode a bad key logs a loud warning and returns nil; with
// AB0T_QUOTA_STRICT_IDEMPOTENCY_KEYS=1 it returns ErrRecyclableIdempotencyKey.
func GuardIdempotencyKey(key string) error {
	if key == "" || IdempotencyKeyHasUniqueComponent(key) {
		return nil
	}
	if StrictIdempotencyEnforcement() {
		return ErrRecyclableIdempotencyKey
	}
	slog.Warn("quota_idempotency_key_looks_recyclable",
		"key", key,
		"note", "no activation-generation/:evt:/uuid/opaque-token component -- a reused container/pool id "+
			"makes a genuinely NEW lifecycle event compute the SAME key as a past one, so it is silently "+
			"treated as a duplicate and the mutation is dropped (ticket 20260810 stuck-gauge collision). "+
			"Key on a unique-per-lifecycle-event id: an activation/claim generation bumped on every "+
			"reactivation (trailing ':<int>'), or a fresh UUID per event (':evt:<uuid4>'). Set "+
			StrictIdempotencyEnv+"=1 to make this a hard error instead of a warning.")
	return nil
}
