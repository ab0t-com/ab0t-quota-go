package meter_test

// v0.3.0 — billing checks each usage event's rate against the price list the service published, keyed
// (metadata.service, metadata.product, metadata.variant). The SNS lifecycle path carries no service
// header, so the meter puts the service into the metadata of the reserve AND of every lifecycle event
// (same key as the Python meter). RED before v0.3.0: no "service" key.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/meter"
	"github.com/ab0t-com/ab0t-quota-go/meter/metertest"
)

func TestReserve_PriceKeyCarriesTheService_20261127(t *testing.T) {
	book, err := meter.ParsePricing(json.RawMessage(pricingJSON))
	if err != nil {
		t.Fatal(err)
	}
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, func(c *meter.Config) { c.Pricing = book; c.ToolID = "acme-render" })
	run, err := m.Reserve(context.Background(), meter.ReserveInput{OrgID: "org-1", UserID: "u", ResourceID: "b", Product: "vm", Variant: "large"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Metadata["service"] != "acme-render" || run.Metadata["product"] != "vm" || run.Metadata["variant"] != "large" {
		t.Fatalf("run metadata %+v", run.Metadata)
	}
	if calls := f.ReserveCalls(); len(calls) != 1 || calls[0].Metadata["service"] != "acme-render" {
		t.Fatalf("the reserve must carry metadata.service: %+v", calls)
	}
	if err := run.Started(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	if ev := f.Events(meter.EventStarted, run.ReservationID)[0]; ev.Metadata["service"] != "acme-render" {
		t.Fatalf("the lifecycle event must carry metadata.service: %+v", ev.Metadata)
	}
}

// CONTROL: no tool id ("default") → no service key (nothing published to match); an explicit rate → no
// price key at all.
func TestReserve_NoServiceKeyWithoutAToolID_20261127(t *testing.T) {
	book, _ := meter.ParsePricing(json.RawMessage(pricingJSON))
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, func(c *meter.Config) { c.Pricing = book })
	run, err := m.Reserve(context.Background(), meter.ReserveInput{OrgID: "org-1", UserID: "u", ResourceID: "b", Product: "vm"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := run.Metadata["service"]; ok {
		t.Fatalf("default tool id must not claim a service: %+v", run.Metadata)
	}
}
