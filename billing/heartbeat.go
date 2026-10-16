package billing

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/internal/httpx"
)

// HeartbeatLoop sends Heartbeat every interval until ctx is cancelled.
// Survives transient billing-service errors (logs + continues). Returns
// a cancel func; callers can stop the loop without cancelling ctx.
type HeartbeatLoop struct {
	Client      *Client
	ServiceName string
	Version     string
	Interval    time.Duration

	once   sync.Once
	stopCh chan struct{}
}

// Start begins the loop. Idempotent — second Start is a no-op.
func (h *HeartbeatLoop) Start(ctx context.Context) {
	h.once.Do(func() {
		h.stopCh = make(chan struct{})
		if h.Interval == 0 {
			h.Interval = 60 * time.Second
		}
		go h.run(ctx)
	})
}

// Stop ends the loop. Safe to call before Start.
func (h *HeartbeatLoop) Stop() {
	if h.stopCh != nil {
		select {
		case <-h.stopCh:
		default:
			close(h.stopCh)
		}
	}
}

func (h *HeartbeatLoop) run(ctx context.Context) {
	t := time.NewTicker(h.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.stopCh:
			return
		case <-t.C:
			if err := h.Client.Heartbeat(ctx, HeartbeatRequest{
				ServiceName: h.ServiceName, Version: h.Version,
			}); err != nil {
				// v0.1.10 (ticket 20261016): billing serves no heartbeat route. A
				// 404/405 is a permanent answer, not a transient failure — stop,
				// once, instead of a warning every interval forever.
				if httpx.IsStatus(err, http.StatusNotFound) || httpx.IsStatus(err, http.StatusMethodNotAllowed) {
					slog.Info("billing heartbeat: billing serves no heartbeat route — heartbeat stopped " +
						"(harmless; nothing depends on it)")
					return
				}
				slog.Warn("billing heartbeat failed", "err", err)
			}
		}
	}
}
