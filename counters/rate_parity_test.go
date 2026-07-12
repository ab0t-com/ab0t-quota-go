package counters

// Claim 3 (finding QG-06 remainder) — rate ZSET score-unit parity. Python
// scores rate entries with epoch SECONDS (time.time(), rate.py:26-46) and
// prunes with a seconds cutoff. The Go redis rate store scored epoch
// NANOSECONDS. On a SHARED quota:{org}:{rk}:rate key (which P5.3 key parity
// now makes reachable) the units disagree by 1e9:
//   - a Go ns-scored entry (~1.7e18) is always above a Python seconds
//     cutoff (~1.7e9) → Python never prunes it → over-counts → false denials;
//   - a Python seconds-scored entry is always below a Go ns cutoff → Go
//     prunes it immediately → under-counts → limit bypass.
// So a shared rate key is unusable cross-runtime until scores align.
//
// RED before the fix: the stored score is nanoseconds, not seconds.

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisRate_ScoreIsEpochSeconds_QG06(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })

	rs, err := NewRedisRateStore(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t0 := time.Unix(1_700_000_000, 500_000_000) // .5s into the second
	key := "quota:org-1:api.calls:rate"
	if err := rs.Record(ctx, key, t0, time.Minute, "m1"); err != nil {
		t.Fatal(err)
	}

	zs, err := c.ZRangeWithScores(ctx, key, 0, -1).Result()
	if err != nil || len(zs) != 1 {
		t.Fatalf("zrange: %v (n=%d)", err, len(zs))
	}
	score := zs[0].Score
	wantSec := float64(t0.UnixNano()) / 1e9 // Python's time.time()-equivalent

	// Python parity: score must be epoch SECONDS (~1.7e9), not ns (~1.7e18).
	if score > 1e12 {
		t.Errorf("QG-06: rate score is %.0f — nanoseconds, not Python epoch-seconds (~%.3f)", score, wantSec)
	}
	if diff := score - wantSec; diff > 0.001 || diff < -0.001 {
		t.Errorf("QG-06: rate score %.6f != Python epoch-seconds %.6f", score, wantSec)
	}
}

// Cross-runtime prune: an entry written with a Python-style seconds score
// must be correctly trimmed by the Go store when it falls outside the
// window (proves the cutoff is also in seconds).
func TestRedisRate_PrunesPythonScoredEntries_QG06(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	rs, _ := NewRedisRateStore(c)
	ctx := context.Background()
	key := "quota:org-1:api.calls:rate"

	base := time.Unix(1_700_000_000, 0)
	now := base.Add(100 * time.Second)
	// PYTHON writer, seconds scores: one STALE (100s ago, outside 60s
	// window) and one RECENT (10s ago, inside the window).
	if err := c.ZAdd(ctx, key,
		redis.Z{Score: float64(base.Unix()), Member: "py-stale"},                        // 100s ago
		redis.Z{Score: float64(now.Add(-10 * time.Second).Unix()), Member: "py-recent"}, // 10s ago
	).Err(); err != nil {
		t.Fatal(err)
	}
	// Go counts with a 60s window: stale pruned, recent kept → exactly 1.
	// Under the ns-cutoff bug BOTH seconds-scored entries fall below the ns
	// cutoff and are wrongly pruned → count 0.
	n, err := rs.Count(ctx, key, now, 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("QG-06: cross-runtime count wrong (got %d, want 1) — cutoff not in Python seconds; a recent Python entry was mis-pruned", n)
	}
}
