package counters

// Claim 3 / QI-09 — every key a Lua script touches MUST be declared in KEYS.
// Python's _DECR_USER computed a claim key inside the script (KEYS[1] ..
// ':gen:' .. gen) that was never declared — undefined on standalone Redis,
// an outright error on cluster; two reviews missed it. This test statically
// guards the Go Lua scripts against that class: it parses every redis.call
// that accesses a key and asserts the key argument is a KEYS[...] reference,
// never a computed/concatenated string.
//
// Covers this package's scripts (decrFloorSrc, acquireSrc). The activations
// package guards transitionSrc with the same helper.

import (
	"regexp"
	"strings"
	"testing"
)

// keyCommands are redis commands whose FIRST argument is a key.
var keyCommands = map[string]bool{
	"GET": true, "SET": true, "SETNX": true, "INCR": true, "INCRBYFLOAT": true,
	"DECR": true, "EXPIRE": true, "DEL": true, "SADD": true, "SREM": true,
	"SMEMBERS": true, "SCARD": true, "ZADD": true, "ZCARD": true,
	"ZREMRANGEBYSCORE": true, "PERSIST": true, "EXISTS": true,
}

var reRedisCall = regexp.MustCompile(`redis\.call\(\s*'([A-Z]+)'\s*,\s*([^,\)]+)`)

// luaKeyViolations returns the QI-09 violations in src (empty ⇒ clean).
func luaKeyViolations(src string) []string {
	var v []string
	// The QI-09 smell in one line: a key built by concatenation.
	if strings.Contains(src, "] ..") || regexp.MustCompile(`'[^']*:'\s*\.\.`).MatchString(src) {
		v = append(v, "script concatenates to build a key (QI-09 smell)")
	}
	for _, m := range reRedisCall.FindAllStringSubmatch(src, -1) {
		cmd, arg := m[1], strings.TrimSpace(m[2])
		if !keyCommands[cmd] {
			continue
		}
		if !strings.HasPrefix(arg, "KEYS[") {
			v = append(v, "redis.call('"+cmd+"', "+arg+" ...) accesses an undeclared key")
		}
	}
	return v
}

// AuditLuaKeysDeclared asserts every key-accessing redis.call in src uses a
// KEYS[...] argument. Exported so sibling packages (activations) reuse it.
func AuditLuaKeysDeclared(t *testing.T, name, src string) {
	t.Helper()
	for _, viol := range luaKeyViolations(src) {
		t.Errorf("%s: %s (QI-09 — every key must arrive via KEYS[])", name, viol)
	}
}

func TestLuaKeysAllDeclared_QI09(t *testing.T) {
	AuditLuaKeysDeclared(t, "decrFloorSrc", decrFloorSrc)
	AuditLuaKeysDeclared(t, "acquireSrc", acquireSrc)
}

// Negative control — the auditor MUST catch a Python-_DECR_USER-style
// computed key. If this passes clean, the auditor is vacuous.
func TestQI09Audit_HasTeeth_NegativeControl(t *testing.T) {
	badComputed := `
local gen = redis.call('GET', KEYS[4])
redis.call('SET', KEYS[1] .. ':gen:' .. gen, '1')
`
	if len(luaKeyViolations(badComputed)) == 0 {
		t.Error("QI-09 auditor did not flag a computed (concatenated) key — it is VACUOUS")
	}
	badUndeclared := `redis.call('INCRBYFLOAT', somevar, 1)`
	if len(luaKeyViolations(badUndeclared)) == 0 {
		t.Error("QI-09 auditor did not flag a non-KEYS[] key argument — it is VACUOUS")
	}
	// A clean script must produce NO violations (guards against false positives).
	if v := luaKeyViolations(`redis.call('GET', KEYS[1]); redis.call('SET', KEYS[2], '0')`); len(v) != 0 {
		t.Errorf("clean script wrongly flagged: %v", v)
	}
}
