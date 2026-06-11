package counters

// Gauge is a current-level counter (e.g. concurrent sandboxes per org).
// No TTL — gauges are managed via explicit increment/decrement.
//
// Wire-level: matches Python's `quota:gauge:{resource}:{scope}`.
type Gauge struct {
	Store       FloatStore
	Prefix      KeyPrefix
	ResourceKey string
}

// Key returns the gauge's key for the scope.
func (g Gauge) Key(scope string) string {
	return g.Prefix.Build("gauge", g.ResourceKey, scope)
}
