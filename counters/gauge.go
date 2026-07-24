package counters

// Gauge is a current-level counter (e.g. concurrent sandboxes per org).
// No TTL — gauges are managed via explicit increment/decrement.
//
// Wire-level: the Python parity key is `quota:{org}:{rk}:gauge` — built by
// OrgKey/UserKey in keys_python.go, which the engine uses. Key() below is
// the DEPRECATED pre-P5.3 shape (finding QG-03).
type Gauge struct {
	Store       FloatStore
	Prefix      KeyPrefix
	ResourceKey string
	// Keyspace (K-8): zero value = legacy v1 path, byte-identical.
	Keyspace Keyspace
}

// Key returns the gauge's key for the scope.
//
// Deprecated: produces the non-parity shape quota:gauge:{rk}:{scope}
// (finding QG-03). Use OrgKey/UserKey (keys_python.go). Retained until D-13.
func (g Gauge) Key(scope string) string {
	return DeprecatedScopeKey(g.Prefix, "gauge", g.ResourceKey, scope)
}
