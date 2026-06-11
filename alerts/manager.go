// Package alerts dispatches threshold-crossing notifications.
//
// Two backends:
//
//	log     — slog.Warn / slog.Error
//	webhook — POST to a consumer-configured URL (SSRF-guarded)
//
// The default is log-only. Webhook is enabled when AlertsConfig.WebhookURL
// is set and passes the SSRF guard.
package alerts

import (
	"context"
	"sync"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/engine"
)

// Manager is the dispatch point. Engine code calls Notify on every Result
// where Decision is Warn or Critical; Manager applies cooldowns + fanout.
type Manager struct {
	Cfg        config.AlertsConfig
	Dispatcher Dispatcher

	mu       sync.Mutex
	lastSent map[string]time.Time
}

// Dispatcher writes an alert somewhere.
type Dispatcher interface {
	Send(ctx context.Context, level Level, r engine.Result) error
}

// Level is the alert severity.
type Level string

const (
	LevelWarning  Level = "warning"
	LevelCritical Level = "critical"
)

// NewManager constructs a Manager. Use LogDispatcher{} as the dispatcher
// for plain slog; chain dispatchers via Multi.
func NewManager(cfg config.AlertsConfig, d Dispatcher) *Manager {
	if cfg.CooldownSeconds == 0 {
		cfg.CooldownSeconds = 3600
	}
	if cfg.WarningThreshold == 0 {
		cfg.WarningThreshold = 0.80
	}
	if cfg.CriticalThreshold == 0 {
		cfg.CriticalThreshold = 0.95
	}
	return &Manager{Cfg: cfg, Dispatcher: d, lastSent: map[string]time.Time{}}
}

// Notify dispatches an alert if cooldown allows. Idempotent under bursts.
func (m *Manager) Notify(ctx context.Context, r engine.Result) {
	if !m.Cfg.Enabled || m.Dispatcher == nil {
		return
	}
	var level Level
	switch r.Decision {
	case engine.Critical:
		level = LevelCritical
	case engine.Warn:
		level = LevelWarning
	default:
		return
	}
	key := string(level) + ":" + r.Resource + ":" + r.TierID
	m.mu.Lock()
	last, seen := m.lastSent[key]
	cool := time.Duration(m.Cfg.CooldownSeconds) * time.Second
	if seen && time.Since(last) < cool {
		m.mu.Unlock()
		return
	}
	m.lastSent[key] = time.Now()
	m.mu.Unlock()
	_ = m.Dispatcher.Send(ctx, level, r)
}

// Multi chains dispatchers; the first error stops fanout. Use this when
// you want log + webhook.
type Multi []Dispatcher

// Send fans out to each child Dispatcher.
func (m Multi) Send(ctx context.Context, level Level, r engine.Result) error {
	for _, d := range m {
		if err := d.Send(ctx, level, r); err != nil {
			return err
		}
	}
	return nil
}
