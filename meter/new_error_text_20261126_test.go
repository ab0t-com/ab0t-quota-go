package meter_test

// meter.New's "Billing is required" error names a constructor a client can actually call:
// billing.New(mesh.Resolve()). An earlier text suggested mesh.FromEnv, which does not exist.

import (
	"errors"
	"strings"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/ab0t-com/ab0t-quota-go/meter"
	"github.com/ab0t-com/ab0t-quota-go/meter/metertest"
)

func TestNew_MissingBillingErrorNamesARealConstructor_20261126(t *testing.T) {
	_, err := meter.New(meter.Config{Sink: metertest.New(nil)})
	if !errors.Is(err, meter.ErrConfig) {
		t.Fatalf("want ErrConfig, got %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "billing.New(mesh.Resolve())") || strings.Contains(msg, "FromEnv") {
		t.Fatalf("error text = %q; want it to suggest billing.New(mesh.Resolve()) (mesh.FromEnv does not exist)", msg)
	}
	_ = mesh.Resolve // compile-time proof the suggested function exists
}
