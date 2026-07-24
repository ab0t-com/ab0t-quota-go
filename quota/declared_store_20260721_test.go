package quota

// T-G1 REDs (GO-01, P0 — pack 20260721_shared_lib_declared_not_discovered).
// Contract: an UNDECLARED counter store is a STARTUP ERROR, never a silent
// downgrade to a per-process counter; in-memory survives only as the explicit
// declaration `"redis_url": "memory://"` (DECISIONS.md D-5(b)); a set,
// namespaced QUOTA_REDIS_URL beats an explicit config null (ST-RESOLVE-1
// clause 3). Error shape: design_dependency_resolution_20260721.md §5 ex. 3.
//
// Configs are built through the JSON document — the way a consumer supplies
// them — so "absent" and "null" mean what they mean in quota-config.json and
// the tests survive the StorageConfig type change they demand.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

const minimalCoreJSON = `
  "enforcement": {"enabled": true},
  "tier_provider": {"type": "static", "mapping": {"alice": "pro"}},
  "tiers": [{"tier_id": "pro", "limits": {"sandbox.concurrent": {"limit": 2}}}],
  "resources": [{"resource_key": "sandbox.concurrent", "counter_type": "gauge"}]
`

func configFromJSON(t *testing.T, doc string) *config.Config {
	t.Helper()
	var cfg config.Config
	if err := json.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	return &cfg
}

// assertDeclaredStoreContract is the shared contract check: with NO declared
// store (key absent or explicit null, no namespaced env), setup must refuse
// with an error naming storage.redis_url and QUOTA_REDIS_URL. NC-8 feeds it
// brokenSetup (today's shipped behaviour, frozen) to prove it can fail.
func assertDeclaredStoreContract(t testing.TB, setup func(context.Context, Options) (*Quota, error)) {
	for name, doc := range map[string]string{
		"absent": `{` + minimalCoreJSON + `}`,
		"null":   `{"storage": {"redis_url": null},` + minimalCoreJSON + `}`,
	} {
		var cfg config.Config
		if err := json.Unmarshal([]byte(doc), &cfg); err != nil {
			t.Errorf("unmarshal (%s): %v", name, err)
			continue
		}
		q, err := setup(context.Background(), Options{ConfigOverride: &cfg})
		if err == nil {
			fs := ""
			if q != nil {
				fs = q.Capabilities().FloatStore
				defer q.Close(context.Background())
			}
			t.Errorf("GO-01 (%s): an UNDECLARED counter store must be a STARTUP ERROR, got a "+
				"running Quota with FloatStore=%q — a silent per-process counter admits the full "+
				"limit in every replica and zeroes on restart (over-admission on a money path "+
				"behind a green health check)", name, fs)
			continue
		}
		for _, tok := range []string{"storage.redis_url", "QUOTA_REDIS_URL"} {
			if !strings.Contains(err.Error(), tok) {
				t.Errorf("(%s) the refusal must name %q verbatim (§5 error contract); got: %v",
					name, tok, err)
			}
		}
	}
}

func TestUndeclaredStoreIsAStartupError(t *testing.T) {
	// No namespaced env declaration either — the error path, not the env tier.
	t.Setenv("QUOTA_REDIS_URL", "")
	assertDeclaredStoreContract(t, Setup)
}

func TestExplicitMemoryModeStillWorks(t *testing.T) {
	t.Setenv("QUOTA_REDIS_URL", "")
	doc := `{"storage": {"redis_url": "memory://"},` + minimalCoreJSON + `}`
	q, err := Setup(context.Background(), Options{ConfigOverride: configFromJSON(t, doc)})
	if err != nil {
		t.Fatalf("explicit \"memory://\" is a DECLARATION (D-5(b)) and must boot: %v", err)
	}
	defer q.Close(context.Background())
	cap := q.Capabilities()
	if cap.FloatStore != "memory" {
		t.Errorf("FloatStore = %q, want \"memory\"", cap.FloatStore)
	}
	if cap.RedisTopology != TopologyNA {
		t.Errorf("RedisTopology = %q, want %q (the affirmative n/a of setup.go's no-store path)",
			cap.RedisTopology, TopologyNA)
	}
	if why, ok := cap.WhyOff["redis_store"]; ok {
		t.Errorf("declared memory mode is not a DEGRADATION — WhyOff[redis_store] must be "+
			"absent, got %q", why)
	}
}

func TestNamespacedEnvBeatsExplicitNull(t *testing.T) {
	// ST-RESOLVE-1 clause 3: a set, namespaced QUOTA_REDIS_URL is a declared,
	// documented source and beats an explicit config null; null with no
	// namespaced env remains an error (covered above).
	mr := miniredis.RunT(t)
	t.Setenv("QUOTA_REDIS_URL", "redis://"+mr.Addr())
	doc := `{"storage": {"redis_url": null},` + minimalCoreJSON + `}`
	q, err := Setup(context.Background(), Options{ConfigOverride: configFromJSON(t, doc)})
	if err != nil {
		t.Fatalf("explicit null + set QUOTA_REDIS_URL must resolve to the env declaration: %v", err)
	}
	defer q.Close(context.Background())
	if fs := q.Capabilities().FloatStore; fs != "redis" {
		t.Errorf("FloatStore = %q, want \"redis\" — the namespaced env declaration was not honoured", fs)
	}
}
