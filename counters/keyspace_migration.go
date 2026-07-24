package counters

// K-8 — the Go keyspace migration engine, ported from Python
// ab0t_quota/keyspace_migration.py (K-5): dual-on → backfill → verify →
// flip → reap, plus the boot guards (QUOTA-CFG-011/012) and the post-flip
// v1-straggler check. All verbs idempotent and resumable — every verb
// re-reads the marker from storage. CLI wiring (quotactl keyspace …) is the
// tooling lane's; this file is the mechanism.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// IdemTTLSeconds is the latch TTL the flip gate must outwait (mirrors the
// 86400s idem TTL both runtimes claim with).
const IdemTTLSeconds = 86400

const migrationEps = 1e-9

// Backfilled VALUE tails; rate is dual-written never copied, latches are
// TTL-bounded dual-claims (spec §5 phase 2 / keyspace_migration.py).
var backfillTails = map[string]bool{"gauge": true, "acc": true}
var reapTails = map[string]bool{"gauge": true, "acc": true, "rate": true, "idem": true, "idemgen": true}

// Atomic seed-if-absent — same semantics as the hot path's seedv2 (§6.1),
// TTL carried. KEYS[1]=v1, KEYS[2]=v2.
var migrationSeedScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[2]) == 0 then
  local v = redis.call('GET', KEYS[1])
  if v then
    redis.call('SET', KEYS[2], v)
    local t = redis.call('PTTL', KEYS[1])
    if tonumber(t) > 0 then redis.call('PEXPIRE', KEYS[2], t) end
    return 1
  end
end
return 0
`)

// MigrationError — a verb refused to run; the message names why.
type MigrationError struct{ Reason string }

func (e *MigrationError) Error() string { return "keyspace migration: " + e.Reason }

// KeyspaceConfigError is the typed boot refusal (QUOTA-CFG-011/012) — the Go
// twin of Python's QuotaConfigError for the keyspace boot guards.
type KeyspaceConfigError struct {
	Code   string
	Detail string
}

func (e *KeyspaceConfigError) Error() string {
	return "[" + e.Code + "] " + e.Detail
}

// Marker is the per-service migration marker (spec §3.1).
type Marker struct {
	HighWater string  `json:"high_water,omitempty"`
	Phase     string  `json:"phase,omitempty"`
	DualSince float64 `json:"dual_since,omitempty"`
	UpdatedAt float64 `json:"updated_at,omitempty"`
	By        string  `json:"by,omitempty"`
}

// ReadMarker returns the migration marker, nil when absent.
func ReadMarker(ctx context.Context, c redis.Cmdable, service string) (*Marker, error) {
	raw, err := c.Get(ctx, MarkerKey(service)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Marker
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("keyspace marker unparseable: %w", err)
	}
	return &m, nil
}

// Migrator drives one service scope's v1→v2 migration against its Redis.
type Migrator struct {
	C                    redis.Cmdable
	Service              string
	MaxRateWindowSeconds int
	Now                  func() time.Time // injectable for tests

	verifiedOK bool // this-instance verify gate (reap/flip never run on a stale verdict)
}

func (m *Migrator) now() float64 {
	if m.Now != nil {
		return float64(m.Now().UnixNano()) / 1e9
	}
	return float64(time.Now().UnixNano()) / 1e9
}

func (m *Migrator) ks() Keyspace { return Keyspace{Service: m.Service, Version: 2} }

func (m *Migrator) writeMarker(ctx context.Context, mutate func(*Marker)) (*Marker, error) {
	cur, err := ReadMarker(ctx, m.C, m.Service)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		cur = &Marker{}
	}
	mutate(cur)
	cur.UpdatedAt = m.now()
	if cur.By == "" {
		cur.By = "ab0t-quota-go keyspace migrator"
	}
	raw, _ := json.Marshal(cur)
	if err := m.C.Set(ctx, MarkerKey(m.Service), raw, 0).Err(); err != nil {
		return nil, err
	}
	return cur, nil
}

// DualOn records the dual window start. Idempotent: DualSince is kept on
// re-run (the flip gate measures from the FIRST dual-on).
func (m *Migrator) DualOn(ctx context.Context) (*Marker, error) {
	cur, err := ReadMarker(ctx, m.C, m.Service)
	if err != nil {
		return nil, err
	}
	if cur != nil && cur.HighWater == "v2-final" {
		return nil, &MigrationError{Reason: "keyspace regression: storage records a completed " +
			"v2 migration (marker high_water=v2-final) — dual-on for v1 would resurrect " +
			"orphaned keys (QUOTA-CFG-011 posture)"}
	}
	if cur != nil && (cur.Phase == "dual" || cur.Phase == "flipped") {
		return cur, nil
	}
	return m.writeMarker(ctx, func(mk *Marker) {
		mk.Phase, mk.HighWater, mk.DualSince = "dual", "dual", m.now()
	})
}

func (m *Migrator) scanV1(ctx context.Context, tails map[string]bool, fn func(key, org, rk, tail string) error) error {
	var cursor uint64
	for {
		keys, next, err := m.C.Scan(ctx, cursor, "quota:*", 500).Result()
		if err != nil {
			return err
		}
		for _, k := range keys {
			org, rk, tail, ok := ClassifyV1CounterKey(k)
			if !ok {
				continue
			}
			head := tail
			for i := 0; i < len(tail); i++ {
				if tail[i] == ':' {
					head = tail[:i]
					break
				}
			}
			if !tails[head] {
				continue
			}
			if err := fn(k, org, rk, tail); err != nil {
				return err
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}

func (m *Migrator) v2Twin(org, rk, tail string) string {
	return m.ks().prefixV(org, rk, 2) + ":" + tail
}

// Backfill seeds every v1 VALUE key's v2 twin (seed-if-absent — fully
// resumable; the hot path or a prior run having seeded it is a no-op).
// budget bounds seeds per pass (0 = unbounded).
func (m *Migrator) Backfill(ctx context.Context, budget int) (scanned, seeded int, err error) {
	marker, err := ReadMarker(ctx, m.C, m.Service)
	if err != nil {
		return 0, 0, err
	}
	if marker == nil || (marker.Phase != "dual" && marker.Phase != "flipped") {
		return 0, 0, &MigrationError{Reason: "backfill requires dual-on first (marker phase=dual) — " +
			"seeding without dual-write live would immediately drift"}
	}
	err = m.scanV1(ctx, backfillTails, func(key, org, rk, tail string) error {
		scanned++
		res, serr := migrationSeedScript.Run(ctx, m.C, []string{key, m.v2Twin(org, rk, tail)}).Result()
		if serr != nil {
			return serr
		}
		if n, ok := res.(int64); ok {
			seeded += int(n)
		}
		if budget > 0 && seeded >= budget {
			return errBudget
		}
		return nil
	})
	if err == errBudget {
		err = nil
	}
	return scanned, seeded, err
}

var errBudget = fmt.Errorf("budget reached")

// VerifyResult reports a value-compare of every v1 VALUE key vs its v2 twin.
type VerifyResult struct {
	OK        bool
	Compared  int
	Divergent []string
}

// Verify value-compares every v1 VALUE key against its v2 twin. Divergence
// keeps the flip/reap gates closed — it catches a broken dual-write.
func (m *Migrator) Verify(ctx context.Context, tolerance float64) (VerifyResult, error) {
	if tolerance <= 0 {
		tolerance = migrationEps
	}
	out := VerifyResult{OK: true}
	err := m.scanV1(ctx, backfillTails, func(key, org, rk, tail string) error {
		out.Compared++
		v1s, e1 := m.C.Get(ctx, key).Result()
		if e1 == redis.Nil {
			v1s = ""
		} else if e1 != nil {
			return e1
		}
		v2s, e2 := m.C.Get(ctx, m.v2Twin(org, rk, tail)).Result()
		missing := e2 == redis.Nil
		if e2 != nil && !missing {
			return e2
		}
		f1, _ := strconv.ParseFloat(v1s, 64)
		f2, _ := strconv.ParseFloat(v2s, 64)
		if missing || math.Abs(f1-f2) > tolerance {
			out.OK = false
			out.Divergent = append(out.Divergent, key)
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	m.verifiedOK = out.OK
	return out, nil
}

// FlipGate returns "" when the flip may proceed, else the refusal reason —
// machine-enforced (§3.3): dual_since must outwait the idem TTL AND the
// longest rate window, and verify must have passed in THIS run.
func (m *Migrator) FlipGate(marker *Marker) string {
	if marker == nil || (marker.Phase != "dual" && marker.Phase != "flipped") {
		return "not in dual phase — run dual-on and backfill first"
	}
	wait := float64(IdemTTLSeconds)
	if w := float64(m.MaxRateWindowSeconds); w > wait {
		wait = w
	}
	age := m.now() - marker.DualSince
	if marker.DualSince == 0 {
		age = 0
	}
	if age < wait {
		return fmt.Sprintf("dual window too young: %.0fs < %.0fs — a retry of a pre-dual op "+
			"(or a rate window) is not yet represented in v2; flipping now double-charges "+
			"on retry (spec §6.2)", age, wait)
	}
	if !m.verifiedOK {
		return "verify has not passed in this run — run verify first"
	}
	return ""
}

// Flip records readiness for the (2,true) config flip; the config change +
// rolling restart is the operator's. Refuses until the gate opens.
func (m *Migrator) Flip(ctx context.Context) (*Marker, error) {
	marker, err := ReadMarker(ctx, m.C, m.Service)
	if err != nil {
		return nil, err
	}
	if reason := m.FlipGate(marker); reason != "" {
		return nil, &MigrationError{Reason: "flip refused: " + reason}
	}
	return m.writeMarker(ctx, func(mk *Marker) { mk.Phase, mk.HighWater = "flipped", "flipped" })
}

// Reap is THE one irreversible step. Guards (§3.3): verify green in THIS
// run; marker set to v2-final BEFORE the first delete; explicit operator
// confirmation that no other service scope still reads the unscoped v1 keys.
func (m *Migrator) Reap(ctx context.Context, iConfirmNoOtherScopeReadsV1 bool) (deleted int, err error) {
	marker, err := ReadMarker(ctx, m.C, m.Service)
	if err != nil {
		return 0, err
	}
	if marker == nil || (marker.Phase != "flipped" && marker.Phase != "reaped") {
		return 0, &MigrationError{Reason: "reap refused: flip has not been recorded"}
	}
	if !m.verifiedOK {
		return 0, &MigrationError{Reason: "reap refused: verify has not passed in this run " +
			"(reap deletes state; it never runs on a stale verdict)"}
	}
	if !iConfirmNoOtherScopeReadsV1 {
		return 0, &MigrationError{Reason: "reap refused: v1 counter keys carry no service scope — " +
			"confirm no other service scope still reads them (iConfirmNoOtherScopeReadsV1)"}
	}
	if _, err := m.writeMarker(ctx, func(mk *Marker) { mk.HighWater, mk.Phase = "v2-final", "reaped" }); err != nil {
		return 0, err
	}
	err = m.scanV1(ctx, reapTails, func(key, _, _, _ string) error {
		n, derr := m.C.Del(ctx, key).Result()
		deleted += int(n)
		return derr
	})
	return deleted, err
}

// Status reports the marker + a keyspace census (the straggler guard's data
// source): v1 counter keys still present are NAMED, not assumed absent.
func (m *Migrator) Status(ctx context.Context) (marker *Marker, v1Keys, v2Keys int, err error) {
	var cursor uint64
	for {
		keys, next, serr := m.C.Scan(ctx, cursor, "quota:*", 500).Result()
		if serr != nil {
			return nil, 0, 0, serr
		}
		for _, k := range keys {
			if len(k) >= 9 && k[:9] == "quota:v2:" {
				v2Keys++
			} else if _, _, _, ok := ClassifyV1CounterKey(k); ok {
				v1Keys++
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	marker, err = ReadMarker(ctx, m.C, m.Service)
	return marker, v1Keys, v2Keys, err
}

// CheckV1Stragglers is the post-flip straggler guard (spec §11.1, top risk):
// after the flip, any v1 counter key still present means a pre-mechanism
// writer is producing keys nobody reads — silent spend loss. LOUD.
func CheckV1Stragglers(ctx context.Context, c redis.Cmdable, service string, alert func(count int, sample []string)) (int, error) {
	marker, err := ReadMarker(ctx, c, service)
	if err != nil {
		return 0, err
	}
	postFlip := marker != nil && (marker.Phase == "flipped" || marker.Phase == "reaped")
	var stragglers []string
	var cursor uint64
	for len(stragglers) < 50 {
		keys, next, serr := c.Scan(ctx, cursor, "quota:*", 500).Result()
		if serr != nil {
			return 0, serr
		}
		for _, k := range keys {
			if _, _, _, ok := ClassifyV1CounterKey(k); ok {
				stragglers = append(stragglers, k)
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	if postFlip && len(stragglers) > 0 {
		sample := stragglers
		if len(sample) > 5 {
			sample = sample[:5]
		}
		slog.Error("keyspace_v1_straggler_writes — a writer is still producing v1 counter keys "+
			"AFTER the flip; its spend lands where nobody reads (spec §11.1). Find the "+
			"straggler before reap.", "service", service, "count", len(stragglers), "sample", sample)
		if alert != nil {
			alert(len(stragglers), sample)
		}
	}
	return len(stragglers), nil
}

// CheckBootKeyspace — boot refusals (spec §3.3), the Go twin of Python's
// check_boot_keyspace: QUOTA-CFG-011 (version regression against a completed
// migration) and QUOTA-CFG-012 (brownfield v2 over live v1 keys). Call at
// Setup after Redis is reachable. Returns the marker for capability display.
func CheckBootKeyspace(ctx context.Context, c redis.Cmdable, ks Keyspace) (*Marker, error) {
	if ks.Service == "" {
		return nil, nil // v1-only consumer with no scope — nothing recorded to regress
	}
	marker, err := ReadMarker(ctx, c, ks.Service)
	if err != nil {
		return nil, err
	}
	final := marker != nil && marker.HighWater == "v2-final"
	if final && (ks.version() == 1 || ks.DualWrite) {
		return nil, &KeyspaceConfigError{Code: "QUOTA-CFG-011", Detail: fmt.Sprintf(
			"keyspace version regression: config declares v%d (dual=%v) but storage records a "+
				"COMPLETED v2 migration (marker v2-final) — a v1 engine would read orphaned keys "+
				"and every counter would read zero. Declare keyspace_version: 2, "+
				"keyspace_dual_write: false. Not operator-overridable (definitive negative).",
			ks.version(), ks.DualWrite)}
	}
	if ks.version() == 2 && !ks.DualWrite && !final {
		var found string
		var cursor uint64
		for found == "" {
			keys, next, serr := c.Scan(ctx, cursor, "quota:*", 200).Result()
			if serr != nil {
				return nil, serr
			}
			for _, k := range keys {
				if _, _, _, ok := ClassifyV1CounterKey(k); ok {
					found = k
					break
				}
			}
			if next == 0 {
				break
			}
			cursor = next
		}
		if found != "" {
			return nil, &KeyspaceConfigError{Code: "QUOTA-CFG-012", Detail: fmt.Sprintf(
				"v1 counter keys exist (e.g. %s) but no completed migration is recorded — run "+
					"the keyspace migration (dual-on → backfill → verify → flip → reap) or declare "+
					"keyspace_version: 1; booting v2-only now would silently orphan live counters.",
				found)}
		}
	}
	return marker, nil
}
