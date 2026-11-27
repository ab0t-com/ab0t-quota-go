package mesh

// v0.2.0: one family of env names — the ones `authsetup connect billing|payment` prints
// (BILLING_SERVICE_URL / BILLING_SERVICE_API_KEY / PAYMENT_SERVICE_URL / PAYMENT_SERVICE_API_KEY).
// The old AB0T_QUOTA_* names are a fallback; the canonical name wins; ONE deprecation log per process.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

var allNames = []string{
	EnvBillingServiceURL, EnvBillingServiceAPIKey, EnvPaymentServiceURL, EnvPaymentServiceAPIKey,
	EnvBillingURL, EnvPaymentURL, EnvServiceTok, EnvBillingTok, EnvPaymentTok,
}

// clearEnv blanks every name (an empty value reads as unset) and resets the once-per-process flag.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, n := range allNames {
		t.Setenv(n, "")
	}
	deprecationMu.Lock()
	deprecationLogged = false
	deprecationMu.Unlock()
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestResolve_CanonicalNames_20261123(t *testing.T) {
	clearEnv(t)
	logs := captureLogs(t)
	t.Setenv("BILLING_SERVICE_URL", "https://billing.example/")
	t.Setenv("BILLING_SERVICE_API_KEY", "ab0t_sk_billing")
	t.Setenv("PAYMENT_SERVICE_URL", "https://payment.example")
	t.Setenv("PAYMENT_SERVICE_API_KEY", "ab0t_sk_payment")
	u := Resolve()
	if u.Billing != "https://billing.example" || u.BillingToken != "ab0t_sk_billing" ||
		u.Payment != "https://payment.example" || u.PaymentToken != "ab0t_sk_payment" {
		t.Fatalf("%+v", u)
	}
	if u.BillingSource != "BILLING_SERVICE_URL" || u.PaymentSource != "PAYMENT_SERVICE_URL" {
		t.Fatalf("sources %+v", u)
	}
	if strings.Contains(logs.String(), "deprecated") {
		t.Fatalf("canonical names only must not log a deprecation: %s", logs.String())
	}
}

func TestResolve_DeprecatedFallback_20261123(t *testing.T) {
	clearEnv(t)
	logs := captureLogs(t)
	t.Setenv("AB0T_QUOTA_BILLING_URL", "https://old-billing.example")
	t.Setenv("AB0T_QUOTA_BILLING_TOKEN", "ab0t_sk_old_billing")
	t.Setenv("AB0T_QUOTA_PAYMENT_URL", "https://old-payment.example")
	t.Setenv("AB0T_QUOTA_SERVICE_TOKEN", "ab0t_sk_shared")
	u := Resolve()
	if u.Billing != "https://old-billing.example" || u.BillingToken != "ab0t_sk_old_billing" ||
		u.Payment != "https://old-payment.example" || u.PaymentToken != "ab0t_sk_shared" || u.Token != "ab0t_sk_shared" {
		t.Fatalf("the old names must keep working: %+v", u)
	}
	if u.BillingSource != "AB0T_QUOTA_BILLING_URL" || u.PaymentSource != "AB0T_QUOTA_PAYMENT_URL" {
		t.Fatalf("sources %+v", u)
	}
	out := logs.String()
	if strings.Count(out, "deprecated env var names") != 1 {
		t.Fatalf("want ONE deprecation line, got:\n%s", out)
	}
	for _, n := range []string{"AB0T_QUOTA_BILLING_URL", "AB0T_QUOTA_BILLING_TOKEN", "AB0T_QUOTA_PAYMENT_URL"} {
		if !strings.Contains(out, n) {
			t.Errorf("the deprecation line must name %s: %s", n, out)
		}
	}
	// the shared key is a supported last fallback (not deprecated — the Python SDK's shape)
	if strings.Contains(out, "AB0T_QUOTA_SERVICE_TOKEN") {
		t.Errorf("the shared fallback key must not be reported as deprecated: %s", out)
	}
	// a second (and third) Resolve in the same process does not log again
	Resolve()
	Resolve()
	if strings.Count(logs.String(), "deprecated env var names") != 1 {
		t.Fatalf("deprecation logged more than once per process:\n%s", logs.String())
	}
}

func TestResolve_CanonicalWins_20261123(t *testing.T) {
	clearEnv(t)
	logs := captureLogs(t)
	t.Setenv("BILLING_SERVICE_URL", "https://new.example")
	t.Setenv("AB0T_QUOTA_BILLING_URL", "https://old.example")
	t.Setenv("BILLING_SERVICE_API_KEY", "ab0t_sk_new")
	t.Setenv("AB0T_QUOTA_BILLING_TOKEN", "ab0t_sk_old")
	t.Setenv("AB0T_QUOTA_SERVICE_TOKEN", "ab0t_sk_shared")
	u := Resolve()
	if u.Billing != "https://new.example" || u.BillingToken != "ab0t_sk_new" || u.BillingSource != "BILLING_SERVICE_URL" {
		t.Fatalf("canonical must win: %+v", u)
	}
	// the payment key had no canonical value: the shared fallback still applies there
	if u.PaymentToken != "ab0t_sk_shared" {
		t.Fatalf("payment fallback: %+v", u)
	}
	if !strings.Contains(logs.String(), "AB0T_QUOTA_BILLING_URL (ignored") || !strings.Contains(logs.String(), "AB0T_QUOTA_BILLING_TOKEN (ignored") {
		t.Fatalf("an old name set to a different value than the winning canonical one must be named: %s", logs.String())
	}
}

// Both names set to the SAME value (the recommended rollback-safe setup): no warning.
func TestResolve_SameValueBothNamesIsQuiet_20261123(t *testing.T) {
	clearEnv(t)
	logs := captureLogs(t)
	t.Setenv("BILLING_SERVICE_URL", "https://billing.example")
	t.Setenv("AB0T_QUOTA_BILLING_URL", "https://billing.example")
	u := Resolve()
	if u.Billing != "https://billing.example" || u.BillingSource != "BILLING_SERVICE_URL" {
		t.Fatalf("%+v", u)
	}
	if strings.Contains(logs.String(), "deprecated") {
		t.Fatalf("an identical old name is harmless and must not warn: %s", logs.String())
	}
}

// Control: with nothing set, everything is empty and nothing is logged.
func TestResolve_NothingSet_20261123(t *testing.T) {
	clearEnv(t)
	logs := captureLogs(t)
	u := Resolve()
	if u != (URLs{}) || logs.Len() != 0 {
		t.Fatalf("%+v / %q", u, logs.String())
	}
}
