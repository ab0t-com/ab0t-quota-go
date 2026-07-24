package quota

// ST-RESOLVE-1 — the Go clause-level binding (T-G9, pack 20260721).
// The canonical scenarios.json (Python repo, mirrored byte-identically here)
// declares six clauses; this file asserts the SHIPPED Go behaviour satisfies
// each one, the way the ST-TOPOLOGY-1 binding does — a declared scenario
// with no binding is coverage that runs nothing (the orphan lesson).
// Python's twin: tests/test_st_resolve_1_conformance_20260721.py.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

type stResolveItem struct {
	ID                       string   `json:"id"`
	Runtimes                 []string `json:"runtimes"`
	Contract                 []string `json:"contract"`
	ConfigKey                string   `json:"config_key"`
	EnvKey                   string   `json:"env_key"`
	RequiredErrorMustContain []string `json:"required_error_must_contain"`
}

func loadSTResolve1(t *testing.T) stResolveItem {
	t.Helper()
	raw, err := os.ReadFile("../conformance/scenarios.json")
	if err != nil {
		t.Fatalf("read scenarios.json: %v", err)
	}
	var doc struct {
		Structural []stResolveItem `json:"structural_conformance"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse scenarios.json: %v", err)
	}
	for _, it := range doc.Structural {
		if it.ID == "ST-RESOLVE-1" {
			return it
		}
	}
	t.Fatal("ST-RESOLVE-1 must be declared in scenarios.json")
	return stResolveItem{}
}

// TestSTResolve1_DeclaredShape pins the declaration itself: go is a bound
// runtime, all six clauses are present, and the token list is non-empty —
// an emptied item cannot pass as coverage.
func TestSTResolve1_DeclaredShape(t *testing.T) {
	item := loadSTResolve1(t)
	goBound := false
	for _, r := range item.Runtimes {
		if r == "go" {
			goBound = true
		}
	}
	if !goBound {
		t.Fatal("ST-RESOLVE-1 must bind the go runtime")
	}
	if len(item.Contract) < 6 {
		t.Fatalf("ST-RESOLVE-1 declares %d clauses, want >= 6", len(item.Contract))
	}
	for i, want := range []string{"Clause 1", "Clause 2", "Clause 3", "Clause 4", "Clause 5", "Clause 6"} {
		if !strings.HasPrefix(item.Contract[i], want) {
			t.Errorf("contract[%d] does not start with %q: %.60s…", i, want, item.Contract[i])
		}
	}
	if len(item.RequiredErrorMustContain) == 0 {
		t.Fatal("required_error_must_contain is empty — the token contract vanished")
	}
	if item.ConfigKey != "storage.redis_url" || item.EnvKey != "QUOTA_REDIS_URL" {
		t.Errorf("declared keys drifted: config_key=%q env_key=%q", item.ConfigKey, item.EnvKey)
	}
}

// Clause 1 + Clause 6: undeclared REQUIRED dependency ⇒ typed error carrying
// every declared token — including the `previously` line (C6), with no
// secret in it.
func TestSTResolve1_Clause1And6_TypedErrorCarriesDeclaredTokens(t *testing.T) {
	item := loadSTResolve1(t)
	t.Setenv("QUOTA_REDIS_URL", "")
	for name, doc := range map[string]string{
		"absent": `{` + minimalCoreJSON + `}`,
		"null":   `{"storage": {"redis_url": null, "redis_password": "s3cret-c6-decoy"},` + minimalCoreJSON + `}`,
	} {
		_, err := Setup(context.Background(), Options{ConfigOverride: configFromJSON(t, doc)})
		if err == nil {
			t.Fatalf("(%s) an undeclared required store must be a typed error", name)
		}
		var cfgErr *config.ConfigError
		if !asConfigError(err, &cfgErr) {
			t.Errorf("(%s) the refusal must be the typed *config.ConfigError, got %T", name, err)
		} else if cfgErr.Code != "QUOTA-CFG-001" {
			t.Errorf("(%s) code = %q, want QUOTA-CFG-001", name, cfgErr.Code)
		}
		for _, tok := range item.RequiredErrorMustContain {
			if !strings.Contains(err.Error(), tok) {
				t.Errorf("(%s) declared token %q missing from the refusal", name, tok)
			}
		}
		if strings.Contains(err.Error(), "s3cret-c6-decoy") {
			t.Errorf("(%s) a declared secret leaked into the refusal (C6 redaction)", name)
		}
	}
}

func asConfigError(err error, target **config.ConfigError) bool {
	for err != nil {
		if ce, ok := err.(*config.ConfigError); ok {
			*target = ce
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Clause 2: null != absent != set — three-way membership, never truthiness.
func TestSTResolve1_Clause2_ThreeWayMembership(t *testing.T) {
	var null, absent, set struct {
		S config.StorageConfig `json:"storage"`
	}
	mustUnmarshal(t, `{"storage":{"redis_url":null}}`, &null)
	mustUnmarshal(t, `{"storage":{}}`, &absent)
	mustUnmarshal(t, `{"storage":{"redis_url":"redis://x:1/0"}}`, &set)
	if !null.S.RedisURL.IsNull() || null.S.RedisURL.IsAbsent() {
		t.Error("explicit null must read as null, not absent")
	}
	if !absent.S.RedisURL.IsAbsent() || absent.S.RedisURL.IsNull() {
		t.Error("absent must read as absent, not null")
	}
	if v, ok := set.S.RedisURL.Get(); !ok || v != "redis://x:1/0" {
		t.Error("a set value must read as declared")
	}
}

func mustUnmarshal(t *testing.T, doc string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(doc), v); err != nil {
		t.Fatal(err)
	}
}

// Clause 3: a set namespaced env declaration beats an explicit config null.
func TestSTResolve1_Clause3_NamespacedEnvBeatsNull(t *testing.T) {
	mr := miniredis.RunT(t)
	t.Setenv("QUOTA_REDIS_URL", "redis://"+mr.Addr())
	doc := `{"storage": {"redis_url": null},` + minimalCoreJSON + `}`
	q, err := Setup(context.Background(), Options{ConfigOverride: configFromJSON(t, doc)})
	if err != nil {
		t.Fatalf("null + set QUOTA_REDIS_URL must boot on the env declaration: %v", err)
	}
	defer q.Close(context.Background())
	if fs := q.Capabilities().FloatStore; fs != "redis" {
		t.Errorf("FloatStore = %q, want redis (env declaration honoured)", fs)
	}
}

// Clause 4 (Go leg): in-memory only by explicit declaration.
func TestSTResolve1_Clause4_ExplicitMemoryDeclaration(t *testing.T) {
	t.Setenv("QUOTA_REDIS_URL", "")
	doc := `{"storage": {"redis_url": "memory://"},` + minimalCoreJSON + `}`
	q, err := Setup(context.Background(), Options{ConfigOverride: configFromJSON(t, doc)})
	if err != nil {
		t.Fatalf("explicit memory:// must boot: %v", err)
	}
	defer q.Close(context.Background())
	cap := q.Capabilities()
	if cap.FloatStore != "memory" || cap.RedisTopology != TopologyNA {
		t.Errorf("FloatStore=%q RedisTopology=%q — memory:// must be the declared dev mode", cap.FloatStore, cap.RedisTopology)
	}
	if _, degraded := cap.WhyOff["redis_store"]; degraded {
		t.Error("a declaration is not a degradation")
	}
}

// Clause 5: the separately-declared redis_password field BEATS a
// URL-embedded one (Go already implements the ruled direction, D-5(a)); when
// both are set and DIFFER, a WARNING names the winning source.
func TestSTResolve1_Clause5_DeclaredPasswordBeatsURL_WithWarning(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.RequireAuth("field-wins-pass")
	t.Setenv("QUOTA_REDIS_URL", "")

	var buf bytes.Buffer
	old := slog.Default()
	defer slog.SetDefault(old)

	doc := `{"storage": {
	          "redis_url": "redis://:url-stale-pass@` + mr.Addr() + `/0",
	          "redis_password": "field-wins-pass",
	          "redis_cluster_confirmed_disabled": true
	        },` + minimalCoreJSON + `}`
	q, err := Setup(context.Background(), Options{
		ConfigOverride: configFromJSON(t, doc),
		Logger:         slog.New(slog.NewTextHandler(&buf, nil)),
	})
	if err != nil {
		t.Fatalf("the declared password field must win (server requires it): %v", err)
	}
	defer q.Close(context.Background())
	if fs := q.Capabilities().FloatStore; fs != "redis" {
		t.Fatalf("FloatStore = %q — the field password did not authenticate", fs)
	}

	logs := buf.String()
	if !strings.Contains(logs, "storage.redis_password") ||
		!strings.Contains(strings.ToLower(logs), "url") ||
		!strings.Contains(strings.ToLower(logs), "differ") {
		t.Errorf("Clause 5: both password sources set and DIFFERING must WARN naming the "+
			"winning source (storage.redis_password) and the loser (URL userinfo) — logs:\n%s", logs)
	}
	for _, secret := range []string{"field-wins-pass", "url-stale-pass"} {
		if strings.Contains(logs, secret) {
			t.Errorf("a password value leaked into the logs: %q", secret)
		}
	}
}

// Clause 7 (T-27): the D-2 retry numbers are the DECLARED cross-runtime
// contract, not a convention maintained by diffing the other runtime.
// Structural leg: Go's constants equal the declared retry_contract exactly.
// Behavioural leg: an auth failure refuses without consuming the declared
// budget. Planted drift in either constant turns this RED.
func TestSTResolve1_Clause7_RetryContractPinned(t *testing.T) {
	raw, err := os.ReadFile("../conformance/scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Structural []struct {
			ID       string   `json:"id"`
			Contract []string `json:"contract"`
			Retry    *struct {
				ConfigKey        string  `json:"config_key"`
				DefaultSeconds   float64 `json:"default_seconds"`
				ZeroMeans        string  `json:"zero_means"`
				BackoffInitial   float64 `json:"backoff_initial_seconds"`
				BackoffCap       float64 `json:"backoff_cap_seconds"`
				RetriesKind      string  `json:"retries_kind"`
				AuthNeverBudget  bool    `json:"auth_never_consumes_budget"`
			} `json:"retry_contract"`
		} `json:"structural_conformance"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, it := range doc.Structural {
		if it.ID != "ST-RESOLVE-1" {
			continue
		}
		if it.Retry == nil {
			t.Fatal("ST-RESOLVE-1 declares no retry_contract — the D-2 numbers are back to " +
				"agreement-by-diffing (T-27's defect)")
		}
		if len(it.Contract) < 7 || !strings.HasPrefix(it.Contract[6], "Clause 7") {
			t.Fatal("Clause 7 missing from the declared contract")
		}
		r := it.Retry
		if r.ConfigKey != "storage.connect_retry_seconds" {
			t.Errorf("declared config_key %q drifted", r.ConfigKey)
		}
		if defaultConnectRetrySeconds != r.DefaultSeconds {
			t.Errorf("Go default = %v, declared %v — runtime drifted from the contract",
				defaultConnectRetrySeconds, r.DefaultSeconds)
		}
		if got := connectBackoffInitial.Seconds(); got != r.BackoffInitial {
			t.Errorf("Go backoff initial = %vs, declared %vs", got, r.BackoffInitial)
		}
		if got := connectBackoffCap.Seconds(); got != r.BackoffCap {
			t.Errorf("Go backoff cap = %vs, declared %vs", got, r.BackoffCap)
		}
		if r.RetriesKind != "unreachable" || !r.AuthNeverBudget {
			t.Errorf("declared kind-selectivity drifted: %+v", r)
		}

		// Behavioural leg, tied to the DECLARED numbers: wrong password with
		// the DECLARED DEFAULT budget must refuse in far less than the
		// budget (bounded here by the declared backoff cap — an auth refusal
		// needs no backoff at all).
		mr := miniredis.RunT(t)
		mr.RequireAuth("right-pass")
		t.Setenv("QUOTA_REDIS_URL", "")
		doc := `{"storage": {"redis_url": "redis://:wrong@` + mr.Addr() + `/0"},` + minimalCoreJSON + `}`
		start := time.Now()
		_, serr := Setup(context.Background(), Options{ConfigOverride: configFromJSON(t, doc)})
		elapsed := time.Since(start)
		if serr == nil {
			t.Fatal("auth failure must refuse")
		}
		if elapsed.Seconds() > r.BackoffCap {
			t.Errorf("auth refusal took %v with the default budget %vs — the budget was consumed "+
				"by a kind that must never consume it", elapsed, r.DefaultSeconds)
		}
		return
	}
	t.Fatal("ST-RESOLVE-1 not declared")
}
