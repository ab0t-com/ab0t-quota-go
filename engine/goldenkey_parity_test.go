package engine

// TASK P0.5 — cross-runtime golden-key parity test (finding QG-03, evidence E-D4).
// Ticket: sandbox-platform/tickets/20260709_ab0t_quota_systemic_integrity_redesign.
//
// Contract under test: SAME config → SAME Redis keys across runtimes. The
// deployed Python lib (shared/ab0t-quota, v0.5.x) is the reference keyspace;
// its actual keys are recorded in testdata/golden_keys_python_v052.json
// (regenerate with testdata/dump_golden_keys_python.py — derivation is
// executed Python, not transcribed comments).
//
// Why this matters: the Go source comments CLAIM parity ("keys, prefixes,
// and TTLs match Python lib v0.5.2" — counters/counter.go:13; gauge.go:6;
// accumulator.go:58) but the shapes differ:
//
//	Python  quota:{org}:{rk}:gauge            (base.py:20-21, gauge.py:19-20)
//	Go      quota:gauge:{rk}:org:{org}        (gauge.go Key + engine orgScope)
//
// A mixed Python/Go fleet would book the same usage under two keyspaces —
// each runtime enforcing against half the truth (silent quota doubling).
//
// EXPECTED RED until TASK P5.3 lands (Go adopts the EXISTING Python keys —
// D-10 / FUTURE §2 forbid changing the Python keyspace). This test is the
// permanent tripwire and must become a CI gate on both repos (FUTURE §2).

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
)

type goldenKeys struct {
	OrgID  string `json:"org_id"`
	UserID string `json:"user_id"`
	AtUTC  string `json:"at_utc"`
	Keys   map[string]struct {
		ResourceKey string `json:"resource_key"`
		Key         string `json:"key"`
	} `json:"keys"`
}

func loadGolden(t *testing.T) goldenKeys {
	t.Helper()
	raw, err := os.ReadFile("testdata/golden_keys_python_v052.json")
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}
	var g goldenKeys
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("parse golden file: %v", err)
	}
	return g
}

// goldenConfig mirrors the config the Python golden keys were derived from.
func goldenConfig() *config.Config {
	return &config.Config{
		Enforcement: config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{
			Type:    "static",
			Mapping: map[string]string{"user-1": "pro"},
		},
		Tiers: []config.Tier{
			{TierID: "pro", Limits: map[string]config.TierLimit{
				"sandboxes":     {Limit: ptrFloat(100)},
				"api_spend_usd": {Limit: ptrFloat(1000)},
				"api_calls":     {Limit: ptrFloat(10000)},
			}},
		},
		Resources: []config.ResourceDef{
			{ResourceKey: "sandboxes", CounterType: config.CounterGauge},
			{ResourceKey: "api_spend_usd", CounterType: config.CounterAccumulator, ResetPeriod: config.ResetMonthly},
			{ResourceKey: "api_calls", CounterType: config.CounterRate, WindowSeconds: 3600},
		},
	}
}

// TestGoldenKeys_CrossRuntimeParity_QG03 spends through the Go engine and
// asserts the value landed under the key the PYTHON fleet reads. Prefix is
// set to "quota" — the most charitable choice for Go (Python hardcodes the
// literal "quota:" head, base.py:20-21) — and the shapes still diverge.
func TestGoldenKeys_CrossRuntimeParity_QG03(t *testing.T) {
	golden := loadGolden(t)
	at, err := time.Parse(time.RFC3339, golden.AtUTC)
	if err != nil {
		t.Fatalf("golden at_utc: %v", err)
	}

	cfg := goldenConfig()
	factory := counters.NewMemoryFactory("quota")
	e := newEngine(t, cfg)
	e.Factory = factory
	e.Clock = func() time.Time { return at }
	ctx := context.Background()
	in := func(rk string, cost float64) CheckInput {
		return CheckInput{UserID: golden.UserID, OrgID: golden.OrgID, ResourceKey: rk, Cost: cost}
	}

	t.Run("gauge_org", func(t *testing.T) {
		if _, err := e.Spend(ctx, in(golden.Keys["gauge_org"].ResourceKey, 1)); err != nil {
			t.Fatalf("Spend: %v", err)
		}
		want := golden.Keys["gauge_org"].Key
		if _, found, _ := factory.Floats.GetFloat(ctx, want); !found {
			t.Errorf("QG-03: Go gauge spend did not write the Python key.\n  python (golden): %s\n  go (actual):     %s",
				want, factory.Gauge("sandboxes").Key("org:"+golden.OrgID))
		}
	})

	t.Run("accumulator_monthly", func(t *testing.T) {
		if _, err := e.Spend(ctx, in(golden.Keys["accumulator_monthly"].ResourceKey, 2.5)); err != nil {
			t.Fatalf("Spend: %v", err)
		}
		want := golden.Keys["accumulator_monthly"].Key
		if _, found, _ := factory.Floats.GetFloat(ctx, want); !found {
			t.Errorf("QG-03: Go accumulator spend did not write the Python key.\n  python (golden): %s\n  go (actual):     %s",
				want, factory.Accumulator("api_spend_usd", config.ResetMonthly).PeriodKey("org:"+golden.OrgID, at))
		}
	})

	t.Run("rate", func(t *testing.T) {
		if _, err := e.Spend(ctx, in(golden.Keys["rate"].ResourceKey, 1)); err != nil {
			t.Fatalf("Spend: %v", err)
		}
		want := golden.Keys["rate"].Key
		n, err := factory.Rates.Count(ctx, want, at, 3600*time.Second)
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		if n != 1 {
			goCfg := goldenConfig().Resources[2]
			t.Errorf("QG-03: Go rate spend did not record under the Python key.\n  python (golden): %s\n  go (actual):     %s",
				want, factory.Rate(goCfg).Key("org:"+golden.OrgID))
		}
	})
}

// TestGoldenKeys_PerUserPartition_QG04 is split out because it is a
// DIFFERENT finding: Python maintains a per-user partition alongside the
// org gauge (gauge.py:22-23, written on every increment_user); Go has no
// per-user scoping at all (engine.go orgScope "for now" — QG-04, E-D5).
// EXPECTED RED until TASK P5.4 (implement per-user scoping or remove it
// from the config schema). Kept separate so P5.3 can go green without P5.4.
func TestGoldenKeys_PerUserPartition_QG04(t *testing.T) {
	golden := loadGolden(t)
	at, err := time.Parse(time.RFC3339, golden.AtUTC)
	if err != nil {
		t.Fatalf("golden at_utc: %v", err)
	}
	factory := counters.NewMemoryFactory("quota")
	e := newEngine(t, goldenConfig())
	e.Factory = factory
	e.Clock = func() time.Time { return at }
	ctx := context.Background()

	if _, err := e.Spend(ctx, CheckInput{
		UserID: golden.UserID, OrgID: golden.OrgID, ResourceKey: "sandboxes", Cost: 1,
	}); err != nil {
		t.Fatalf("Spend: %v", err)
	}
	want := golden.Keys["gauge_user_partition"].Key
	if _, found, _ := factory.Floats.GetFloat(ctx, want); !found {
		t.Errorf("QG-04: Go spend wrote no per-user partition; Python writes %s on every user-attributed increment (gauge.py:42-46)", want)
	}
}
