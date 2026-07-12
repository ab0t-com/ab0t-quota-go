package quota

// D-26 — Setup must WIRE the engine's over-admission event seam to the alert
// manager when alerts are enabled. Without this the over_limit_admitted
// event has no sink and D-24 B's observability premise is unmet. Guards
// against the wiring silently regressing to "emit with no consumer".

import (
	"context"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

func TestSetup_WiresOverLimitSink_D26(t *testing.T) {
	cfg := minimalConfig()
	cfg.Alerts = config.AlertsConfig{Enabled: true}
	q, err := Setup(context.Background(), Options{ConfigOverride: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	if q.Engine.OnEvent == nil {
		t.Error("D-26: Setup did not wire Engine.OnEvent — over_limit_admitted has no sink")
	}
	if q.Alerts == nil {
		t.Error("alerts manager should be constructed when alerts enabled")
	}
}

func TestSetup_NoAlerts_NoSinkButNoPanic_D26(t *testing.T) {
	cfg := minimalConfig() // alerts disabled
	q, err := Setup(context.Background(), Options{ConfigOverride: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	// With alerts off there is no sink (events still log); OnEvent may be nil.
	if q.Engine.OnEvent != nil {
		t.Log("OnEvent wired even without alerts — acceptable if it no-ops")
	}
}
