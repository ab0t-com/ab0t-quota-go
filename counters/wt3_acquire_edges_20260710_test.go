package counters

// W-T3 (ticket 20260709_ab0t_quota_systemic_integrity_redesign) — numeric
// edges on the exported AtomicAcquire primitive, BOTH backends (in-memory
// and Redis/Lua). Governing rule D-31: a weird input may never silently
// widen a limit or erase a spend.
//
// Defects these tests were RED against (pre-fix):
//   CT-01  a NaN OrgLimit/UserLimit made every comparison false and ADMITTED
//          everything — a corrupted limit silently widened to infinity
//          (in-memory: `v+delta > NaN` is false; Lua: `tonumber('NaN')`
//          comparisons are false). Python's boundary now raises ValueError.
//   CT-02  a NEGATIVE Delta passed every limit check trivially and then
//          INCRBYFLOAT'd the gauge DOWN — an "acquire" that ERASES spend and
//          can drive the gauge below the QG-06 zero floor (observed -4 via
//          the Python twin of this script pre-fix).
//   CT-03  a NaN Delta passed the checks (comparisons false), CLAIMED the
//          idempotency key, then errored on INCRBYFLOAT mid-script; Redis
//          scripts do not roll back, so the claim persisted and — in a
//          multi-gauge bundle — earlier gauges stayed PARTIALLY spent. The
//          corrected retry is then swallowed as a dup: spend erased.
//
// Emulator caveat: the Redis branch runs on miniredis (gopher-lua), never a
// real Redis EVAL (standing pre-deploy gate A1).

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func wt3Backends(t *testing.T) map[string]FloatStore {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	rs, err := NewRedisStore(client)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]FloatStore{
		"memory": NewInMemoryStore(),
		"redis":  rs,
	}
}

func ptr(f float64) *float64 { return &f }

// CT-01 — NaN limit must error (or deny), never admit.
func TestAtomicAcquire_NaNLimit_MustNotWiden_WT3(t *testing.T) {
	for name, fs := range wt3Backends(t) {
		t.Run(name, func(t *testing.T) {
			acq := fs.(GaugeAcquirer)
			ctx := context.Background()
			_ = fs.Set(ctx, "quota:o1:sandboxes:gauge", 10, 0)
			out, err := acq.AtomicAcquire(ctx, "quota:o1:sandboxes:idem:__unused__", false, time.Hour,
				[]AcquireSpec{{OrgKey: "quota:o1:sandboxes:gauge", Delta: 1, OrgLimit: ptr(math.NaN())}})
			if err == nil && out.Admitted {
				t.Fatalf("CT-01 (D-31): a NaN limit ADMITTED (out=%+v) — a corrupted "+
					"limit silently widened to infinity; must error or deny", out)
			}
			v, _, _ := fs.GetFloat(ctx, "quota:o1:sandboxes:gauge")
			if v != 10 {
				t.Errorf("gauge mutated to %v under a NaN limit (want 10 untouched)", v)
			}
		})
	}
}

// CT-02 — a negative Delta must error, never decrement ("acquire" ≠ release).
func TestAtomicAcquire_NegativeDelta_Rejected_WT3(t *testing.T) {
	for name, fs := range wt3Backends(t) {
		t.Run(name, func(t *testing.T) {
			acq := fs.(GaugeAcquirer)
			ctx := context.Background()
			_ = fs.Set(ctx, "quota:o1:sandboxes:gauge", 5, 0)
			out, err := acq.AtomicAcquire(ctx, "quota:o1:sandboxes:idem:__unused__", false, time.Hour,
				[]AcquireSpec{{OrgKey: "quota:o1:sandboxes:gauge", Delta: -5, OrgLimit: ptr(10.0)}})
			v, _, _ := fs.GetFloat(ctx, "quota:o1:sandboxes:gauge")
			if err == nil && out.Admitted && v < 5 {
				t.Fatalf("CT-02 (D-31): AtomicAcquire(Delta=-5) admitted and drove the "+
					"gauge from 5 to %v — an admission op ERASED spend", v)
			}
			if err == nil {
				t.Fatalf("CT-02: negative delta must be rejected with an error (got out=%+v, gauge=%v)", out, v)
			}
			if v != 5 {
				t.Errorf("gauge mutated to %v by a rejected delta (want 5)", v)
			}
		})
	}
}

// CT-03 — a non-finite Delta must be rejected BEFORE the idem claim / any
// partial spend. Uses a 2-gauge bundle: gauge A finite, gauge B NaN.
func TestAtomicAcquire_NonFiniteDelta_NoBurnNoPartialSpend_WT3(t *testing.T) {
	for name, fs := range wt3Backends(t) {
		t.Run(name, func(t *testing.T) {
			acq := fs.(GaugeAcquirer)
			ctx := context.Background()
			idem := "quota:o1:sandboxes:idem:acq-1"
			specs := []AcquireSpec{
				{OrgKey: "quota:o1:a:gauge", Delta: 1, OrgLimit: ptr(10.0)},
				{OrgKey: "quota:o1:b:gauge", Delta: math.NaN(), OrgLimit: ptr(10.0)},
			}
			_, err := acq.AtomicAcquire(ctx, idem, true, time.Hour, specs)
			if err == nil {
				t.Fatalf("CT-03: NaN delta in a bundle did not error")
			}
			va, _, _ := fs.GetFloat(ctx, "quota:o1:a:gauge")
			if va != 0 {
				t.Errorf("CT-03: PARTIAL SPEND — gauge A at %v after a rejected bundle (want 0)", va)
			}
			// The idem claim must not be burned: a corrected retry must apply.
			out, err := acq.AtomicAcquire(ctx, idem, true, time.Hour,
				[]AcquireSpec{{OrgKey: "quota:o1:a:gauge", Delta: 1, OrgLimit: ptr(10.0)}})
			if err != nil || !out.Admitted || out.Dup {
				t.Fatalf("CT-03: corrected retry after a rejected bundle was swallowed "+
					"(out=%+v err=%v) — the idempotency key was BURNED; the spend is erased", out, err)
			}
			va, _, _ = fs.GetFloat(ctx, "quota:o1:a:gauge")
			if va != 1 {
				t.Errorf("corrected retry did not land: gauge A = %v (want 1)", va)
			}
		})
	}
}

// CT-04 — the exported Accumulator.Add/AddOrg must apply magnitude semantics
// (Python parity: AccumulatorCounter.increment(-4) on 10 ⇒ 14) and reject
// non-finite deltas. Pre-fix, Add(-4) ERASED period spend one layer below the
// engine (engine.Spend was fixed; the exported counter surface was not).
func TestAccumulatorAdd_NegativeAndNonFinite_WT3(t *testing.T) {
	for name, fs := range wt3Backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			a := Accumulator{Store: fs, Prefix: "quota", ResourceKey: "spend"}
			key := a.OrgPeriodKey("o1", time.Unix(1_700_000_000, 0).UTC())
			_ = fs.Set(ctx, key, 10, 0)
			v, err := a.AddOrg(ctx, "o1", time.Unix(1_700_000_000, 0).UTC(), -4)
			if err != nil {
				t.Fatalf("AddOrg(-4): %v", err)
			}
			if v != 14 {
				t.Fatalf("CT-04 (D-31): AddOrg(-4) on 10 yielded %v — Python reference "+
					"is 14 (magnitude); a sign flip must never erase period spend", v)
			}
			if _, err := a.AddOrg(ctx, "o1", time.Unix(1_700_000_000, 0).UTC(), math.NaN()); err == nil {
				t.Errorf("CT-04: AddOrg(NaN) did not error")
			}
		})
	}
}
