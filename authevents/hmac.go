package authevents

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// VerifyHMAC checks the request signature against the secret.
//
// Accepts both formats:
//   - "sha256=<hex>" (canonical)
//   - "<hex>"        (CLI replay/backfill signs bare)
//
// HMAC is computed over the raw body bytes — NEVER re-canonicalize JSON
// in the receiver (PRODUCT_SPEC §11.5; Known Upstream Bug #3 cautionary).
//
// Uses hmac.Equal for constant-time comparison.
func VerifyHMAC(body []byte, signature, secret string) bool {
	if signature == "" || secret == "" {
		return false
	}
	sig := signature
	if strings.HasPrefix(sig, "sha256=") {
		sig = sig[7:]
	}
	provided, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)
	return hmac.Equal(provided, expected)
}

// SignBody computes the HMAC of body with secret. Convenience for tests
// and for the Go equivalent of the Python CLI's replay/backfill signers.
// Returns the bare hex (no `sha256=` prefix).
func SignBody(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
