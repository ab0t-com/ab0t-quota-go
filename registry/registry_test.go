package registry

import (
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

func makeCfg() *config.Config {
	return &config.Config{
		Tiers: []config.Tier{
			{
				TierID: "pro",
				Limits: map[string]config.TierLimit{
					"sandbox.concurrent": {Limit: ptrFloat(25)},
				},
			},
		},
		Resources: []config.ResourceDef{
			{ResourceKey: "sandbox.concurrent", CounterType: config.CounterGauge},
		},
		ResourceBundles: map[string][]string{
			"core": {"sandbox.concurrent"},
		},
	}
}

func ptrFloat(f float64) *float64 { return &f }

func TestRegistry_LookupsAndBundles(t *testing.T) {
	r := New(makeCfg())
	if _, ok := r.Resource("sandbox.concurrent"); !ok {
		t.Error("missing resource")
	}
	if _, ok := r.Tier("pro"); !ok {
		t.Error("missing tier")
	}
	lim, ok := r.TierLimit("pro", "sandbox.concurrent")
	if !ok || lim.Limit == nil || *lim.Limit != 25 {
		t.Errorf("got %+v", lim)
	}
	if b := r.Bundle("core"); len(b) != 1 || b[0] != "sandbox.concurrent" {
		t.Errorf("bundle = %v", b)
	}
}

func TestRegistry_UnknownReturnsFalse(t *testing.T) {
	r := New(makeCfg())
	if _, ok := r.Resource("nope"); ok {
		t.Error("expected false")
	}
	if _, ok := r.TierLimit("missing", "sandbox.concurrent"); ok {
		t.Error("expected false")
	}
}

func TestRegistry_MustResourcePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	New(makeCfg()).MustResource("nope")
}
