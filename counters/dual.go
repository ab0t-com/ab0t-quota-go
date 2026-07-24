package counters

// K-8 (keyspace spec §3.2/§6) — dual-keyspace mutation ops. During a
// migration window BOTH shapes are maintained atomically: seed-if-absent
// (v2 twin initialised from the LIVE v1 level inside the same script),
// mutate-both, idem latches checked AND claimed on both shapes.
//
// The Lua helper block is the Python law (ab0t_quota/counters/base.py
// _HELPERS) ported byte-for-byte — ONE home for the dual-write law per
// runtime, so drift here is the double-charge bug K-3 exists to prevent.
// Convention: KEYS[1..NK] = primary shape, KEYS[NK+1..2NK] = secondary;
// DUAL/V2P ride ARGV. seedv2 always seeds the V2 side from the V1 side.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const dualHelpers = `
local function seedv2(i)
  if not DUAL then return end
  if V2P then
    if redis.call('EXISTS', KEYS[i]) == 0 then
      local v = redis.call('GET', KEYS[NK+i])
      if v then redis.call('SET', KEYS[i], v) end
    end
  else
    if redis.call('EXISTS', KEYS[NK+i]) == 0 then
      local v = redis.call('GET', KEYS[i])
      if v then redis.call('SET', KEYS[NK+i], v) end
    end
  end
end
local function idem_dup(i)
  if redis.call('GET', KEYS[i]) then return true end
  if DUAL and redis.call('GET', KEYS[NK+i]) then return true end
  return false
end
local function idem_claim(i)
  redis.call('SET', KEYS[i], '1', 'NX', 'EX', ARGV[2])
  if DUAL then redis.call('SET', KEYS[NK+i], '1', 'NX', 'EX', ARGV[2]) end
end
local function incrboth(i, d)
  local v = redis.call('INCRBYFLOAT', KEYS[i], d)
  if DUAL then redis.call('INCRBYFLOAT', KEYS[NK+i], d) end
  return v
end
local function incrfloorboth(i, d)
  local v = redis.call('INCRBYFLOAT', KEYS[i], d)
  if tonumber(v) < 0 then redis.call('SET', KEYS[i], '0'); v = '0' end
  if DUAL then
    local w = redis.call('INCRBYFLOAT', KEYS[NK+i], d)
    if tonumber(w) < 0 then redis.call('SET', KEYS[NK+i], '0') end
  end
  return v
end
local function incrintboth(i)
  redis.call('INCR', KEYS[i])
  if DUAL then redis.call('INCR', KEYS[NK+i]) end
end
local function expboth(i, t)
  redis.call('EXPIRE', KEYS[i], t)
  if DUAL then redis.call('EXPIRE', KEYS[NK+i], t) end
end
`

// DualLua composes a dual-capable script (Python counters/base.py dual_lua,
// same composition): NK may be a Lua expression; ARGV[dualArgv]=dual flag,
// ARGV[dualArgv+1]=primary-is-v2.
func DualLua(nk string, dualArgv int, body string) string {
	return "local NK = " + nk + "\n" +
		"local DUAL = ARGV[" + strconv.Itoa(dualArgv) + "] == '1'\n" +
		"local V2P = ARGV[" + strconv.Itoa(dualArgv+1) + "] == '1'\n" +
		dualHelpers + body
}

// Spend scripts. idem_claim's ARGV[2] slot is unused here (Go's legacy
// Spend never claims); the slot is kept so the helper block stays verbatim.
//
// org-only: KEYS[1]=org; ARGV[1]=delta [2]=dual [3]=v2p
var dualSpendOrgScript = redis.NewScript(DualLua("1", 2, `
seedv2(1)
return incrboth(1, ARGV[1])
`))

// per-user: KEYS[1]=org [2]=user [3]=seq; ARGV[1]=delta [2]=dual [3]=v2p.
// The seq key is SEEDED (its loss resurrects QI-05.1, spec §6.2) but not
// bumped — Go's legacy Spend never bumped it (pre-existing Go semantics).
var dualSpendUserScript = redis.NewScript(DualLua("3", 2, `
seedv2(1); seedv2(2); seedv2(3)
local o = incrboth(1, ARGV[1])
local u = incrboth(2, ARGV[1])
return {o, u}
`))

// decrement + floor at zero, both shapes: KEYS[1]=key; ARGV[1]=magnitude
// [2]=dual [3]=v2p
var dualDecrFloorScript = redis.NewScript(DualLua("1", 2, `
seedv2(1)
return incrfloorboth(1, '-'..ARGV[1])
`))

// accumulator add + period TTL: KEYS[1]=acc; ARGV[1]=delta [2]=period_ttl
// [3]=dual [4]=v2p
var dualAccAddScript = redis.NewScript(DualLua("1", 3, `
seedv2(1)
local v = incrboth(1, ARGV[1])
if tonumber(ARGV[2]) > 0 then expboth(1, ARGV[2]) end
return v
`))

// dualAcquireSrc — the Python engine._ACQUIRE body, composed identically
// (KEYS doubled, ARGV[4]=dual [5]=v2p, per-gauge base 5), so a mixed
// Python/Go fleet in dual runs the same admission semantics.
var dualAcquireScript = redis.NewScript(DualLua("1 + tonumber(ARGV[3])*3", 4, `
local n = tonumber(ARGV[3])
for i=1,n do
  local kb = 1 + (i-1)*3
  local ab = 5 + (i-1)*4
  seedv2(kb+1)
  if ARGV[ab+1] == '1' then seedv2(kb+2); seedv2(kb+3) end
end
if ARGV[1] == '1' then
  if idem_dup(1) then
    return {'1', 'dup'}
  end
end
for i=1,n do
  local kb = 1 + (i-1)*3
  local ab = 5 + (i-1)*4
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
  idem_claim(1)
end
for i=1,n do
  local kb = 1 + (i-1)*3
  local ab = 5 + (i-1)*4
  local has_user = ARGV[ab+1]
  local delta = tonumber(ARGV[ab+2])
  incrboth(kb+1, delta)
  if has_user == '1' then
    incrintboth(kb+3)
    incrboth(kb+2, delta)
  end
end
return {'1', 'ok'}
`))

// DualOps is implemented by stores that can maintain BOTH keyspace shapes
// atomically. Both built-in stores implement it; the engine REFUSES dual on
// a store that does not (a declared dual that silently single-writes is the
// exact defect class this programme is named after).
type DualOps interface {
	DualSpendOrg(ctx context.Context, org DualPair, delta float64, v2p bool) (float64, error)
	DualSpendUser(ctx context.Context, org, user, seq DualPair, delta float64, v2p bool) (orgV, userV float64, err error)
	DualDecrFloor(ctx context.Context, key DualPair, amount float64, v2p bool) (float64, error)
	DualAccAdd(ctx context.Context, acc DualPair, delta float64, ttl time.Duration, v2p bool) (float64, error)
	DualAtomicAcquire(ctx context.Context, idem DualPair, hasIdem bool, idemTTL time.Duration, v2p bool, specs []AcquireSpec) (AcquireOutcome, error)
}

// GetFloatDual is the dual-READ: authoritative shape first, fall back to the
// other on absence (keyspace spec §3.2). Works on any FloatStore.
func GetFloatDual(ctx context.Context, s FloatStore, k DualPair) (float64, bool, error) {
	v, ok, err := s.GetFloat(ctx, k.P)
	if err != nil || ok || k.S == "" {
		return v, ok, err
	}
	return s.GetFloat(ctx, k.S)
}

func dualFlags(k DualPair, v2p bool) (string, string) {
	d := "0"
	if k.S != "" {
		d = "1"
	}
	v := "0"
	if v2p {
		v = "1"
	}
	return d, v
}

func fstr(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// --- redis implementations (atomic Lua; every key declared in KEYS, QI-09) ---

func (s *redisFloatStore) DualSpendOrg(ctx context.Context, org DualPair, delta float64, v2p bool) (float64, error) {
	d, v := dualFlags(org, v2p)
	keys := []string{org.P}
	if org.S != "" {
		keys = append(keys, org.S)
	}
	res, err := dualSpendOrgScript.Run(ctx, s.c, keys, fstr(delta), d, v).Result()
	if err != nil {
		return 0, err
	}
	return parseLuaFloat(res)
}

func (s *redisFloatStore) DualSpendUser(ctx context.Context, org, user, seq DualPair, delta float64, v2p bool) (float64, float64, error) {
	d, v := dualFlags(org, v2p)
	keys := []string{org.P, user.P, seq.P}
	if org.S != "" {
		keys = append(keys, org.S, user.S, seq.S)
	}
	res, err := dualSpendUserScript.Run(ctx, s.c, keys, fstr(delta), d, v).Result()
	if err != nil {
		return 0, 0, err
	}
	arr, ok := res.([]any)
	if !ok || len(arr) != 2 {
		return 0, 0, fmt.Errorf("DualSpendUser: unexpected reply %T", res)
	}
	o, err := parseLuaFloat(arr[0])
	if err != nil {
		return 0, 0, err
	}
	u, err := parseLuaFloat(arr[1])
	return o, u, err
}

func (s *redisFloatStore) DualDecrFloor(ctx context.Context, key DualPair, amount float64, v2p bool) (float64, error) {
	d, v := dualFlags(key, v2p)
	keys := []string{key.P}
	if key.S != "" {
		keys = append(keys, key.S)
	}
	res, err := dualDecrFloorScript.Run(ctx, s.c, keys, fstr(amount), d, v).Result()
	if err != nil {
		return 0, err
	}
	return parseLuaFloat(res)
}

func (s *redisFloatStore) DualAccAdd(ctx context.Context, acc DualPair, delta float64, ttl time.Duration, v2p bool) (float64, error) {
	d, v := dualFlags(acc, v2p)
	keys := []string{acc.P}
	if acc.S != "" {
		keys = append(keys, acc.S)
	}
	res, err := dualAccAddScript.Run(ctx, s.c, keys,
		fstr(delta), strconv.Itoa(int(ttl.Seconds())), d, v).Result()
	if err != nil {
		return 0, err
	}
	return parseLuaFloat(res)
}

func (s *redisFloatStore) DualAtomicAcquire(ctx context.Context, idem DualPair, hasIdem bool, idemTTL time.Duration, v2p bool, specs []AcquireSpec) (AcquireOutcome, error) {
	if err := validateAcquireSpecs(specs); err != nil {
		return AcquireOutcome{}, err
	}
	dual := idem.S != ""
	n := 1 + 3*len(specs)
	if dual {
		n *= 2
	}
	keys := make([]string, 0, n)
	primary := func(sp AcquireSpec) []string {
		if sp.HasUser {
			return []string{sp.OrgKey, sp.UserKey, sp.SeqKey}
		}
		return []string{sp.OrgKey, sp.OrgKey, sp.OrgKey} // placeholders; has_user='0' skips
	}
	secondary := func(sp AcquireSpec) []string {
		if sp.HasUser {
			return []string{sp.OrgKey2, sp.UserKey2, sp.SeqKey2}
		}
		return []string{sp.OrgKey2, sp.OrgKey2, sp.OrgKey2}
	}
	keys = append(keys, idem.P)
	for _, sp := range specs {
		keys = append(keys, primary(sp)...)
	}
	if dual {
		keys = append(keys, idem.S)
		for _, sp := range specs {
			keys = append(keys, secondary(sp)...)
		}
	}
	hasIdemStr := "0"
	if hasIdem {
		hasIdemStr = "1"
	}
	ttl := int(idemTTL.Seconds())
	if ttl <= 0 {
		ttl = 86400
	}
	dstr := "0"
	if dual {
		dstr = "1"
	}
	vstr := "0"
	if v2p {
		vstr = "1"
	}
	argv := []any{hasIdemStr, strconv.Itoa(ttl), strconv.Itoa(len(specs)), dstr, vstr}
	for _, sp := range specs {
		hu := "0"
		if sp.HasUser {
			hu = "1"
		}
		argv = append(argv, hu, fstr(sp.Delta), fmtLimit(sp.OrgLimit), fmtLimit(sp.UserLimit))
	}
	res, err := dualAcquireScript.Run(ctx, s.c, keys, argv...).Result()
	if err != nil {
		return AcquireOutcome{}, err
	}
	return parseAcquireReply(res), nil
}

func parseLuaFloat(res any) (float64, error) {
	switch t := res.(type) {
	case string:
		return strconv.ParseFloat(t, 64)
	case []byte:
		return strconv.ParseFloat(string(t), 64)
	case int64:
		return float64(t), nil
	}
	return 0, fmt.Errorf("unexpected Lua numeric reply %T", res)
}

func parseAcquireReply(res any) AcquireOutcome {
	arr, ok := res.([]any)
	if !ok || len(arr) != 2 {
		return AcquireOutcome{}
	}
	out := AcquireOutcome{Admitted: toStr(arr[0]) == "1"}
	switch reason := toStr(arr[1]); reason {
	case "dup":
		out.Dup = true
	case "ok":
	default:
		if idx, e := strconv.Atoi(reason); e == nil {
			out.DeniedIndex = idx
		}
	}
	return out
}

// --- in-memory implementations (atomic under the store mutex) ---
// seedPair: initialise the V2-side key from the V1 side when absent — the
// same seed-if-absent law as the Lua (v2p says which side of the pair is v2).

func (s *InMemoryStore) seedFloatPair(k DualPair, v2p bool) {
	if k.S == "" {
		return
	}
	v2, v1 := k.P, k.S
	if !v2p {
		v2, v1 = k.S, k.P
	}
	if _, ok := s.floats[v2]; !ok {
		if v, ok2 := s.floats[v1]; ok2 {
			s.floats[v2] = v
		}
	}
}

func (s *InMemoryStore) DualSpendOrg(_ context.Context, org DualPair, delta float64, v2p bool) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	s.seedFloatPair(org, v2p)
	s.floats[org.P] += delta
	if org.S != "" {
		s.floats[org.S] += delta
	}
	return s.floats[org.P], nil
}

func (s *InMemoryStore) DualSpendUser(_ context.Context, org, user, seq DualPair, delta float64, v2p bool) (float64, float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	for _, k := range []DualPair{org, user, seq} {
		s.seedFloatPair(k, v2p)
	}
	s.floats[org.P] += delta
	s.floats[user.P] += delta
	if org.S != "" {
		s.floats[org.S] += delta
		s.floats[user.S] += delta
	}
	return s.floats[org.P], s.floats[user.P], nil
}

func (s *InMemoryStore) DualDecrFloor(_ context.Context, key DualPair, amount float64, v2p bool) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	s.seedFloatPair(key, v2p)
	dec := func(k string) float64 {
		v := s.floats[k] - amount
		if v < 0 {
			v = 0
		}
		s.floats[k] = v
		return v
	}
	v := dec(key.P)
	if key.S != "" {
		dec(key.S)
	}
	return v, nil
}

func (s *InMemoryStore) DualAccAdd(_ context.Context, acc DualPair, delta float64, ttl time.Duration, v2p bool) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	s.seedFloatPair(acc, v2p)
	s.floats[acc.P] += delta
	if ttl > 0 {
		s.expiry[acc.P] = s.now().Add(ttl)
	}
	if acc.S != "" {
		s.floats[acc.S] += delta
		if ttl > 0 {
			s.expiry[acc.S] = s.now().Add(ttl)
		}
	}
	return s.floats[acc.P], nil
}

func (s *InMemoryStore) DualAtomicAcquire(_ context.Context, idem DualPair, hasIdem bool, idemTTL time.Duration, v2p bool, specs []AcquireSpec) (AcquireOutcome, error) {
	if err := validateAcquireSpecs(specs); err != nil {
		return AcquireOutcome{}, err
	}
	dual := idem.S != ""
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	for _, sp := range specs {
		s.seedFloatPair(DualPair{P: sp.OrgKey, S: sp.OrgKey2}, v2p)
		if sp.HasUser {
			s.seedFloatPair(DualPair{P: sp.UserKey, S: sp.UserKey2}, v2p)
			s.seedFloatPair(DualPair{P: sp.SeqKey, S: sp.SeqKey2}, v2p)
		}
	}
	if hasIdem {
		if _, exists := s.strings[idem.P]; exists {
			return AcquireOutcome{Admitted: true, Dup: true}, nil
		}
		if dual {
			if _, exists := s.strings[idem.S]; exists {
				return AcquireOutcome{Admitted: true, Dup: true}, nil
			}
		}
	}
	for i, sp := range specs {
		if sp.OrgLimit != nil && s.floats[sp.OrgKey]+sp.Delta > *sp.OrgLimit {
			return AcquireOutcome{Admitted: false, DeniedIndex: i + 1}, nil
		}
		if sp.HasUser && sp.UserLimit != nil && s.floats[sp.UserKey]+sp.Delta > *sp.UserLimit {
			return AcquireOutcome{Admitted: false, DeniedIndex: i + 1}, nil
		}
	}
	if idemTTL <= 0 {
		idemTTL = 86400 * time.Second
	}
	if hasIdem {
		claim := func(k string) {
			s.strings[k] = "1"
			s.expiry[k] = s.now().Add(idemTTL)
		}
		claim(idem.P)
		if dual {
			claim(idem.S)
		}
	}
	for _, sp := range specs {
		apply := func(org, user, seq string) {
			s.floats[org] += sp.Delta
			if sp.HasUser {
				s.floats[seq] += 1
				s.floats[user] += sp.Delta
			}
		}
		apply(sp.OrgKey, sp.UserKey, sp.SeqKey)
		if dual {
			apply(sp.OrgKey2, sp.UserKey2, sp.SeqKey2)
		}
	}
	return AcquireOutcome{Admitted: true}, nil
}
