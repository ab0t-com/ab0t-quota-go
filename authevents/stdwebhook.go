package authevents

// Standard Webhooks verification (v0.2.0, additive) — the auth service's webhook-* headers.
//
// Auth signs every delivery twice: X-Event-Signature over the body only (no timestamp, so no replay
// protection; see VerifyHMAC), and the Standard Webhooks headers (standardwebhooks.com):
//
//	webhook-id        = the event id
//	webhook-timestamp = unix seconds of THIS attempt
//	webhook-signature = space-separated "v1,<base64 HMAC-SHA256(key, "{id}.{timestamp}.{body}")>" tokens —
//	                    the current secret first, then the previous one while a rotation window is open.
//
// Key: strip "whsec_" and base64-decode (standard alphabet, padded; CR/LF ignored); if that is not valid
// base64, the secret's bytes are the key. Same contract as the Python library
// (ab0t_quota.auth_events.check_standard_webhook); shared golden testdata/webhook_signature_golden_20261126.json.
// Implemented with the standard library only (no dependency on an events package).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// Standard Webhooks header names.
const (
	StandardHeaderID        = "webhook-id"
	StandardHeaderTimestamp = "webhook-timestamp"
	StandardHeaderSignature = "webhook-signature"
)

// DefaultStandardTolerance is Standard Webhooks' default: a timestamp within 5 minutes of now, either side.
const DefaultStandardTolerance = 5 * time.Minute

// Reasons VerifyStandardWebhook returns ("" = verified).
const (
	ReasonMalformedTimestamp = "malformed_timestamp"
	ReasonOutOfTolerance     = "timestamp_out_of_tolerance"
	ReasonSignatureMismatch  = "signature_mismatch"
)

// StandardWebhookKey is the HMAC key of a Standard Webhooks secret (the auth service's rule).
func StandardWebhookKey(secret string) []byte {
	body := strings.TrimPrefix(secret, "whsec_")
	if b, err := base64.StdEncoding.DecodeString(body); err == nil {
		return b
	}
	return []byte(secret)
}

// VerifyStandardWebhook verifies one Standard Webhooks delivery against the configured secrets
// (empty entries are skipped). It returns "" when it verifies, else the reason, checked in this order:
// ReasonMalformedTimestamp (not 1-15 ASCII digits), ReasonOutOfTolerance (|now − timestamp| > tolerance
// — a replay or a skewed clock; exactly at the tolerance is accepted), ReasonSignatureMismatch (no "v1"
// token matches). Comparison is constant-time.
func VerifyStandardWebhook(body []byte, id, timestamp, signatureHeader string, secrets []string,
	now time.Time, tolerance time.Duration) string {
	if len(timestamp) == 0 || len(timestamp) > 15 || !asciiDigits(timestamp) {
		return ReasonMalformedTimestamp
	}
	secs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ReasonMalformedTimestamp
	}
	delta := now.Sub(time.Unix(secs, 0))
	if delta < 0 {
		delta = -delta
	}
	if delta > tolerance {
		return ReasonOutOfTolerance
	}
	var tokens [][]byte
	for _, tok := range strings.Fields(signatureHeader) {
		version, sig, ok := strings.Cut(tok, ",")
		if ok && version == "v1" {
			tokens = append(tokens, []byte(sig))
		}
	}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		mac := hmac.New(sha256.New, StandardWebhookKey(s))
		mac.Write([]byte(id))
		mac.Write([]byte("."))
		mac.Write([]byte(timestamp))
		mac.Write([]byte("."))
		mac.Write(body)
		want := []byte(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		for _, t := range tokens {
			if hmac.Equal(want, t) {
				return ""
			}
		}
	}
	return ReasonSignatureMismatch
}

func asciiDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
