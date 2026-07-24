package quota

// Claim 1 / D-44 / D-34 — the fail-closed billing gate. A paid service that
// cannot durably record billing must NOT start (serving billable work for
// free is the leak, not availability). This tests the gate AND its negative
// control (a durable outbox → Setup succeeds, billing ON).

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/outbox"
)

type fakeSettlementPublisher struct{}

func (fakeSettlementPublisher) Publish(context.Context, outbox.Record) error { return nil }

// durableRedisCfg returns a paid config with a durable Redis outbox wired.
func durableRedisCfg(t *testing.T) *config.Config {
	t.Helper()
	mr := miniredis.RunT(t)
	c := paidCfg()
	c.Storage = config.StorageConfig{RedisURL: config.Declare("redis://" + mr.Addr())}
	c.Outbox = config.OutboxConfig{Store: "redis", RedisDurabilityConfirmed: true}
	return c
}

func paidCfg() *config.Config {
	c := minimalConfig()
	c.Billing = config.BillingConfig{EnablePaid: true}
	return c
}

// enable_paid + NO durable outbox + !allow_ephemeral → REFUSE to start.
func TestSetup_PaidNoDurableOutbox_Refuses_D44(t *testing.T) {
	_, err := Setup(context.Background(), Options{ConfigOverride: paidCfg()})
	if err == nil {
		t.Fatal("D-44: enable_paid with no durable outbox must REFUSE to start")
	}
	// The chain is severed at its first link (the durable store); the error
	// names it and cites the whole-chain gate (D-56).
	if !strings.Contains(err.Error(), "durable outbox") || !strings.Contains(err.Error(), "D-56") {
		t.Errorf("error should name the durable-outbox link + D-56, got: %v", err)
	}
}

// allow_ephemeral=true is the explicit, on-the-record dev escape: start, but
// billing DISABLED (a known, visible state — not QB-01).
func TestSetup_PaidAllowEphemeral_StartsBillingDisabled_D44(t *testing.T) {
	c := paidCfg()
	c.Outbox = config.OutboxConfig{AllowEphemeral: true}
	q, err := Setup(context.Background(), Options{ConfigOverride: c})
	if err != nil {
		t.Fatalf("allow_ephemeral should start (dev), got: %v", err)
	}
	defer q.Close(context.Background())
	if !strings.Contains(q.Capabilities().BillingStatus, "OFF") {
		t.Errorf("billing should be OFF under allow_ephemeral, got %q", q.Capabilities().BillingStatus)
	}
}

// D-56: a DURABLE OUTBOX ALONE is NOT the chain. With a durable store but no
// settlement publisher, the gate must REFUSE naming the weakest link — "has a
// durable outbox" was never the guarantee; "usage reaches billing" is.
func TestSetup_PaidDurableOutboxNoPublisher_Refuses_D56(t *testing.T) {
	_, err := Setup(context.Background(), Options{ConfigOverride: durableRedisCfg(t)})
	if err == nil {
		t.Fatal("D-56: durable outbox but no publisher → the chain is severed; Setup must refuse")
	}
	if !strings.Contains(err.Error(), "publisher") || !strings.Contains(err.Error(), "D-56") {
		t.Errorf("error should name the missing publisher link + D-56, got: %v", err)
	}
}

// D-56: durable store + publisher but NO billing sink → refuses naming the sink.
func TestSetup_PaidNoBillingSink_Refuses_D56(t *testing.T) {
	t.Setenv("AB0T_QUOTA_BILLING_URL", "") // no sink
	_, err := Setup(context.Background(), Options{
		ConfigOverride: durableRedisCfg(t), SettlementPublisher: fakeSettlementPublisher{},
	})
	if err == nil {
		t.Fatal("D-56: no billing sink → chain severed; Setup must refuse")
	}
	if !strings.Contains(err.Error(), "billing sink") {
		t.Errorf("error should name the missing billing_sink link, got: %v", err)
	}
}

// D-56 NEGATIVE CONTROL — the WHOLE chain present: durable store + publisher +
// billing sink → Setup succeeds, billing ON, drain loop started, health OK.
func TestSetup_PaidWholeChain_StartsBillingOn_D56(t *testing.T) {
	t.Setenv("AB0T_QUOTA_BILLING_URL", "http://billing.test")
	q, err := Setup(context.Background(), Options{
		ConfigOverride: durableRedisCfg(t), SettlementPublisher: fakeSettlementPublisher{},
	})
	if err != nil {
		t.Fatalf("whole chain present — Setup must NOT refuse: %v", err)
	}
	defer q.Close(context.Background())
	if q.Outbox == nil {
		t.Error("outbox emitter should be wired on the handle")
	}
	if !strings.HasPrefix(q.Capabilities().BillingStatus, "ON") {
		t.Errorf("billing should be ON with the whole chain, got %q", q.Capabilities().BillingStatus)
	}
	if ok, why := q.BillingHealthy(); !ok {
		t.Errorf("BillingHealthy() should be true with the whole chain, got false (%s)", why)
	}
}

// The health probe FAILS while any link is missing (D-56/D-51).
func TestBillingHealthy_FailsWhenChainSevered_D56(t *testing.T) {
	q, err := Setup(context.Background(), Options{ConfigOverride: func() *config.Config {
		c := paidCfg()
		c.Outbox = config.OutboxConfig{AllowEphemeral: true} // start, billing off
		return c
	}()})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	if ok, _ := q.BillingHealthy(); ok {
		t.Error("D-51: a severed chain must read UNHEALTHY, not healthy")
	}
}

// Not a paid service → no gate, no refusal (existing behavior preserved).
func TestSetup_NotPaid_NoGate_D44(t *testing.T) {
	q, err := Setup(context.Background(), Options{ConfigOverride: minimalConfig()})
	if err != nil {
		t.Fatalf("non-paid service must not be gated: %v", err)
	}
	defer q.Close(context.Background())
	if q.Capabilities().BillingStatus != "OFF (paid disabled)" {
		t.Errorf("expected OFF (paid disabled), got %q", q.Capabilities().BillingStatus)
	}
}

// The Options.EnablePaid override forces the gate even without config/billing URL.
func TestSetup_OptionsEnablePaidOverride_D44(t *testing.T) {
	yes := true
	_, err := Setup(context.Background(), Options{ConfigOverride: minimalConfig(), EnablePaid: &yes})
	if err == nil {
		t.Fatal("Options.EnablePaid=true with no durable outbox must refuse")
	}
}
