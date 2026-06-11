package quota

import (
	"context"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

// TestSetup_DegradedMode_NoEnv_NoMeshClients confirms that Setup runs
// cleanly with no env vars and the Capabilities snapshot accurately
// reports which subsystems are off + why.
func TestSetup_DegradedMode_NoEnv_NoMeshClients(t *testing.T) {
	// Wipe relevant env so test is hermetic.
	for _, k := range []string{
		"AB0T_QUOTA_BILLING_URL",
		"AB0T_QUOTA_PAYMENT_URL",
		"AB0T_QUOTA_SERVICE_TOKEN",
		"AB0T_AUTH_WEBHOOK_SECRET",
	} {
		t.Setenv(k, "")
	}

	q, err := Setup(context.Background(), Options{ConfigOverride: minimalConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())

	cap := q.Capabilities()

	// Required-on subsystems
	if !cap.Engine {
		t.Error("Engine should always be on")
	}
	if !cap.AuthEvents {
		t.Error("AuthEvents (the receiver mount point) should always be on")
	}
	if cap.LedgerBackend != "memory" {
		t.Errorf("ledger backend = %q (want memory)", cap.LedgerBackend)
	}
	if cap.FloatStore != "memory" {
		t.Errorf("float store = %q (want memory)", cap.FloatStore)
	}

	// Required-off + WhyOff coverage
	mustOff := map[string]bool{
		"billing":            cap.Billing,
		"payment":            cap.Payment,
		"credit_grant":       cap.CreditGrant,
		"auto_subscribe":     cap.AutoSubscribe,
		"alerts_webhook":     cap.AlertsWebhook,
	}
	for name, on := range mustOff {
		if on {
			t.Errorf("%s should be off in degraded mode", name)
		}
	}

	mustReportWhy := []string{"billing", "payment", "credit_grant", "auth_events_signed"}
	for _, k := range mustReportWhy {
		if reason, ok := cap.WhyOff[k]; !ok || reason == "" {
			t.Errorf("WhyOff[%q] missing or empty (got %q)", k, reason)
		}
	}
}

// TestSetup_AlertsWebhook_HonorsConfig confirms that an alerts.webhook_url
// in config is actually picked up and surfaces as a Capability.
func TestSetup_AlertsWebhook_HonorsConfig(t *testing.T) {
	cfg := minimalConfig()
	cfg.Alerts = config.AlertsConfig{
		Enabled:          true,
		CooldownSeconds:  60,
		WarningThreshold: 0.8,
		WebhookURL:       "https://hooks.example.com/quota",
	}
	q, err := Setup(context.Background(), Options{ConfigOverride: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())

	cap := q.Capabilities()
	if !cap.Alerts {
		t.Error("Alerts should be on")
	}
	if !cap.AlertsWebhook {
		t.Error("AlertsWebhook should be on with WebhookURL")
	}
}

// TestSetup_AlertsWebhook_RejectsBadScheme — config-supplied file:// URL
// is rejected by SSRF guard at Setup, WhyOff explains.
func TestSetup_AlertsWebhook_RejectsBadScheme(t *testing.T) {
	cfg := minimalConfig()
	cfg.Alerts = config.AlertsConfig{
		Enabled:    true,
		WebhookURL: "file:///etc/passwd",
	}
	q, err := Setup(context.Background(), Options{ConfigOverride: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	cap := q.Capabilities()
	if cap.AlertsWebhook {
		t.Error("file:// URL must not enable webhook")
	}
	if _, ok := cap.WhyOff["alerts_webhook"]; !ok {
		t.Error("WhyOff should explain alerts_webhook rejection")
	}
}
