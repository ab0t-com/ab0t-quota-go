package quota

// T-G5 REDs (DOC-04 Go side, pack 20260721; design §6.2 — Capabilities gains
// Resolved provenance, mirroring Python's ResolutionPlan.provenance_block).
// D-8 binds this task: quotactl capabilities runs full Setup, and its writes
// are ENUMERATED in the work log — never asserted away. The background-loop
// writes (billing heartbeat POSTs, outbox drain publish/settle) are routed
// behind Options so a smoke run moves no money.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func newCountingServer(hits *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
}

func atomicLoad(p *int32) int32 { return atomic.LoadInt32(p) }

// TestCapabilitiesCarryResolvedProvenance is written against the JSON shape
// so it is a TRUE red pre-implementation (a struct-field reference would be
// a [build failed] false red).
func TestCapabilitiesCarryResolvedProvenance(t *testing.T) {
	t.Setenv("QUOTA_REDIS_URL", "")
	doc := `{"storage": {"redis_url": "memory://", "redis_password": "s3cret-decoy"},` + minimalCoreJSON + `}`
	q, err := Setup(context.Background(), Options{ConfigOverride: configFromJSON(t, doc)})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())

	raw, err := json.Marshal(q.Capabilities())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	resolved, ok := m["Resolved"].(map[string]any)
	if !ok || len(resolved) == 0 {
		t.Fatalf("Capabilities carry no Resolved provenance map — an operator reading " +
			"`quotactl capabilities` cannot see WHERE each dependency came from (design §6.2; " +
			"ENV-11's Go twin)")
	}
	entry, ok := resolved["redis_url"].(map[string]any)
	if !ok {
		t.Fatalf("Resolved must carry the counter store; got keys %v", keysOf(resolved))
	}
	if src, _ := entry["source"].(string); src != "config storage.redis_url" {
		t.Errorf("redis_url source = %q, want \"config storage.redis_url\"", src)
	}
	// Redaction rule (§6.3): the declared password NEVER appears anywhere in
	// the capabilities output — presence only.
	if s := string(raw); json.Valid(raw) && containsStr(s, "s3cret-decoy") {
		t.Errorf("the declared redis_password value leaked into the capabilities output")
	}
}

// TestSkipBackgroundLoops_NoHeartbeat (green-time — the option did not exist
// at RED) pins the D-8 routing: with SkipBackgroundLoops, the billing
// heartbeat loop is never constructed/started and the decoy billing endpoint
// receives nothing during Setup.
func TestSkipBackgroundLoops_NoHeartbeat(t *testing.T) {
	var hits int32
	decoy := newCountingServer(&hits)
	defer decoy.Close()
	t.Setenv("AB0T_QUOTA_BILLING_URL", decoy.URL)
	t.Setenv("AB0T_QUOTA_SERVICE_TOKEN", "tok")
	t.Setenv("QUOTA_REDIS_URL", "")

	// allow_ephemeral: wiring a billing client asserts paid intent (D-44), and
	// this smoke fixture has no durable outbox — the dev escape lets Setup
	// boot with billing chain OFF, which is exactly a smoke run's shape.
	doc := `{"storage": {"redis_url": "memory://"},
	         "outbox": {"allow_ephemeral": true},` + minimalCoreJSON + `}`
	q, err := Setup(context.Background(), Options{
		ConfigOverride:      configFromJSON(t, doc),
		SkipBackgroundLoops: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	if q.Heartbeat != nil {
		t.Error("SkipBackgroundLoops must not construct/start the billing heartbeat loop")
	}
	if n := atomicLoad(&hits); n != 0 {
		t.Errorf("smoke run sent %d request(s) to the billing endpoint — a pre-deploy check must not beat", n)
	}
	if !q.Capabilities().Billing {
		t.Error("the billing CLIENT should still be wired (assessment, not amputation)")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func containsStr(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
