package authevents

// Edge cases (v0.2.0): webhook signature verification at the edges. The table is
// testdata/webhook_signature_golden_20261126.json, an identical copy of the Python library's: the auth
// service's Standard Webhooks vectors, rotated secret lists, the tolerance edge on both sides, timestamp
// and header syntax, and the legacy X-Event-Signature forms. Then the receiver: rotation, the tolerance
// edge, and the replay case (stale webhook-* headers next to a valid X-Event-Signature). Rows marked RED
// were wrong before this change (there was no Standard Webhooks verification); every gate has a control.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

type webhookGolden struct {
	Tolerance int `json:"tolerance_seconds"`
	Vectors   []struct {
		Note, ID, Timestamp, Body, Secret, Signature string
	} `json:"standard_vectors"`
	Verify []struct {
		Note, ID, Timestamp, Body, Header, Now, Expect string
		Secrets                                        []string
	} `json:"verify"`
	Legacy struct {
		Body, Secret string
		Cases        []struct {
			Note, Signature string
			Valid           bool
		}
	} `json:"legacy"`
}

func loadWebhookGolden(t *testing.T) webhookGolden {
	t.Helper()
	raw, err := os.ReadFile("../testdata/webhook_signature_golden_20261126.json")
	if err != nil {
		t.Fatal(err)
	}
	var g webhookGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Vectors) == 0 || len(g.Verify) == 0 || len(g.Legacy.Cases) == 0 || g.Tolerance == 0 {
		t.Fatal("webhook golden is missing sections")
	}
	return g
}

// goldenNow reads a decimal-seconds string ("1795140300.5") as an exact time.
func goldenNow(t *testing.T, s string) time.Time {
	t.Helper()
	whole, frac, _ := strings.Cut(s, ".")
	sec, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		t.Fatalf("now %q", s)
	}
	nanos := int64(0)
	if frac != "" {
		frac = (frac + "000000000")[:9]
		if nanos, err = strconv.ParseInt(frac, 10, 64); err != nil {
			t.Fatalf("now %q", s)
		}
	}
	return time.Unix(sec, nanos)
}

func TestStandardWebhookGolden_Vectors_EQ1(t *testing.T) {
	for _, v := range loadWebhookGolden(t).Vectors {
		secs, _ := strconv.ParseInt(v.Timestamp, 10, 64)
		now := time.Unix(secs, 0)
		if r := VerifyStandardWebhook([]byte(v.Body), v.ID, v.Timestamp, v.Signature, []string{v.Secret}, now, DefaultStandardTolerance); r != "" {
			t.Errorf("%s: %s", v.Note, r)
		}
		// control: another secret does not verify it
		if r := VerifyStandardWebhook([]byte(v.Body), v.ID, v.Timestamp, v.Signature, []string{"not-the-secret"}, now, DefaultStandardTolerance); r != ReasonSignatureMismatch {
			t.Errorf("%s: control got %q", v.Note, r)
		}
	}
}

func TestStandardWebhookGolden_Boundaries_EQ1(t *testing.T) {
	g := loadWebhookGolden(t)
	tol := time.Duration(g.Tolerance) * time.Second
	for _, c := range g.Verify {
		got := VerifyStandardWebhook([]byte(c.Body), c.ID, c.Timestamp, c.Header, c.Secrets, goldenNow(t, c.Now), tol)
		if got == "" {
			got = "ok"
		}
		if got != c.Expect {
			t.Errorf("%s: %s; golden %s", c.Note, got, c.Expect)
		}
	}
}

// The tolerance edge at nanosecond resolution (beyond the golden's decimal seconds).
func TestStandardWebhook_ToleranceEdgeNanoseconds_EQ1(t *testing.T) {
	body := []byte(`{"event_id":"evt_ns"}`)
	ts := int64(1795140000)
	tsText := strconv.FormatInt(ts, 10)
	sig := signStandard("whsec_YWJjZGVm", "evt_ns", tsText, body)
	at := time.Unix(ts, 0)
	for d, want := range map[time.Duration]string{
		300 * time.Second:                  "",
		-300 * time.Second:                 "",
		300*time.Second + time.Nanosecond:  ReasonOutOfTolerance,
		-300*time.Second - time.Nanosecond: ReasonOutOfTolerance,
	} {
		if got := VerifyStandardWebhook(body, "evt_ns", tsText, sig, []string{"whsec_YWJjZGVm"}, at.Add(d), DefaultStandardTolerance); got != want {
			t.Errorf("now = ts %+v: %q; want %q", d, got, want)
		}
	}
}

func TestLegacyXEventSignatureGolden_EQ1(t *testing.T) {
	L := loadWebhookGolden(t).Legacy
	for _, c := range L.Cases {
		if got := VerifyHMAC([]byte(L.Body), c.Signature, L.Secret); got != c.Valid {
			t.Errorf("%s (%q): %v; golden %v", c.Note, c.Signature, got, c.Valid)
		}
		if VerifyHMAC([]byte(L.Body), c.Signature, "") {
			t.Errorf("%s: an empty secret verified", c.Note)
		}
	}
}

// ----------------------------------------------------------------------------- the receiver

const (
	eqNew = "whsec_bmV3LXNlY3JldC0wMDAwMDAwMDAwMDAwMDAw"
	eqOld = "whsec_b2xkLXNlY3JldC0wMDAwMDAwMDAwMDAwMDAw"
	eqT   = int64(1795140000)
)

var eqBody = []byte(`{"event_type":"eq1.edge.unhandled","event_id":"evt_eq1"}`)

func signStandard(secret, id, ts string, body []byte) string {
	mac := hmac.New(sha256.New, StandardWebhookKey(secret))
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func stdHeaders(secret string, ts int64) map[string]string {
	tsText := strconv.FormatInt(ts, 10)
	return map[string]string{StandardHeaderID: "evt_eq1", StandardHeaderTimestamp: tsText,
		StandardHeaderSignature: signStandard(secret, "evt_eq1", tsText, eqBody)}
}

func legacyHeader(secret string) map[string]string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(eqBody)
	return map[string]string{"X-Event-Signature": "sha256=" + hex.EncodeToString(mac.Sum(nil))}
}

func merge(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func TestReceiver_RotationToleranceAndReplay_EQ1(t *testing.T) {
	at := func(off time.Duration) func() time.Time {
		return func() time.Time { return time.Unix(eqT, 0).Add(off) }
	}
	cases := []struct {
		name     string
		headers  map[string]string
		previous []string
		now      func() time.Time
		status   int
	}{
		{"standard, current secret", stdHeaders(eqNew, eqT), nil, at(0), 200},
		{"standard, signed by the OLD secret, previous configured", stdHeaders(eqOld, eqT), []string{eqOld}, at(0), 200},
		{"standard, signed by the OLD secret, no previous", stdHeaders(eqOld, eqT), nil, at(0), 401},
		{"legacy by the OLD secret, previous configured", legacyHeader(eqOld), []string{eqOld}, at(0), 200},
		{"standard exactly at +300 s", stdHeaders(eqNew, eqT), nil, at(300 * time.Second), 200},
		{"standard exactly at -300 s", stdHeaders(eqNew, eqT), nil, at(-300 * time.Second), 200},
		{"standard at +300.5 s", stdHeaders(eqNew, eqT), nil, at(300*time.Second + 500*time.Millisecond), 401},  // RED
		{"standard at -300.5 s", stdHeaders(eqNew, eqT), nil, at(-300*time.Second - 500*time.Millisecond), 401}, // RED
		{"a REPLAY: stale standard headers + a valid X-Event-Signature",
			merge(stdHeaders(eqNew, eqT), legacyHeader(eqNew)), nil, at(24 * time.Hour), 401}, // RED
		{"only webhook-signature + a valid X-Event-Signature",
			merge(map[string]string{StandardHeaderSignature: stdHeaders(eqNew, eqT)[StandardHeaderSignature]}, legacyHeader(eqNew)),
			nil, at(0), 401}, // RED
		{"standard with a malformed timestamp",
			merge(stdHeaders(eqNew, eqT), map[string]string{StandardHeaderTimestamp: "+" + strconv.FormatInt(eqT, 10)}),
			nil, at(0), 401}, // RED
		{"control: no standard headers, valid X-Event-Signature", legacyHeader(eqNew), nil, at(24 * time.Hour), 200},
		{"control: nothing at all", map[string]string{}, nil, at(0), 401},
	}
	for _, c := range cases {
		h := MakeRouter(ReceiverConfig{Secret: eqNew, PreviousSecrets: c.previous, Registry: NewRegistry(), Now: c.now})
		req := httptest.NewRequest(http.MethodPost, "/api/quotas"+WebhookPath, bytes.NewReader(eqBody))
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.status {
			t.Errorf("%s: %d; want %d (%s)", c.name, rec.Code, c.status, rec.Body.String())
		}
	}
}
