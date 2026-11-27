//go:build packaging_gate

// Command packaginggate is the consumer smoke program of scripts/packaging-gate.sh.
//
// The gate copies this file into a fresh, throw-away consumer module that requires the library
// through a `replace` to the checkout, resolves that module's dependencies from scratch
// (`go mod tidy`), builds it and runs it. It proves the module as a consumer sees it: every package a
// client imports builds from the published go.mod / go.sum, and the core objects construct and work
// without any network (a meter end to end against the in-process fake, the typed billing client,
// env resolution, exact money).
//
// The build tag keeps it out of `go build ./...` / `go vet ./...` in this repository.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/ab0t-com/ab0t-quota-go/meter"
	"github.com/ab0t-com/ab0t-quota-go/meter/metertest"
	"github.com/shopspring/decimal"
)

func main() {
	failures := 0
	check := func(name string, err error) {
		if err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", name, err)
			return
		}
		fmt.Printf("ok   %s\n", name)
	}

	check("billing.New (typed client, no network at construction)", func() error {
		_, err := billing.New(mesh.URLs{Billing: "http://127.0.0.1:9", BillingToken: "ab0t_sk_packaging_gate"})
		return err
	}())

	check("mesh.Resolve", func() error {
		_ = mesh.Resolve()
		return nil
	}())

	check("exact money: CostForDuration rounds UP to 6 places", func() error {
		got, err := billing.CostForDuration(billing.MustParseMoney("0.000061"), 60*time.Second)
		if err != nil {
			return err
		}
		if got.String() != "0.000002" {
			return fmt.Errorf("0.000061/h x 60s = %s, want 0.000002", got)
		}
		return nil
	}())

	check("meter.New + Reserve + Stop against the in-process fake", func() error {
		f := metertest.New(map[string]string{"org-1": "5"})
		m, err := meter.New(meter.Config{Billing: f, Sink: f, HeartbeatInterval: time.Hour})
		if err != nil {
			return err
		}
		defer m.Shutdown()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		run, err := m.Reserve(ctx, meter.ReserveInput{
			OrgID: "org-1", UserID: "u-1", ResourceID: "box-1", ResourceType: "vm",
			HourlyRate: decimal.RequireFromString("0.25"), AllocationFee: decimal.RequireFromString("0.01"),
		})
		if err != nil {
			return err
		}
		if len(f.ReserveCalls()) != 1 {
			return fmt.Errorf("reserve calls = %d, want 1", len(f.ReserveCalls()))
		}
		if _, err := run.Abort(ctx, "packaging_gate"); err != nil {
			return err
		}
		return nil
	}())

	if failures > 0 {
		fmt.Fprintf(os.Stderr, "packaging gate: %d check(s) failed\n", failures)
		os.Exit(1)
	}
	fmt.Println("packaging gate: consumer smoke passed")
}
