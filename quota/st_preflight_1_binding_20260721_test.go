package quota

// ST-PREFLIGHT-1 — the T-G9 extension of the Go binding. The token/policy/
// floor legs are already bound in quota/preflight_guard_d72_test.go (a LOCKED
// file — never edited; this is a NEW file beside it). Bound here are the
// declared legs that binding does not assert:
//   - capability_keys: the three verdicts are PUBLISHED, not just computed;
//   - "appendonly=no alone must NOT block startup" (the counter's fatal
//     property is EVICTION — a restart-lost counter heals, D-28);
//   - "an unreadable version is `unknown`, recorded, not a refusal";
//   - the ERROR-CLASS substrate of Python's preflight exit taxonomy
//     (EXIT_CONFIG/EXIT_GATE/EXIT_REACH): Go's refusals are TYPED and
//     distinguishable — config errors are *config.ConfigError with a stable
//     code; topology refusals (including the T-G3 probe-failure state, the
//     reachability/credential class) unwrap to ErrClusterTopology; gate
//     refusals unwrap to redisguard sentinels. The canonical declares no CLI
//     exit codes for Go today; if it ever does, quotactl maps these classes.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/redisguard"
)

func loadSTPreflight1CapKeys(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../conformance/scenarios.json")
	if err != nil {
		t.Fatalf("read scenarios.json: %v", err)
	}
	var doc struct {
		Structural []struct {
			ID             string   `json:"id"`
			CapabilityKeys []string `json:"capability_keys"`
		} `json:"structural_conformance"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, it := range doc.Structural {
		if it.ID == "ST-PREFLIGHT-1" {
			if len(it.CapabilityKeys) == 0 {
				t.Fatal("ST-PREFLIGHT-1 declares no capability_keys — the publication contract vanished")
			}
			return it.CapabilityKeys
		}
	}
	t.Fatal("ST-PREFLIGHT-1 must be declared")
	return nil
}

// One Setup against miniredis: no AOF (appendonly is not even configurable
// there), unreadable INFO version — and it must BOOT, publishing all three
// declared verdicts. That single fact binds three clauses at once.
func TestSTPreflight1_CapabilityKeysPublished_AOFAndVersionClausesHold(t *testing.T) {
	keys := loadSTPreflight1CapKeys(t)
	mr := miniredis.RunT(t)
	t.Setenv("QUOTA_REDIS_URL", "")
	doc := `{"storage": {"redis_url": "redis://` + mr.Addr() + `",
	          "redis_cluster_confirmed_disabled": true},` + minimalCoreJSON + `}`
	q, err := Setup(context.Background(), Options{ConfigOverride: configFromJSON(t, doc)})
	if err != nil {
		t.Fatalf("a non-evicting, non-persisted (no AOF) Redis with unreadable version must "+
			"BOOT — eviction is the counter's fatal property, persistence and version are not: %v", err)
	}
	defer q.Close(context.Background())
	cap := q.Capabilities()
	published := map[string]string{
		"counter_eviction_policy": cap.CounterEvictionPolicy,
		"redis_scripting":         cap.RedisScripting,
		"redis_version":           cap.RedisVersion,
	}
	for _, k := range keys {
		v, known := published[k]
		if !known {
			t.Errorf("declared capability key %q has no Go publication mapping", k)
			continue
		}
		if strings.TrimSpace(v) == "" {
			t.Errorf("capability %q is EMPTY after a gated boot — a verdict nobody can read is "+
				"not a verdict (D-40/D-51)", k)
		}
	}
	if !strings.HasPrefix(strings.ToLower(cap.RedisVersion), "unknown") {
		t.Errorf("redis_version = %q — an unreadable version must be recorded as `unknown`, "+
			"never refused and never invented", cap.RedisVersion)
	}
}

// The error-class substrate: the three refusal classes Python's preflight
// maps to distinct exit codes are TYPED and mutually distinguishable in Go.
func TestSTPreflight1_ErrorClassesAreDistinguishable(t *testing.T) {
	t.Setenv("QUOTA_REDIS_URL", "")

	// Class "config" (Python EXIT_CONFIG): typed ConfigError with stable code.
	_, err := Setup(context.Background(), Options{
		ConfigOverride: configFromJSON(t, `{`+minimalCoreJSON+`}`)})
	var cfgErr *config.ConfigError
	if !asConfigError(err, &cfgErr) || cfgErr.Code == "" {
		t.Errorf("config-class refusal must be a coded *config.ConfigError, got %T: %v", err, err)
	}

	// Class "gate would refuse" (EXIT_GATE): eviction refusal unwraps to the
	// redisguard sentinel — checked at the constructor (the gate wiring is
	// covered by the locked D-72 file).
	if !errors.Is(redisguard.CounterEvictionError("allkeys-lru"), redisguard.ErrCounterEviction) {
		t.Error("gate-class refusal lost its sentinel (redisguard.ErrCounterEviction)")
	}

	// Class "cannot reach / credentials" (EXIT_REACH): the T-G3 probe-failed
	// topology state unwraps to ErrClusterTopology AND its text points at
	// credentials/reachability, never at the gate assertion flags.
	reachErr := TopologyError(TopologyProbeFailed, "INFO cluster probe failed (auth): NOAUTH")
	if !errors.Is(reachErr, ErrClusterTopology) {
		t.Error("reachability-class refusal lost its sentinel (ErrClusterTopology)")
	}
	if !strings.Contains(reachErr.Error(), "NOT a topology verdict") ||
		strings.Contains(reachErr.Error(), "set storage.redis_cluster_confirmed_disabled: true") {
		t.Errorf("reachability-class refusal must name the credential/reachability condition and "+
			"never advise the topology assertion: %v", reachErr)
	}

	// And the classes are pairwise distinguishable.
	if errors.Is(reachErr, redisguard.ErrCounterEviction) {
		t.Error("reachability and gate classes must not alias")
	}
	var ce *config.ConfigError
	if asConfigError(reachErr, &ce) {
		t.Error("reachability and config classes must not alias")
	}
}
