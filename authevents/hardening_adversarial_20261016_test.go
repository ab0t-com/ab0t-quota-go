package authevents

// hardening_adversarial_20261016_test.go — black-box attacks on the v0.1.10
// webhook receiver (owner instruction 2026-10-16: "robust testing, no
// assumptions"). Every case asserts the status AND whether a handler ran — a
// receiver that answered 401 but still dispatched would pass a status-only test.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func countingRouter(t *testing.T, cfg ReceiverConfig) (http.Handler, *atomic.Int32, *[]Event) {
	t.Helper()
	reg := NewRegistry()
	var n atomic.Int32
	var seen []Event
	reg.OnAuthEvent("org.created", HandlerFunc(func(_ context.Context, e Event) error {
		n.Add(1)
		seen = append(seen, e)
		return nil
	}))
	cfg.Registry = reg
	return MakeRouter(cfg), &n, &seen
}

func rawSig(body []byte, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func post(h http.Handler, path string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHardening_SignatureMatrix(t *testing.T) {
	const secret = "s3cret-current"
	body := []byte(`{"event_type":"org.created","event_id":"e1","data":{"user_id":"u1","org_id":"o1"}}`)
	good := rawSig(body, secret)
	flipped := append([]byte(nil), body...)
	flipped[len(flipped)-3] ^= 0x01
	other := []byte(`{"event_type":"org.created","event_id":"e2","data":{"user_id":"attacker","org_id":"o1"}}`)
	cases := []struct {
		name string
		body []byte
		hdr  map[string]string
		ok   bool
	}{
		{"canonical sha256= prefix", body, map[string]string{"X-Event-Signature": "sha256=" + good}, true},
		{"bare hex", body, map[string]string{"X-Event-Signature": good}, true},
		{"upper-case hex", body, map[string]string{"X-Event-Signature": "sha256=" + strings.ToUpper(good)}, true},
		{"legacy header", body, map[string]string{"X-Webhook-Signature": "sha256=" + good}, true},
		{"missing header", body, nil, false},
		{"empty header", body, map[string]string{"X-Event-Signature": ""}, false},
		{"empty after prefix", body, map[string]string{"X-Event-Signature": "sha256="}, false},
		{"SHA256= prefix", body, map[string]string{"X-Event-Signature": "SHA256=" + good}, false},
		{"sha1= prefix", body, map[string]string{"X-Event-Signature": "sha1=" + good}, false},
		{"leading space", body, map[string]string{"X-Event-Signature": " sha256=" + good}, false},
		{"truncated (31 bytes)", body, map[string]string{"X-Event-Signature": good[:62]}, false},
		{"extended (+1 byte)", body, map[string]string{"X-Event-Signature": good + "00"}, false},
		{"odd-length hex", body, map[string]string{"X-Event-Signature": good[:63]}, false},
		{"non-hex", body, map[string]string{"X-Event-Signature": strings.Repeat("zz", 32)}, false},
		{"all-zero", body, map[string]string{"X-Event-Signature": strings.Repeat("0", 64)}, false},
		{"wrong secret", body, map[string]string{"X-Event-Signature": rawSig(body, "s3cret-curren")}, false},
		{"secret with suffix", body, map[string]string{"X-Event-Signature": rawSig(body, secret+"x")}, false},
		{"tampered body (1 bit)", flipped, map[string]string{"X-Event-Signature": good}, false},
		{"body + trailing newline", append(append([]byte(nil), body...), '\n'), map[string]string{"X-Event-Signature": good}, false},
		{"signature of another body", other, map[string]string{"X-Event-Signature": good}, false},
		{"HMAC of body with empty key", body, map[string]string{"X-Event-Signature": rawSig(body, "")}, false},
		{"plain SHA-256 of body (no key)", body, map[string]string{"X-Event-Signature": func() string { s := sha256.Sum256(body); return hex.EncodeToString(s[:]) }()}, false},
		// X-Event-Signature present (wrong) takes precedence; a correct legacy header does not rescue it.
		{"wrong primary + right legacy", body, map[string]string{"X-Event-Signature": strings.Repeat("ab", 32), "X-Webhook-Signature": good}, false},
		{"right primary + wrong legacy", body, map[string]string{"X-Event-Signature": good, "X-Webhook-Signature": "junk"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, n, _ := countingRouter(t, ReceiverConfig{Secret: secret})
			rec := post(h, WebhookPath, c.body, c.hdr)
			if c.ok {
				if rec.Code != 200 || n.Load() != 1 {
					t.Fatalf("code=%d ran=%d body=%s, want 200 and the handler run once", rec.Code, n.Load(), rec.Body)
				}
				return
			}
			if rec.Code != http.StatusUnauthorized || n.Load() != 0 {
				t.Fatalf("code=%d ran=%d, want 401 and NO handler", rec.Code, n.Load())
			}
			if strings.TrimSpace(rec.Body.String()) != `{"detail":"invalid signature"}` {
				t.Fatalf("401 body %q, want the static detail", rec.Body.String())
			}
		})
	}
}

func TestHardening_SignatureCheckedBeforeJSON_NoParserOracle(t *testing.T) {
	h, n, _ := countingRouter(t, ReceiverConfig{Secret: "k"})
	for _, b := range [][]byte{[]byte(`not json`), []byte(`{"event_type":`), {}, []byte(`null`)} {
		rec := post(h, WebhookPath, b, map[string]string{"X-Event-Signature": "sha256=" + strings.Repeat("00", 32)})
		if rec.Code != http.StatusUnauthorized || n.Load() != 0 {
			t.Fatalf("body %q unsigned: code=%d — an unauthenticated caller must not learn whether the JSON parses", b, rec.Code)
		}
	}
	// Signed garbage: now the parse result may be revealed.
	b := []byte(`not json`)
	if rec := post(h, WebhookPath, b, map[string]string{"X-Event-Signature": rawSig(b, "k")}); rec.Code != http.StatusBadRequest {
		t.Fatalf("signed non-JSON: %d, want 400", rec.Code)
	}
}

func TestHardening_EmptySecretNeverVerifiesAnything(t *testing.T) {
	body := []byte(`{"event_type":"org.created","event_id":"e1","data":{"user_id":"u1"}}`)
	for _, cfg := range []ReceiverConfig{
		{Secret: ""},
		{Secret: "", PreviousSecrets: []string{"", ""}},
	} {
		h, n, _ := countingRouter(t, cfg)
		for _, sig := range []string{"", "sha256=", rawSig(body, ""), "sha256=" + rawSig(body, "")} {
			rec := post(h, WebhookPath, body, map[string]string{"X-Event-Signature": sig})
			if rec.Code != http.StatusUnauthorized || n.Load() != 0 {
				t.Fatalf("cfg=%+v sig=%q: code=%d ran=%d — an unset secret must reject everything", cfg, sig, rec.Code, n.Load())
			}
		}
	}
}

func TestHardening_PreviousSecretOnlyWhenConfigured(t *testing.T) {
	body := []byte(`{"event_type":"org.created","event_id":"e1","data":{"user_id":"u1"}}`)
	h, n, _ := countingRouter(t, ReceiverConfig{Secret: "new"})
	if rec := post(h, WebhookPath, body, map[string]string{"X-Event-Signature": rawSig(body, "old")}); rec.Code != 401 || n.Load() != 0 {
		t.Fatalf("old secret with no PreviousSecrets configured: code=%d ran=%d, want 401", rec.Code, n.Load())
	}
	h2, n2, _ := countingRouter(t, ReceiverConfig{Secret: "new", PreviousSecrets: []string{"old"}})
	if rec := post(h2, WebhookPath, body, map[string]string{"X-Event-Signature": rawSig(body, "old")}); rec.Code != 200 || n2.Load() != 1 {
		t.Fatalf("old secret configured as previous: code=%d", rec.Code)
	}
	if rec := post(h2, WebhookPath, body, map[string]string{"X-Event-Signature": rawSig(body, "older")}); rec.Code != 401 {
		t.Fatalf("a secret that is neither current nor previous: %d", rec.Code)
	}
}

func TestHardening_BodySizeBoundaryIsInclusive(t *testing.T) {
	for _, max := range []int64{100, DefaultMaxBodyBytes} {
		cfg := ReceiverConfig{Secret: "k"}
		if max != DefaultMaxBodyBytes {
			cfg.MaxBodyBytes = max
		}
		h, _, _ := countingRouter(t, cfg)
		prefix := []byte(`{"event_type":"x.none","pad":"`)
		mk := func(n int64) []byte {
			b := append([]byte(nil), prefix...)
			b = append(b, bytes.Repeat([]byte("p"), int(n)-len(prefix)-2)...)
			return append(b, '"', '}')
		}
		at, over := mk(max), mk(max+1)
		if int64(len(at)) != max || int64(len(over)) != max+1 {
			t.Fatalf("construction: %d / %d", len(at), len(over))
		}
		if rec := post(h, WebhookPath, at, map[string]string{"X-Event-Signature": rawSig(at, "k")}); rec.Code != 200 {
			t.Fatalf("max=%d: a body of exactly the limit got %d, want 200", max, rec.Code)
		}
		if rec := post(h, WebhookPath, over, map[string]string{"X-Event-Signature": rawSig(over, "k")}); rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("max=%d: limit+1 got %d, want 413", max, rec.Code)
		}
	}
}

func TestHardening_PathAndMethodMatrix(t *testing.T) {
	body := []byte(`{"event_type":"org.created","event_id":"e1","data":{"user_id":"u1"}}`)
	sig := map[string]string{"X-Event-Signature": rawSig(body, "k")}
	h, _, _ := countingRouter(t, ReceiverConfig{Secret: "k"})
	for path, want := range map[string]int{
		WebhookPath:                       200,
		"/api/quotas" + WebhookPath:       200,
		"/a/b/c" + WebhookPath:            200,
		WebhookPath + "/":                 404,
		WebhookPath + "x":                 404,
		"/api/quotas/x_webhooks/auth":     404,
		"/_webhooks/Auth":                 404,
		"/_webhooks":                      404,
		"/":                               404,
		"/_webhooks/auth/../../admin":     404,
		"/api/quotas/_webhooks/auth%2F..": 404,
	} {
		if rec := post(h, path, body, sig); rec.Code != want {
			t.Errorf("POST %s: %d, want %d", path, rec.Code, want)
		}
	}
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodHead, http.MethodOptions} {
		req := httptest.NewRequest(m, WebhookPath, bytes.NewReader(body))
		req.Header.Set("X-Event-Signature", rawSig(body, "k"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
			t.Errorf("%s: %d Allow=%q, want 405 Allow: POST", m, rec.Code, rec.Header().Get("Allow"))
		}
	}
}

func TestHardening_HandlerSeesTheExactSignedBytes(t *testing.T) {
	// HMAC is over the raw body; the handler must get those same bytes (no re-canonicalisation).
	body := []byte("{ \"data\" : {\"org_id\":\"o1\" ,\"user_id\":\"u1\"},\n\"event_id\":\"e1\",\"event_type\":\"org.created\" }")
	h, n, seen := countingRouter(t, ReceiverConfig{Secret: "k"})
	if rec := post(h, WebhookPath, body, map[string]string{"X-Event-Signature": rawSig(body, "k")}); rec.Code != 200 || n.Load() != 1 {
		t.Fatalf("code=%d", rec.Code)
	}
	if got := (*seen)[0].Raw(); !bytes.Equal(got, body) {
		t.Fatalf("handler raw = %q, want the exact signed bytes", got)
	}
}
