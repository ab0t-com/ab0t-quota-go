package main

// v0.3.2 — `quotactl doctor --money` runs the money-safety checklist (quota.MoneyChecklist): no Setup,
// contacts nothing without --live; unknown is never "good". The subscribe-events pins are in
// never_astray_behaviour_20261128_test.go.
// RED before v0.3.2: runDoctorMoney / moneyFlags do not exist (build failure); --money is unknown.
// Run: go test ./cmd/quotactl -run NeverAstray -count=1

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func runMoney(t *testing.T, path string, failOnRisk bool, m moneyFlags) (int, map[string]any) {
	t.Helper()
	cmd := &cobra.Command{}
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	m.on = true
	code := runDoctorMoney(context.Background(), cmd, path, true, failOnRisk, m)
	var doc map[string]any
	if out.Len() > 0 {
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("json: %v\n%s", err, out.String())
		}
	}
	return code, doc
}

func TestNeverAstray_DoctorMoney_20261128(t *testing.T) {
	for _, k := range []string{"BILLING_SERVICE_URL", "AB0T_QUOTA_BILLING_URL", "AB0T_AUTH_WEBHOOK_SECRET",
		"AB0T_ENVIRONMENT", "ENVIRONMENT"} {
		t.Setenv(k, "")
	}
	p := writeQuotaConfig(t, giftConfig+`}`)
	code, doc := runMoney(t, p, false, moneyFlags{})
	findings, _ := doc["findings"].([]any)
	if code != exitOK || len(findings) != 8 {
		t.Fatalf("exit %d findings %d", code, len(findings))
	}
	grades := map[string]string{}
	for _, f := range findings {
		m := f.(map[string]any)
		grades[m["id"].(string)] = m["grade"].(string)
	}
	// No billing URL while a gift is declared: paid mode OFF is a risk; the missing secret is a risk.
	if grades["M1"] != "risk" || grades["M3"] != "risk" || grades["M7"] != "not_checked" {
		t.Fatalf("grades %v", grades)
	}
	if code, _ := runMoney(t, p, true, moneyFlags{}); code != exitGate {
		t.Fatalf("--fail-on-risk exit %d, want %d", code, exitGate)
	}
	if code, _ := runMoney(t, filepath.Join(t.TempDir(), "missing.json"), false, moneyFlags{}); code != exitConfig {
		t.Fatalf("missing config exit %d, want %d", code, exitConfig)
	}
}

// --live --org with a static provider: the tier the runtime would serve is graded against --expect-tier.
func TestNeverAstray_DoctorMoneyTierLookup_20261128(t *testing.T) {
	for _, k := range []string{"AB0T_AUTH_AUTH_URL", "AB0T_AUTH_ADMIN_TOKEN", "AB0T_AUTH_WEBHOOK_PUBLIC_URL"} {
		t.Setenv(k, "")
	}
	p := writeQuotaConfig(t, giftConfig+`}`)
	_, doc := runMoney(t, p, false, moneyFlags{live: true, org: "org_paying", expectTier: "pro"})
	for _, f := range doc["findings"].([]any) {
		m := f.(map[string]any)
		if m["id"] == "M4" && m["grade"] != "risk" {
			t.Fatalf("M4 %v — the org resolves to free, expected pro", m)
		}
	}
}
