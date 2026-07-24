package counters

// K-8 — Go migration parity (Python keyspace_migration.py, K-5) + boot
// guards. Full state machine with an injected clock, plus the D-14 planted
// offenders: (a) a broken dual-write must turn verify RED; (c) a young dual
// window must refuse the flip. (Plant (b) — pre-dual idem replay across the
// flip never double-counts — is bound at the engine level, KS6.)

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func migHarness(t *testing.T) (context.Context, *redis.Client, *Migrator, *time.Time) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	now := time.Unix(1_700_000_000, 0).UTC()
	m := &Migrator{C: c, Service: "test-svc", MaxRateWindowSeconds: 3600,
		Now: func() time.Time { return now }}
	return context.Background(), c, m, &now
}

func TestMigrationFullStateMachine(t *testing.T) {
	ctx, c, m, now := migHarness(t)
	// The v1 world: a gauge and a mid-period accumulator.
	if err := c.Set(ctx, "quota:org1:sandbox.concurrent:gauge", "3", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(ctx, "quota:org1:api.spend:acc:2026-07", "41.5", 0).Err(); err != nil {
		t.Fatal(err)
	}

	// backfill before dual-on refuses (seeding without dual live drifts).
	if _, _, err := m.Backfill(ctx, 0); err == nil {
		t.Fatal("backfill before dual-on must refuse")
	}
	if _, err := m.DualOn(ctx); err != nil {
		t.Fatal(err)
	}
	scanned, seeded, err := m.Backfill(ctx, 0)
	if err != nil || scanned != 2 || seeded != 2 {
		t.Fatalf("backfill scanned=%d seeded=%d err=%v (want 2/2)", scanned, seeded, err)
	}
	// resumable: a second pass seeds nothing, breaks nothing.
	if _, seeded2, err := m.Backfill(ctx, 0); err != nil || seeded2 != 0 {
		t.Fatalf("backfill must be seed-if-absent idempotent, re-seeded %d err=%v", seeded2, err)
	}
	v2g, _ := c.Get(ctx, "quota:v2:{test-svc/org1}:sandbox.concurrent:gauge").Result()
	if v2g != "3" {
		t.Fatalf("no counter may read zero while its v1 twin holds a value: v2 gauge=%q", v2g)
	}

	vr, err := m.Verify(ctx, 0)
	if err != nil || !vr.OK || vr.Compared != 2 {
		t.Fatalf("verify: %+v err=%v", vr, err)
	}
	// Plant (c): the flip gate is machine-enforced — a young dual refuses.
	if _, err := m.Flip(ctx); err == nil || !strings.Contains(err.Error(), "too young") {
		t.Fatalf("flip before the idem-TTL window must refuse: %v", err)
	}
	*now = now.Add((IdemTTLSeconds + 10) * time.Second)
	if _, err := m.Verify(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Flip(ctx); err != nil {
		t.Fatalf("flip after the window + green verify must proceed: %v", err)
	}

	// Reap: refused without in-run verify on a FRESH instance; refused
	// without the shared-scope confirmation; then deletes only v1 keys.
	fresh := &Migrator{C: c, Service: "test-svc", Now: m.Now}
	if _, err := fresh.Reap(ctx, true); err == nil || !strings.Contains(err.Error(), "verify") {
		t.Fatalf("reap without in-run verify must refuse: %v", err)
	}
	if _, err := fresh.Verify(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Reap(ctx, false); err == nil || !strings.Contains(err.Error(), "confirm") {
		t.Fatalf("reap without the scope confirmation must refuse: %v", err)
	}
	deleted, err := fresh.Reap(ctx, true)
	if err != nil || deleted != 2 {
		t.Fatalf("reap deleted=%d err=%v (want 2)", deleted, err)
	}
	marker, v1n, v2n, err := fresh.Status(ctx)
	if err != nil || marker == nil || marker.HighWater != "v2-final" || v1n != 0 || v2n != 2 {
		t.Fatalf("post-reap: marker=%+v v1=%d v2=%d err=%v", marker, v1n, v2n, err)
	}
	if v2g, _ := c.Get(ctx, "quota:v2:{test-svc/org1}:sandbox.concurrent:gauge").Result(); v2g != "3" {
		t.Fatalf("reap changed a v2 value: %q", v2g)
	}
}

// Plant (a): a broken dual-write (v1 mutated with NO v2 maintenance after
// backfill) must turn verify RED — the instrument is seen to fail.
func TestPlantBrokenDualWriteIsCaughtByVerify(t *testing.T) {
	ctx, c, m, _ := migHarness(t)
	_ = c.Set(ctx, "quota:org1:sandbox.concurrent:gauge", "3", 0).Err()
	if _, err := m.DualOn(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Backfill(ctx, 0); err != nil {
		t.Fatal(err)
	}
	// The plant: a single-shape writer mutates v1 only.
	if err := c.IncrByFloat(ctx, "quota:org1:sandbox.concurrent:gauge", 2).Err(); err != nil {
		t.Fatal(err)
	}
	vr, err := m.Verify(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if vr.OK {
		t.Fatal("verify passed over a broken dual-write — the instrument proves nothing")
	}
	if reason := m.FlipGate(&Marker{Phase: "dual", DualSince: 1}); reason == "" {
		t.Fatal("flip gate open after a RED verify")
	}
}

func TestBootGuardsCFG011AndCFG012(t *testing.T) {
	ctx, c, _, _ := migHarness(t)
	// Greenfield: empty keyspace, no marker — v2 boots.
	ks, _ := NewKeyspace("test-svc", 2, false)
	if m, err := CheckBootKeyspace(ctx, c, ks); err != nil || m != nil {
		t.Fatalf("greenfield v2 must boot: marker=%v err=%v", m, err)
	}
	// CFG-012: live v1 counter keys + (2,false) + no completed migration.
	_ = c.Set(ctx, "quota:org1:sandbox.concurrent:gauge", "3", 0).Err()
	if _, err := CheckBootKeyspace(ctx, c, ks); err == nil ||
		!strings.Contains(err.Error(), "QUOTA-CFG-012") {
		t.Fatalf("brownfield v2 boot must refuse with QUOTA-CFG-012: %v", err)
	}
	// CFG-011: marker v2-final + a v1/dual config.
	raw, _ := json.Marshal(Marker{HighWater: "v2-final", Phase: "reaped"})
	_ = c.Set(ctx, MarkerKey("test-svc"), raw, 0).Err()
	v1ks, _ := NewKeyspace("test-svc", 1, false)
	if _, err := CheckBootKeyspace(ctx, c, v1ks); err == nil ||
		!strings.Contains(err.Error(), "QUOTA-CFG-011") {
		t.Fatalf("v1 boot over a reaped migration must refuse with QUOTA-CFG-011: %v", err)
	}
	dualKs, _ := NewKeyspace("test-svc", 2, true)
	if _, err := CheckBootKeyspace(ctx, c, dualKs); err == nil ||
		!strings.Contains(err.Error(), "QUOTA-CFG-011") {
		t.Fatalf("dual boot over a reaped migration must refuse: %v", err)
	}
	// Negative control: the matching (2,false) config boots and reads the marker.
	if m, err := CheckBootKeyspace(ctx, c, ks); err != nil || m == nil || m.HighWater != "v2-final" {
		t.Fatalf("(2,false) over v2-final must boot with the marker: %v %v", m, err)
	}
	// Unscoped v1 consumer: no service — guard is a no-op (nothing recorded).
	if m, err := CheckBootKeyspace(ctx, c, Keyspace{}); err != nil || m != nil {
		t.Fatalf("unscoped v1 must not consult a scoped marker: %v %v", m, err)
	}
}

func TestV1StragglerCheckFiresOnlyPostFlip(t *testing.T) {
	ctx, c, _, _ := migHarness(t)
	_ = c.Set(ctx, "quota:org1:sandbox.concurrent:gauge", "3", 0).Err()
	fired := 0
	// Pre-flip: quiet (v1 is still authoritative).
	n, err := CheckV1Stragglers(ctx, c, "test-svc", func(int, []string) { fired++ })
	if err != nil || n != 1 || fired != 0 {
		t.Fatalf("pre-flip: n=%d fired=%d err=%v (want 1/0)", n, fired, err)
	}
	raw, _ := json.Marshal(Marker{Phase: "flipped", HighWater: "flipped"})
	_ = c.Set(ctx, MarkerKey("test-svc"), raw, 0).Err()
	n, err = CheckV1Stragglers(ctx, c, "test-svc", func(int, []string) { fired++ })
	if err != nil || n != 1 || fired != 1 {
		t.Fatalf("post-flip straggler must ALERT: n=%d fired=%d err=%v", n, fired, err)
	}
	// Negative control: no stragglers → no alert.
	_ = c.Del(ctx, "quota:org1:sandbox.concurrent:gauge").Err()
	n, err = CheckV1Stragglers(ctx, c, "test-svc", func(int, []string) { fired++ })
	if err != nil || n != 0 || fired != 1 {
		t.Fatalf("clean post-flip must not alert: n=%d fired=%d err=%v", n, fired, err)
	}
}
