package config

import (
	"os"
	"testing"
)

func TestLoadMinimal(t *testing.T) {
	c, err := LoadConfig("testdata/minimal.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.ServiceName != "test-service" {
		t.Errorf("ServiceName: got %q", c.ServiceName)
	}
	if len(c.Tiers) != 2 {
		t.Errorf("Tiers: got %d, want 2", len(c.Tiers))
	}
	if tier := c.TierByID("pro"); tier == nil || tier.DisplayName != "Pro" {
		t.Errorf("TierByID(pro): %+v", tier)
	}
}

func TestLoadFullWithEnvInterpolation(t *testing.T) {
	t.Setenv("QUOTA_SERVICE_NAME", "from-env")
	t.Setenv("QUOTA_REDIS_URL", "redis://test:6379/1")
	t.Setenv("QUOTA_STATE_TABLE", "test_table")
	c, err := LoadConfig("testdata/full.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.ServiceName != "from-env" {
		t.Errorf("env interpolation failed: %q", c.ServiceName)
	}
	if c.Storage.RedisURL != "redis://test:6379/1" {
		t.Errorf("redis interpolation: %q", c.Storage.RedisURL)
	}
	if c.Storage.DynamoDBTable != "test_table" {
		t.Errorf("ddb table: %q", c.Storage.DynamoDBTable)
	}
}

func TestEnvInterpolationDefault(t *testing.T) {
	os.Unsetenv("QUOTA_SERVICE_NAME")
	c, err := LoadConfig("testdata/full.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.ServiceName != "default-svc" {
		t.Errorf("default not applied: %q", c.ServiceName)
	}
}

func TestForwardCompatUnknownKey(t *testing.T) {
	c, err := LoadConfig("testdata/full.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := c.Extra["unknown_forward_compat_key"]; !ok {
		t.Errorf("unknown top-level key not captured in Extra; have %v", c.Extra)
	}
}

func TestTierLimitShapes(t *testing.T) {
	c, err := LoadConfig("testdata/full.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	free := c.TierByID("free")
	if free == nil {
		t.Fatal("no free tier")
	}
	// numeric form
	conc := free.Limits["x.concurrent"]
	if conc.Limit == nil || *conc.Limit != 1.0 {
		t.Errorf("x.concurrent limit: %v", conc.Limit)
	}
	// null form → unlimited
	rate := free.Limits["x.rate"]
	if !rate.IsUnlimited() {
		t.Errorf("x.rate should be unlimited")
	}
	// object form with thresholds
	spend := free.Limits["x.spend"]
	if spend.Limit == nil || *spend.Limit != 10.00 {
		t.Errorf("x.spend limit: %v", spend.Limit)
	}
	if spend.WarningThreshold != 0.8 {
		t.Errorf("warning threshold: %v", spend.WarningThreshold)
	}
}

func TestInitialCreditSynthesizesCreditGrant(t *testing.T) {
	c, err := LoadConfig("testdata/full.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	free := c.TierByID("free")
	if free.CreditGrant == nil {
		t.Fatal("initial_credit should have synthesized a CreditGrant")
	}
	if free.CreditGrant.Trigger != CreditTriggerSignup {
		t.Errorf("trigger: %v", free.CreditGrant.Trigger)
	}
	if free.CreditGrant.Destination != CreditDestCreditBalance {
		t.Errorf("destination: %v", free.CreditGrant.Destination)
	}
	if free.CreditGrant.Dedup != DedupPerUserPerTier {
		t.Errorf("dedup default: %v", free.CreditGrant.Dedup)
	}
}

func TestBillingModelValidation(t *testing.T) {
	c, err := LoadConfig("testdata/full.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	pro := c.TierByID("pro")
	if pro.BillingModel != BillingModelSubscriptionWithCredits {
		t.Errorf("billing model: %v", pro.BillingModel)
	}
	if pro.Price == nil || pro.Price.AmountPerPeriod.Decimal.String() != "29" {
		t.Errorf("price: %v", pro.Price)
	}
}

func TestDecimalRoundTrip(t *testing.T) {
	d := NewDecimal("10.00")
	b, err := d.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var d2 Decimal
	if err := d2.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if !d.Decimal.Equal(d2.Decimal) {
		t.Errorf("round-trip: %v != %v", d, d2)
	}
}

func TestDecimalAcceptsNumber(t *testing.T) {
	var d Decimal
	if err := d.UnmarshalJSON([]byte("10.5")); err != nil {
		t.Fatal(err)
	}
	if !d.Decimal.Equal(NewDecimal("10.5").Decimal) {
		t.Errorf("number form: %v", d)
	}
}

func TestDedupPolicyDefaultsToPerUserPerTier(t *testing.T) {
	g := &CreditGrant{Trigger: CreditTriggerSignup, AmountPerPeriod: NewDecimal("10")}
	g.applyDefaults()
	if g.Dedup != DedupPerUserPerTier {
		t.Errorf("default dedup: %v", g.Dedup)
	}
}

func TestDerivePerUserLimit(t *testing.T) {
	limit := 10.0
	tl := TierLimit{Limit: &limit}
	pul := tl.DerivePerUserLimit(0.5)
	if pul == nil || *pul != 5.0 {
		t.Errorf("derive(0.5 of 10): %v", pul)
	}
	// floor at 1.0
	smaller := 1.0
	tl2 := TierLimit{Limit: &smaller}
	pul2 := tl2.DerivePerUserLimit(0.1)
	if pul2 == nil || *pul2 != 1.0 {
		t.Errorf("floor at 1.0: %v", pul2)
	}
	// unlimited stays unlimited
	tl3 := TierLimit{Limit: nil}
	if tl3.DerivePerUserLimit(0.5) != nil {
		t.Errorf("unlimited should derive nil")
	}
	// explicit per_user_limit wins
	explicit := 7.0
	tl4 := TierLimit{Limit: &limit, PerUserLimit: &explicit}
	pul4 := tl4.DerivePerUserLimit(0.5)
	if pul4 == nil || *pul4 != 7.0 {
		t.Errorf("explicit per_user_limit ignored: %v", pul4)
	}
}
