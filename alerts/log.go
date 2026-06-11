package alerts

import (
	"context"
	"log/slog"

	"github.com/ab0t-com/ab0t-quota-go/engine"
)

// LogDispatcher writes to slog. Used by default when no webhook is set.
type LogDispatcher struct{}

// Send logs at Warn or Error based on level.
func (LogDispatcher) Send(_ context.Context, level Level, r engine.Result) error {
	attrs := []any{
		"resource", r.Resource,
		"tier", r.TierID,
		"used", r.Used,
		"threshold", r.Threshold,
	}
	if r.Limit != nil {
		attrs = append(attrs, "limit", *r.Limit)
	}
	if level == LevelCritical {
		slog.Error("quota threshold critical", attrs...)
	} else {
		slog.Warn("quota threshold warning", attrs...)
	}
	return nil
}
