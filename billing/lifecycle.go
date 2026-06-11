package billing

import (
	"context"
	"log/slog"
)

// LifecycleHook registers the consumer service with billing on startup.
// Best-effort; never blocks startup.
type LifecycleHook struct {
	Client      *Client
	ServiceName string
	Version     string
	Capability  string // optional — declares what the service emits
}

// Register sends one Heartbeat to record the service as live.
func (l *LifecycleHook) Register(ctx context.Context) {
	if err := l.Client.Heartbeat(ctx, HeartbeatRequest{
		ServiceName: l.ServiceName, Version: l.Version, Capability: l.Capability,
	}); err != nil {
		slog.Warn("billing lifecycle register failed", "err", err,
			"service", l.ServiceName)
		return
	}
	slog.Info("billing lifecycle: registered", "service", l.ServiceName,
		"version", l.Version)
}
