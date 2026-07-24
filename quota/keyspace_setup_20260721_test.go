package quota

// K-8 — quota.Setup consumes the declared keyspace state (the Go twin of the
// Python K-9 wiring suite): the default stays v1, a declared v2 reaches the
// factory/engine, the boot guards (QUOTA-CFG-011/012) fire THROUGH Setup,
// and the capability surface reports the active shape + migration phase.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/engine"
)

const ksCoreJSON = `
  "service_name": "test-svc",
  "enforcement": {"enabled": true},
  "tier_provider": {"type": "static", "mapping": {"alice": "pro"}},
  "tiers": [{"tier_id": "pro", "limits": {"sandbox.concurrent": {"limit": 5}}}],
  "resources": [{"resource_key": "sandbox.concurrent", "counter_type": "gauge"}]
`

func ksSetup(t *testing.T, mr *miniredis.Miniredis, storageExtra string) (*Quota, error) {
	t.Helper()
	t.Setenv("QUOTA_REDIS_URL", "")
	doc := `{"storage": {"redis_url": "redis://` + mr.Addr() + `"` + storageExtra + `},` +
		ksCoreJSON + `}`
	return Setup(context.Background(), Options{ConfigOverride: configFromJSON(t, doc)})
}

func TestSetupDefaultKeyspaceIsV1(t *testing.T) {
	mr := miniredis.RunT(t)
	q, err := ksSetup(t, mr, "")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	if ks := q.Engine.Factory.Keyspace; ks.Enabled() {
		t.Fatalf("default must be the v1 keyspace, got %+v", ks)
	}
	if cap := q.Capabilities().Keyspace; !strings.HasPrefix(cap, "v1") {
		t.Fatalf("capability must report the v1 default, got %q", cap)
	}
}

func TestSetupConsumesDeclaredV2Greenfield(t *testing.T) {
	mr := miniredis.RunT(t)
	q, err := ksSetup(t, mr, `, "keyspace_version": 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	ks := q.Engine.Factory.Keyspace
	if ks.Version != 2 || ks.Service != "test-svc" || ks.DualWrite {
		t.Fatalf("Setup did not consume the declared keyspace: %+v", ks)
	}
	cap := q.Capabilities().Keyspace
	if !strings.Contains(cap, "v2") || !strings.Contains(cap, "phase=none") {
		t.Fatalf("capability must report shape+phase, got %q", cap)
	}
	// The engine actually WRITES the declared shape.
	if _, err := q.Engine.Spend(context.Background(), engine.CheckInput{
		UserID: "alice", OrgID: "org1", ResourceKey: "sandbox.concurrent", Cost: 1,
	}); err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer c.Close()
	v, _ := c.Get(context.Background(), "quota:v2:{test-svc/org1}:sandbox.concurrent:gauge").Result()
	if v != "1" {
		keys, _ := c.Keys(context.Background(), "*").Result()
		t.Fatalf("v2 Setup wrote no v2 key (got %v)", keys)
	}
}

func TestSetupBootGuardCFG012ThroughSetup(t *testing.T) {
	mr := miniredis.RunT(t)
	// The brownfield world: a live v1 counter key, no completed migration.
	if err := mr.Set("quota:org1:sandbox.concurrent:gauge", "3"); err != nil {
		t.Fatal(err)
	}
	q, err := ksSetup(t, mr, `, "keyspace_version": 2`)
	if err == nil {
		defer q.Close(context.Background())
		t.Fatal("brownfield v2 Setup must refuse with QUOTA-CFG-012")
	}
	if !strings.Contains(err.Error(), "QUOTA-CFG-012") {
		t.Fatalf("refusal must carry QUOTA-CFG-012, got: %v", err)
	}
}

func TestSetupBootGuardCFG011ThroughSetup(t *testing.T) {
	mr := miniredis.RunT(t)
	raw, _ := json.Marshal(counters.Marker{HighWater: "v2-final", Phase: "reaped"})
	if err := mr.Set(counters.MarkerKey("test-svc"), string(raw)); err != nil {
		t.Fatal(err)
	}
	q, err := ksSetup(t, mr, "") // default v1 against a completed migration
	if err == nil {
		defer q.Close(context.Background())
		t.Fatal("v1 Setup over a reaped migration must refuse with QUOTA-CFG-011")
	}
	if !strings.Contains(err.Error(), "QUOTA-CFG-011") {
		t.Fatalf("refusal must carry QUOTA-CFG-011, got: %v", err)
	}
}
