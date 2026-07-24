package counters

// TASK P5.2 — the atomic bundle check-and-spend primitive behind
// engine.Acquire. Ticket 20260709_ab0t_quota_systemic_integrity_redesign.
//
// Checks ALL gauge limits (org + per-user) and, only if EVERY one passes,
// spends ALL of them — in ONE atomic step. This is the real cross-resource
// atomicity that kills QI-03's TOCTOU (two racers at limit-1 cannot both be
// admitted) and the fake-atomic sequential "batch" loop.
//
// Backed by Lua on Redis (all keys declared in KEYS — QI-09) and by the
// store mutex in memory. Exposed as an OPTIONAL interface so FloatStore
// stays lean; both built-in stores implement it.

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// AcquireSpec is one gauge in an atomic bundle acquire.
type AcquireSpec struct {
	OrgKey    string
	UserKey   string
	SeqKey    string
	HasUser   bool
	Delta     float64
	OrgLimit  *float64 // nil = unlimited
	UserLimit *float64 // nil = unlimited (or no per-user limit)
	// Secondary-shape twins (K-8) — set only during keyspace dual-write and
	// consumed only by DualAtomicAcquire; zero values change nothing.
	OrgKey2  string
	UserKey2 string
	SeqKey2  string
}

// AcquireOutcome is the result of an atomic acquire.
type AcquireOutcome struct {
	Admitted    bool
	Dup         bool // idempotency key already claimed → admitted, NOT re-spent
	DeniedIndex int  // 1-based index of the first gauge that would exceed; 0 if admitted
}

// GaugeAcquirer is implemented by stores that can run the atomic bundle
// acquire. The engine type-asserts FloatStore to this; both InMemoryStore
// and the redis FloatStore implement it.
type GaugeAcquirer interface {
	AtomicAcquire(ctx context.Context, idemKey string, hasIdem bool, idemTTL time.Duration, specs []AcquireSpec) (AcquireOutcome, error)
}

func fmtLimit(l *float64) string {
	if l == nil {
		return "" // '' = unlimited; the Lua skips the check
	}
	return strconv.FormatFloat(*l, 'f', -1, 64)
}

// validateAcquireSpecs guards the D-31 fail direction at the LIBRARY BOUNDARY,
// before any Lua runs (W-T3 defects CT-01/CT-02/CT-03, ticket 20260709):
//
//   - a NaN limit makes every comparison false and ADMITS everything — a
//     corrupted limit silently widened to infinity (real Lua tonumber('NaN')
//     is NaN; NOTE miniredis's gopher-lua returns nil and errors instead, so
//     only client-side validation behaves identically on both);
//   - a negative delta passes every limit check trivially and then DECREMENTS
//     the gauge — an "acquire" that erases spend and can breach the QG-06
//     zero floor;
//   - a non-finite delta passes the checks, CLAIMS the idempotency key, then
//     errors on INCRBYFLOAT mid-script; Redis scripts do not roll back, so
//     the claim is burned (the corrected retry is swallowed as a dup) and a
//     multi-gauge bundle is left PARTIALLY spent.
//
// Python's boundary applies the same rule (counters/base.py finite_magnitude
// + gauge._fmt_limit) — cross-runtime agreement is at this seam.
func validateAcquireSpecs(specs []AcquireSpec) error {
	for i, sp := range specs {
		if math.IsNaN(sp.Delta) || math.IsInf(sp.Delta, 0) {
			return fmt.Errorf("counters: AtomicAcquire spec %d: delta must be finite, got %v — "+
				"refusing before Lua so the idempotency claim is not burned (D-31)", i+1, sp.Delta)
		}
		if sp.Delta < 0 {
			return fmt.Errorf("counters: AtomicAcquire spec %d: delta must be non-negative, got %v — "+
				"an acquire that decrements erases spend (D-31)", i+1, sp.Delta)
		}
		if sp.OrgLimit != nil && math.IsNaN(*sp.OrgLimit) {
			return fmt.Errorf("counters: AtomicAcquire spec %d: org limit is NaN — a NaN limit "+
				"admits everything (D-31 forbids silently widening a limit)", i+1)
		}
		if sp.HasUser && sp.UserLimit != nil && math.IsNaN(*sp.UserLimit) {
			return fmt.Errorf("counters: AtomicAcquire spec %d: user limit is NaN — a NaN limit "+
				"admits everything (D-31 forbids silently widening a limit)", i+1)
		}
	}
	return nil
}

// --- in-memory (atomic under the store mutex) ---

func (s *InMemoryStore) AtomicAcquire(_ context.Context, idemKey string, hasIdem bool, idemTTL time.Duration, specs []AcquireSpec) (AcquireOutcome, error) {
	if err := validateAcquireSpecs(specs); err != nil {
		return AcquireOutcome{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	if hasIdem {
		if _, exists := s.strings[idemKey]; exists {
			return AcquireOutcome{Admitted: true, Dup: true}, nil
		}
	}
	// Check ALL.
	for i, sp := range specs {
		if sp.OrgLimit != nil && s.floats[sp.OrgKey]+sp.Delta > *sp.OrgLimit {
			return AcquireOutcome{Admitted: false, DeniedIndex: i + 1}, nil
		}
		if sp.HasUser && sp.UserLimit != nil && s.floats[sp.UserKey]+sp.Delta > *sp.UserLimit {
			return AcquireOutcome{Admitted: false, DeniedIndex: i + 1}, nil
		}
	}
	// Claim idem, then spend ALL.
	if hasIdem {
		s.strings[idemKey] = "1"
		if idemTTL > 0 {
			s.expiry[idemKey] = s.now().Add(idemTTL)
		}
	}
	for _, sp := range specs {
		s.floats[sp.OrgKey] += sp.Delta
		if sp.HasUser {
			s.floats[sp.SeqKey] += 1
			s.floats[sp.UserKey] += sp.Delta
		}
	}
	return AcquireOutcome{Admitted: true}, nil
}

// --- redis (atomic via Lua; all keys in KEYS — QI-09) ---
//
// Byte-for-byte the same script Python runs (engine.py _ACQUIRE), so a mixed
// Python/Go fleet sharing one keyspace spends identically.
//
//	KEYS[1]=idem, then per gauge i (kb=1+(i-1)*3): org, user, seq
//	ARGV[1]=has_idem ARGV[2]=idem_ttl ARGV[3]=n
//	per gauge i (ab=3+(i-1)*4): has_user, delta, org_limit, user_limit
//
// Returns {admitted('1'/'0'), reason} reason='ok'|'dup'|1-based denied index.
const acquireSrc = `
local n = tonumber(ARGV[3])
if ARGV[1] == '1' then
  if redis.call('GET', KEYS[1]) then
    return {'1', 'dup'}
  end
end
for i=1,n do
  local kb = 1 + (i-1)*3
  local ab = 3 + (i-1)*4
  local has_user = ARGV[ab+1]
  local delta = tonumber(ARGV[ab+2])
  local org_limit = ARGV[ab+3]
  local user_limit = ARGV[ab+4]
  local ocur = redis.call('GET', KEYS[kb+1]); if not ocur then ocur = '0' end
  if org_limit ~= '' and (tonumber(ocur) + delta) > tonumber(org_limit) then
    return {'0', tostring(i)}
  end
  if has_user == '1' and user_limit ~= '' then
    local ucur = redis.call('GET', KEYS[kb+2]); if not ucur then ucur = '0' end
    if (tonumber(ucur) + delta) > tonumber(user_limit) then
      return {'0', tostring(i)}
    end
  end
end
if ARGV[1] == '1' then
  if not redis.call('SET', KEYS[1], '1', 'NX', 'EX', ARGV[2]) then
    return {'1', 'dup'}
  end
end
for i=1,n do
  local kb = 1 + (i-1)*3
  local ab = 3 + (i-1)*4
  local has_user = ARGV[ab+1]
  local delta = tonumber(ARGV[ab+2])
  redis.call('INCRBYFLOAT', KEYS[kb+1], delta)
  if has_user == '1' then
    redis.call('INCR', KEYS[kb+3])
    redis.call('INCRBYFLOAT', KEYS[kb+2], delta)
  end
end
return {'1', 'ok'}
`

var acquireScript = redis.NewScript(acquireSrc)

// AcquireSrc exposes the REAL acquire script for the boot-time scripting capability check
// (D-73). The check must load THIS source — a probe that loads `return 1` proves nothing
// about whether this Redis can run our Lua.
const AcquireSrc = acquireSrc

func (s *redisFloatStore) AtomicAcquire(ctx context.Context, idemKey string, hasIdem bool, idemTTL time.Duration, specs []AcquireSpec) (AcquireOutcome, error) {
	if err := validateAcquireSpecs(specs); err != nil {
		return AcquireOutcome{}, err
	}
	keys := make([]string, 0, 1+3*len(specs))
	keys = append(keys, idemKey)
	hasIdemStr := "0"
	if hasIdem {
		hasIdemStr = "1"
	}
	ttl := int(idemTTL.Seconds())
	if ttl <= 0 {
		ttl = 86400
	}
	argv := []any{hasIdemStr, strconv.Itoa(ttl), strconv.Itoa(len(specs))}
	for _, sp := range specs {
		keys = append(keys, sp.OrgKey)
		if sp.HasUser {
			keys = append(keys, sp.UserKey, sp.SeqKey)
		} else {
			keys = append(keys, sp.OrgKey, sp.OrgKey) // placeholders; has_user='0' skips them
		}
		hu := "0"
		if sp.HasUser {
			hu = "1"
		}
		argv = append(argv,
			hu,
			strconv.FormatFloat(sp.Delta, 'f', -1, 64),
			fmtLimit(sp.OrgLimit),
			fmtLimit(sp.UserLimit),
		)
	}
	res, err := acquireScript.Run(ctx, s.c, keys, argv...).Result()
	if err != nil {
		return AcquireOutcome{}, err
	}
	arr, ok := res.([]any)
	if !ok || len(arr) != 2 {
		return AcquireOutcome{}, nil
	}
	admitted := toStr(arr[0]) == "1"
	reason := toStr(arr[1])
	out := AcquireOutcome{Admitted: admitted}
	switch reason {
	case "dup":
		out.Dup = true
	case "ok":
	default:
		if idx, e := strconv.Atoi(reason); e == nil {
			out.DeniedIndex = idx
		}
	}
	return out, nil
}

func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return ""
}
