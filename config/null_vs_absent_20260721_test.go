package config

// T-G1 RED (GO-01/E-63, pack 20260721_shared_lib_declared_not_discovered).
// JSON null, an absent key, and a set value must be three DISTINGUISHABLE
// states of storage.redis_url. Today `RedisURL string` + omitempty collapses
// all three to "", and quota.Setup reads "" as "use in-memory counters" —
// an undeclared store silently becomes a per-process counter (P0).
// Design: design_dependency_resolution_20260721.md §7.1 (Declared[T]).

import (
	"encoding/json"
	"reflect"
	"testing"
)

func unmarshalStorage(t *testing.T, doc string) StorageConfig {
	t.Helper()
	var sc StorageConfig
	if err := json.Unmarshal([]byte(doc), &sc); err != nil {
		t.Fatalf("unmarshal %s: %v", doc, err)
	}
	return sc
}

func TestRedisURLNullVsAbsentVsSet(t *testing.T) {
	null := unmarshalStorage(t, `{"redis_url": null}`)
	absent := unmarshalStorage(t, `{}`)
	set := unmarshalStorage(t, `{"redis_url": "redis://example.invalid:6379/0"}`)

	if reflect.DeepEqual(null, absent) {
		t.Errorf("GO-01: JSON null and an ABSENT redis_url unmarshal to identical "+
			"StorageConfig values (%+v) — the config type cannot represent 'declared null' "+
			"vs 'never declared', so an undeclared store is indistinguishable from a "+
			"declared-off one", null)
	}
	if reflect.DeepEqual(set, absent) {
		t.Errorf("a set redis_url must be distinguishable from an absent one")
	}
	if reflect.DeepEqual(set, null) {
		t.Errorf("a set redis_url must be distinguishable from an explicit null")
	}
}

// TestDeclaredStates pins the Declared[T] API introduced for the contract
// above (written at GREEN time — the API did not exist at RED).
func TestDeclaredStates(t *testing.T) {
	null := unmarshalStorage(t, `{"redis_url": null}`).RedisURL
	absent := unmarshalStorage(t, `{}`).RedisURL
	set := unmarshalStorage(t, `{"redis_url": "redis://example.invalid:6379/0"}`).RedisURL

	if !null.IsNull() || null.IsAbsent() {
		t.Errorf("explicit null: IsNull=%v IsAbsent=%v (want true,false)", null.IsNull(), null.IsAbsent())
	}
	if _, ok := null.Get(); ok {
		t.Errorf("explicit null must not report a declared value")
	}
	if !absent.IsAbsent() || absent.IsNull() {
		t.Errorf("absent: IsAbsent=%v IsNull=%v (want true,false)", absent.IsAbsent(), absent.IsNull())
	}
	if v, ok := set.Get(); !ok || v != "redis://example.invalid:6379/0" {
		t.Errorf("set: Get() = (%q,%v), want the declared value", v, ok)
	}
	if d := Declare("memory://"); d.IsAbsent() || d.IsNull() {
		t.Errorf("Declare() must read as a declaration")
	}
	if d := DeclareNull[string](); !d.IsNull() {
		t.Errorf("DeclareNull() must read as an explicit null")
	}
}
