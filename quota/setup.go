// Package quota is the consumer-facing entry point. Most users only ever
// call Setup; everything else is reachable via the returned *Quota handle.
//
// Wire-level parity: env-var names, config file search paths, capability
// gates, and default behaviors all match Python lib v0.5.2.
package quota

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/alerts"
	"github.com/ab0t-com/ab0t-quota-go/authevents"
	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/engine"
	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
	"github.com/ab0t-com/ab0t-quota-go/messages"
	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/ab0t-com/ab0t-quota-go/payment"
	"github.com/ab0t-com/ab0t-quota-go/providers"
	"github.com/ab0t-com/ab0t-quota-go/registry"
)

// Setup is the one-liner. Reads config from disk, builds every subsystem,
// and reports its capabilities. Consumers wire it once during startup.
//
// Example:
//
//	q, err := quota.Setup(ctx, quota.Options{
//	    ConfigPath: "quota-config.json",
//	    AutoSubscribeAuthEvents: true,
//	})
//	if err != nil { log.Fatal(err) }
//	defer q.Close(context.Background())
//
//	http.Handle("/api/", q.Middleware()(yourHandler))
type Options struct {
	// ConfigPath overrides the config file location. Empty → standard
	// search paths (see config.LoadConfig).
	ConfigPath string
	// ConfigOverride lets tests pass an already-parsed Config in-line.
	ConfigOverride *config.Config

	// AutoSubscribeAuthEvents triggers SubscribeOnStartup in a goroutine
	// once the handlers are registered.
	AutoSubscribeAuthEvents bool

	// CreditGranter is the callback the default credit-grant handler hits.
	// If nil, the credit-grant handler is NOT registered (an explicit
	// no-op rather than silently failing).
	CreditGranter authevents.CreditGranter

	// IdentityResolver maps requests → user_id / org_id. Required for the
	// middleware to function. Tests can use a stub.
	IdentityResolver func(any) (string, string, error)

	// Logger replaces the default slog logger.
	Logger *slog.Logger
}

// Quota is the configured runtime handle.
type Quota struct {
	Cfg         *config.Config
	Engine      *engine.Engine
	Provider    providers.Provider
	Registry    *registry.Registry
	Messages    *messages.Builder
	Alerts      *alerts.Manager
	Billing     *billing.Client // nil if AB0T_QUOTA_BILLING_URL not set
	Payment     *payment.Client // nil if AB0T_QUOTA_PAYMENT_URL not set
	LedgerStore handlerledger.LedgerStore
	PinStore    authevents.PinStore
	Heartbeat   *billing.HeartbeatLoop

	webhookHandler http.Handler
	capability     Capabilities
	closeFns       []func() error
}

// Capabilities reports which subsystems are wired ("on") and why. Emitted
// at Setup time as a single structured log line and accessible via Q.Capabilities().
type Capabilities struct {
	Engine           bool   // always true
	Enforcement      bool   // config.Enforcement.Enabled
	ShadowMode       bool   // config.Enforcement.ShadowMode
	Billing          bool   // billing client wired
	Payment          bool   // payment client wired
	Alerts           bool   // alerts manager dispatching
	AlertsWebhook    bool   // webhook dispatcher wired
	AuthEvents       bool   // receiver routable
	CreditGrant      bool   // default credit-grant handler registered
	AutoSubscribe    bool   // SubscribeOnStartup will fire
	LedgerBackend    string // "memory" | "redis_stub" | "ddb_stub"
	FloatStore       string // "memory" | "redis_stub"
	WhyOff           map[string]string
}

// Setup constructs a Quota.
func Setup(ctx context.Context, opts Options) (*Quota, error) {
	if opts.Logger != nil {
		slog.SetDefault(opts.Logger)
	}

	cfg := opts.ConfigOverride
	if cfg == nil {
		loaded, err := config.LoadConfig(opts.ConfigPath)
		if err != nil {
			return nil, fmt.Errorf("quota.Setup: load config: %w", err)
		}
		cfg = loaded
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("quota.Setup: validate config: %w", err)
	}

	cap := Capabilities{Engine: true, WhyOff: map[string]string{}}

	prov, err := providers.New(cfg.TierProvider)
	if err != nil {
		return nil, fmt.Errorf("quota.Setup: tier provider: %w", err)
	}
	if cfg.TierProvider.CacheTTLSec > 0 {
		prov = providers.WithCache(prov, time.Duration(cfg.TierProvider.CacheTTLSec)*time.Second)
	}

	reg := registry.New(cfg)
	factory := counters.NewMemoryFactory(counters.KeyPrefix(cfg.Storage.RedisKeyPrefix))
	cap.FloatStore = "memory"
	if cfg.Storage.RedisURL != "" {
		cap.WhyOff["redis_store"] = "v0.1.0 ships in-memory only; redis store wires in v0.2"
		slog.Warn("redis storage configured but stub backend in use; counters are process-local",
			"redis_url", cfg.Storage.RedisURL)
	}

	q := &Quota{
		Cfg:      cfg,
		Provider: prov,
		Registry: reg,
		Messages: messages.New(messages.Templates{}),
	}
	q.Engine = &engine.Engine{
		Cfg:      cfg,
		Reg:      reg,
		Provider: prov,
		Factory:  factory,
		Messages: q.Messages,
	}
	cap.Enforcement = cfg.Enforcement.Enabled
	cap.ShadowMode = cfg.Enforcement.ShadowMode
	if !cap.Enforcement {
		cap.WhyOff["enforcement"] = "config.enforcement.enabled = false"
	}

	// Mesh-side clients (optional).
	mu := mesh.Resolve()
	if mu.Billing != "" {
		c, err := billing.New(mu)
		if err == nil {
			q.Billing = c
			cap.Billing = true
		} else {
			cap.WhyOff["billing"] = err.Error()
		}
	} else {
		cap.WhyOff["billing"] = "AB0T_QUOTA_BILLING_URL not set"
	}
	if mu.Payment != "" {
		c, err := payment.New(mu)
		if err == nil {
			q.Payment = c
			cap.Payment = true
		} else {
			cap.WhyOff["payment"] = err.Error()
		}
	} else {
		cap.WhyOff["payment"] = "AB0T_QUOTA_PAYMENT_URL not set"
	}

	// Alerts.
	if cfg.Alerts.Enabled {
		var dispatcher alerts.Dispatcher = alerts.LogDispatcher{}
		if cfg.Alerts.WebhookURL != "" {
			wh, err := alerts.NewWebhookDispatcher(cfg.Alerts.WebhookURL)
			if err != nil {
				cap.WhyOff["alerts_webhook"] = err.Error()
			} else {
				dispatcher = alerts.Multi{alerts.LogDispatcher{}, wh}
				cap.AlertsWebhook = true
			}
		}
		q.Alerts = alerts.NewManager(cfg.Alerts, dispatcher)
		cap.Alerts = true
	} else {
		cap.WhyOff["alerts"] = "config.alerts.enabled = false"
	}

	// Auth events.
	q.LedgerStore = handlerledger.NewInMemoryLedgerStore()
	cap.LedgerBackend = "memory"
	if cfg.Storage.DynamoDBTable != "" || cfg.Storage.RedisURL != "" {
		cap.WhyOff["ledger_persistent"] = "v0.1.0 ships in-memory ledger; persistent backends wire in v0.2"
	}
	q.PinStore = authevents.NewMemoryPinStore()
	cap.AuthEvents = true
	secret := os.Getenv("AB0T_AUTH_WEBHOOK_SECRET")
	q.webhookHandler = authevents.MakeRouter(authevents.ReceiverConfig{
		Secret:      secret,
		LedgerStore: q.LedgerStore,
	})
	if secret == "" {
		cap.WhyOff["auth_events_signed"] = "AB0T_AUTH_WEBHOOK_SECRET not set — receiver will reject all events with 401"
	}

	// Credit-grant handler — only if a Granter is supplied.
	if opts.CreditGranter != nil {
		_, err := authevents.RegisterDefaultCreditGrantHandler(authevents.CreditGrantDeps{
			Config:       cfg,
			TierProvider: providerAdapter{prov},
			PinStore:     q.PinStore,
			Ledger:       q.LedgerStore,
			Granter:      opts.CreditGranter,
		})
		if err != nil {
			return nil, fmt.Errorf("quota.Setup: credit grant handler: %w", err)
		}
		cap.CreditGrant = true
	} else {
		cap.WhyOff["credit_grant"] = "no CreditGranter supplied; default handler not registered"
	}

	if opts.AutoSubscribeAuthEvents {
		cap.AutoSubscribe = true
		go func() {
			_, _ = authevents.SubscribeOnStartup(context.Background(), authevents.SubscribeInput{})
		}()
	}

	q.capability = cap
	logCapabilities(cap)
	return q, nil
}

// Capabilities returns the snapshot.
func (q *Quota) Capabilities() Capabilities { return q.capability }

// Close releases background goroutines + Persistence connections in safe
// order: stop heartbeat → flush ledger → close clients.
func (q *Quota) Close(ctx context.Context) error {
	if q.Heartbeat != nil {
		q.Heartbeat.Stop()
	}
	var firstErr error
	for _, fn := range q.closeFns {
		if err := fn(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// providerAdapter bridges providers.Provider → authevents.TierProvider.
type providerAdapter struct{ p providers.Provider }

func (a providerAdapter) GetTier(ctx context.Context, userID, orgID string) (string, error) {
	return a.p.GetTier(ctx, userID, orgID)
}

func logCapabilities(c Capabilities) {
	attrs := []any{
		"engine", c.Engine,
		"enforcement", c.Enforcement,
		"shadow_mode", c.ShadowMode,
		"billing", c.Billing,
		"payment", c.Payment,
		"alerts", c.Alerts,
		"alerts_webhook", c.AlertsWebhook,
		"auth_events", c.AuthEvents,
		"credit_grant", c.CreditGrant,
		"auto_subscribe", c.AutoSubscribe,
		"ledger", c.LedgerBackend,
		"float_store", c.FloatStore,
	}
	for k, v := range c.WhyOff {
		attrs = append(attrs, "off:"+k, v)
	}
	slog.Info("ab0t-quota capabilities", attrs...)
}

// ErrNoLedgerStore is returned when an operation needs a real ledger but
// only in-memory is available. v0.1.0 never returns this; v0.2 will when
// the operator opts into persistent ledger but the connection fails.
var ErrNoLedgerStore = errors.New("quota: no persistent ledger configured")
