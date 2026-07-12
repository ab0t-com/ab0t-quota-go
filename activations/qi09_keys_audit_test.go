package activations

// Claim 3 / QI-09 — guard the activation _TRANSITION Lua: every key it
// touches must be declared in KEYS (no computed keys). Self-contained (the
// counters auditor lives in a _test.go and isn't importable cross-package).

import (
	"regexp"
	"strings"
	"testing"
)

var reCall = regexp.MustCompile(`redis\.call\(\s*'([A-Z]+)'\s*,\s*([^,\)]+)`)

var keyCmds = map[string]bool{
	"GET": true, "SET": true, "SETNX": true, "INCR": true, "INCRBYFLOAT": true,
	"EXPIRE": true, "DEL": true, "SADD": true, "SREM": true, "EXISTS": true,
}

func TestTransitionLuaKeysDeclared_QI09(t *testing.T) {
	src := transitionSrc
	if strings.Contains(src, "] ..") || regexp.MustCompile(`'[^']*:'\s*\.\.`).MatchString(src) {
		t.Error("transitionSrc concatenates to build a key (QI-09 smell)")
	}
	for _, m := range reCall.FindAllStringSubmatch(src, -1) {
		cmd, arg := m[1], strings.TrimSpace(m[2])
		if keyCmds[cmd] && !strings.HasPrefix(arg, "KEYS[") {
			t.Errorf("transitionSrc: redis.call('%s', %s ...) accesses an undeclared key (QI-09)", cmd, arg)
		}
	}
}
