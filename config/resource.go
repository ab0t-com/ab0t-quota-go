package config

import "fmt"

// CounterType is the kind of counter a resource uses.
type CounterType string

const (
	CounterGauge       CounterType = "gauge"       // current resource level (concurrent X)
	CounterRate        CounterType = "rate"        // sliding-window throughput
	CounterAccumulator CounterType = "accumulator" // monotonic-within-period (spend cap)
)

// ResetPeriod is the calendar boundary an accumulator resets on.
type ResetPeriod string

const (
	ResetHourly  ResetPeriod = "hourly"
	ResetDaily   ResetPeriod = "daily"
	ResetWeekly  ResetPeriod = "weekly"
	ResetMonthly ResetPeriod = "monthly"
)

// ResourceDef describes one trackable resource.
type ResourceDef struct {
	Service       string      `json:"service"`
	ResourceKey   string      `json:"resource_key"`
	DisplayName   string      `json:"display_name"`
	CounterType   CounterType `json:"counter_type"`
	Unit          string      `json:"unit,omitempty"`
	WindowSeconds int         `json:"window_seconds,omitempty"` // rate counters
	ResetPeriod   ResetPeriod `json:"reset_period,omitempty"`   // accumulators
	Precision     int         `json:"precision,omitempty"`      // decimal places for float counters
}

// Validate enforces shape rules.
func (r *ResourceDef) Validate() error {
	if r.ResourceKey == "" {
		return fmt.Errorf("resource: resource_key required")
	}
	if r.DisplayName == "" {
		r.DisplayName = r.ResourceKey
	}
	switch r.CounterType {
	case CounterGauge, CounterRate, CounterAccumulator:
	case "":
		return fmt.Errorf("resource %q: counter_type required", r.ResourceKey)
	default:
		return fmt.Errorf("resource %q: unknown counter_type %q", r.ResourceKey, r.CounterType)
	}
	if r.CounterType == CounterRate && r.WindowSeconds <= 0 {
		return fmt.Errorf("resource %q: window_seconds required for rate counters", r.ResourceKey)
	}
	if r.CounterType == CounterAccumulator && r.ResetPeriod == "" {
		return fmt.Errorf("resource %q: reset_period required for accumulator counters", r.ResourceKey)
	}
	return nil
}
