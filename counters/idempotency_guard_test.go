package counters

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
)

// mintActivationID mirrors activations.MintActivationID ("act_" + 32 hex)
// without importing that package (activations imports counters via engine —
// keep this test in-package and dependency-free).
func mintActivationID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "act_" + hex.EncodeToString(b[:])
}

func TestIdempotencyKeyHasUniqueComponent_Accepts(t *testing.T) {
	cases := map[string]string{
		"empty (no idempotency requested)":               "",
		"dashed uuid":                                    "counter:lifecycle:end:abc:550e8400-e29b-41d4-a716-446655440000",
		"evt marker":                                     "counter:lifecycle:end:desktop-abc123:evt:whatever",
		"trailing numeric generation":                    "counter:lifecycle:end:desktop-abc123:7",
		"generation in the middle (bundle-suffixed key)": "counter:lifecycle:end:desktop-abc123:7:sandbox.desktop_sessions",
		"gen: prefixed segment":                          "counter:lifecycle:end:desktop-abc123:gen:12",
		"opaque token_hex(16)":                           "release:" + "act_" + "0123456789abcdef0123456789abcdef",
		"dash-less uuid (32 hex)":                        "9c8f1e2a3b4c5d6e7f8091a2b3c4d5e6",
	}
	for name, key := range cases {
		if !IdempotencyKeyHasUniqueComponent(key) {
			t.Errorf("%s: key %q should be ACCEPTED (has a unique component)", name, key)
		}
	}
}

func TestIdempotencyKeyHasUniqueComponent_Flags(t *testing.T) {
	// Genuinely-bare recyclable ids — no generation/evt/uuid/opaque token.
	bare := []string{
		"counter:lifecycle:end:desktop-abc123",
		"desktop-abc123",
		"sandbox-xyz",
		"counter:lifecycle:start:browser-pool-slot",
		"pool:warm:container",
	}
	for _, key := range bare {
		if IdempotencyKeyHasUniqueComponent(key) {
			t.Errorf("bare recyclable key %q should be FLAGGED (no unique component)", key)
		}
	}
}

// The CRITICAL parity case (ticket 20260810): the library's OWN release key,
// `release:{activation_id}` where activation_id is a freshly minted
// "act_"+token_hex(16), MUST be accepted — otherwise strict mode would raise
// mid-release and leak a slot (gauge stuck high, the exact drift). Prove it
// across many minted ids, and prove a genuinely-bare id IS flagged.
func TestReleaseKeyAcceptedAndBareFlagged(t *testing.T) {
	for i := 0; i < 200; i++ {
		relKey := "release:" + mintActivationID()
		if !IdempotencyKeyHasUniqueComponent(relKey) {
			t.Fatalf("library release key %q was FLAGGED — strict mode would break engine release (slot leak / stuck-high gauge)", relKey)
		}
		if GuardIdempotencyKey(relKey) != nil {
			t.Fatalf("GuardIdempotencyKey rejected the library's own release key %q", relKey)
		}
	}
	// Contrast: a bare recyclable id is flagged.
	if IdempotencyKeyHasUniqueComponent("desktop-abc123") {
		t.Fatal("bare recyclable id desktop-abc123 should be flagged")
	}
}

func TestGuardIdempotencyKey_WarnModeNeverErrors(t *testing.T) {
	// Ensure strict is OFF for this test.
	t.Setenv(StrictIdempotencyEnv, "")
	for _, key := range []string{"", "desktop-abc123", "counter:lifecycle:end:pool-slot", "release:act_0123456789abcdef0123456789abcdef"} {
		if err := GuardIdempotencyKey(key); err != nil {
			t.Errorf("warn mode must never error; key %q returned %v", key, err)
		}
	}
}

func TestGuardIdempotencyKey_StrictMode(t *testing.T) {
	t.Setenv(StrictIdempotencyEnv, "1")
	if !StrictIdempotencyEnforcement() {
		t.Fatal("StrictIdempotencyEnforcement should be true when env=1")
	}
	// A bare recyclable id is a hard error.
	if err := GuardIdempotencyKey("desktop-abc123"); err == nil {
		t.Error("strict mode: bare recyclable key must return an error")
	}
	// A well-shaped key (incl. the library's own release key) is still accepted.
	for _, ok := range []string{"", "counter:lifecycle:end:pool:7", "release:" + mintActivationID(), "550e8400-e29b-41d4-a716-446655440000"} {
		if err := GuardIdempotencyKey(ok); err != nil {
			t.Errorf("strict mode: well-shaped key %q must pass, got %v", ok, err)
		}
	}
}

func TestStrictIdempotencyEnforcement_Values(t *testing.T) {
	for _, on := range []string{"1", "true", "TRUE", "Yes", " yes "} {
		t.Setenv(StrictIdempotencyEnv, on)
		if !StrictIdempotencyEnforcement() {
			t.Errorf("value %q should enable strict mode", on)
		}
	}
	for _, off := range []string{"", "0", "false", "no", "off"} {
		t.Setenv(StrictIdempotencyEnv, off)
		if StrictIdempotencyEnforcement() {
			t.Errorf("value %q should NOT enable strict mode", off)
		}
	}
}
