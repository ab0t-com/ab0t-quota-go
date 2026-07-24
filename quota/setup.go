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
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/redis/go-redis/v9"

	"github.com/ab0t-com/ab0t-quota-go/activations"
	"github.com/ab0t-com/ab0t-quota-go/alerts"
	"github.com/ab0t-com/ab0t-quota-go/authevents"
	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/ddbguard"
	"github.com/ab0t-com/ab0t-quota-go/engine"
	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/ab0t-com/ab0t-quota-go/messages"
	"github.com/ab0t-com/ab0t-quota-go/outbox"
	"github.com/ab0t-com/ab0t-quota-go/payment"
	"github.com/ab0t-com/ab0t-quota-go/providers"
	"github.com/ab0t-com/ab0t-quota-go/reconcile"
	"github.com/ab0t-com/ab0t-quota-go/redisguard"
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

	// EnablePaid overrides the paid-mode assertion (D-44/D-34). nil → derive
	// from config (billing.enable_paid) OR a wired billing mesh client. When
	// true and the billing chain is severed (and !outbox.allow_ephemeral),
	// Setup REFUSES to start.
	EnablePaid *bool

	// SettlementPublisher is the transport that delivers settlement events off
	// the outbox (SNS in prod). REQUIRED for paid mode (D-56): the money-emit
	// path is emit → durable intent → publish(this) → drain → billing. Without
	// it the outbox is a durable store of nothing, so the gate refuses paid.
	SettlementPublisher outbox.Publisher

	// ObservedUsageProvider is the consumer's product-state truth (EXISTENCE)
	// for the reconciler (D-33). nil ⇒ ledger-only reconciliation.
	ObservedUsageProvider reconcile.ObservedUsageProvider

	// ReconcileOrgs supplies the orgs to reconcile each pass (the library can't
	// enumerate them). nil ⇒ the reconciler is NOT started (Capabilities says why).
	ReconcileOrgs func() []string

	// SkipBackgroundLoops suppresses the ONGOING side-effect loops — the
	// billing heartbeat POSTs and the outbox drain loop (which publishes and
	// SETTLES money events) — for smoke/diagnostic runs. `quotactl
	// capabilities` sets it (D-8): a pre-deploy check must not move money.
	// One-shot verification I/O (redis preflight incl. SCRIPT LOAD, DDB
	// table ensure/verify) still runs — it is the thing being verified.
	SkipBackgroundLoops bool
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
	Outbox      *outbox.Emitter       // durable lifecycle-settlement outbox (D-44); nil when billing off/ephemeral
	Reconciler  *reconcile.Reconciler // gauge-drift reconciler (D-62); nil when not safely runnable

	webhookHandler http.Handler
	capability     Capabilities
	// capMu guards capability + unsafeInvariants: D-75's re-verification WRITES them from
	// the reconciler goroutine while Healthy()/Capabilities() read them from request handlers.
	capMu            sync.RWMutex
	unsafeInvariants map[string]bool
	// counterUntrusted (D-80): the Redis ALREADY evicted keys, so the counter may be
	// under-counting live resources. The reconcile pass in the same tick converges it.
	counterUntrusted bool
	// outboxOnRedis (D-81): the outbox landed on Redis, so a persistence FAILURE there is
	// money nobody can reconstruct — not a counter that heals.
	outboxOnRedis bool
	// redisProbe is the counter's Redis, kept for D-75's periodic re-verification.
	redisProbe redisguard.Prober
	closeFns   []func() error
}

// Capabilities reports which subsystems are wired ("on") and why. Emitted
// at Setup time as a single structured log line and accessible via Q.Capabilities().
type Capabilities struct {
	Engine        bool   // always true
	Enforcement   bool   // config.Enforcement.Enabled
	ShadowMode    bool   // config.Enforcement.ShadowMode
	Billing       bool   // billing client wired
	Payment       bool   // payment client wired
	Alerts        bool   // alerts manager dispatching
	AlertsWebhook bool   // webhook dispatcher wired
	AuthEvents    bool   // receiver routable
	CreditGrant   bool   // default credit-grant handler registered
	AutoSubscribe bool   // SubscribeOnStartup will fire
	LedgerBackend string // "memory" | "redis" | "ddb" (self-reported by the store — QG-02)
	FloatStore    string // "memory" | "redis"
	Outbox        string // "off" | "none" | "DDB" | "Redis (...)" | "NON-DURABLE (...)"  (D-44)
	BillingStatus string // "OFF (paid disabled)" | "ON (chain complete: ...)" | "OFF — <weakest link>"
	Reconciler    string // "ON" | "OFF — <reason>" (D-62/D-39/D-51: absence is OFF, not healthy)
	// RedisTopology is the D-71 machine-checked verdict on the client's Redis:
	// "single-node[ (…assertion…)]" | "CLUSTER (unsupported)" | "unknown" | "n/a (no redis counter store)".
	// Setup REFUSES to start on anything but single-node/n-a; the field exists so the
	// verdict is READABLE afterwards and can FAIL Healthy() — an event with no sink is
	// not observability (D-40).
	RedisTopology string
	// CounterEvictionPolicy is the D-72 verdict on the COUNTER's Redis: the maxmemory-policy
	// read from the server, or "EVICTING/UNVERIFIED (...)". An allkeys-* policy evicts a LIVE
	// gauge → the counter reads zero for a running resource → over-admission (D-31). Setup
	// REFUSES to start on it; this field makes the verdict readable and FAILS Healthy().
	CounterEvictionPolicy string
	// RedisScripting is the D-73 verdict: "on (EVAL verified, ...)" | "OFF (...)".
	RedisScripting string
	// RedisVersion is the D-74 verdict: the version | "below_floor (...)" | "unknown (...)".
	// Recorded, but NOT probe-critical (see the artifact).
	RedisVersion string
	// DDBHandlerLedger is the D-82/D-76 verdict on the handler-ledger table.
	DDBHandlerLedger string
	// RedisPersistStatus is the D-81 verdict — the FACT, not the config. `appendonly yes` says
	// what SHOULD happen; aof_last_write_status says what DID. A failing AOF on the Redis
	// holding the OUTBOX is money nobody can reconstruct.
	RedisPersistStatus string
	// CounterEvictionsObserved is the D-80 verdict — the FACT, not the policy. A Redis that has
	// ALREADY evicted keys passes every policy check we own while an evicted gauge sits
	// under-counted in the counter. "0 (…)" | "evictions_observed (…)" | "unknown".
	CounterEvictionsObserved string
	// PreflightReverification is the D-79 derived contract: if the counter lives on Redis, its
	// invariants MUST be re-verified (the D-75 re-check rides the reconciler loop). A client who
	// switches the reconciler off no longer switches the GUARANTEE off silently — this reads
	// OFF and FAILS Healthy(). "on (rides the reconciler loop)" | "OFF — <reason>".
	PreflightReverification string
	// MemoryHeadroom is the D-77 verdict: "<pct>% of maxmemory used" | "low_headroom (...)" |
	// "unbounded (...)" | "unknown". `noeviction` fails CLOSED at the cliff (safe) but the
	// service DIES — so the cliff must be visible BEFORE 3am. Degrades on a READ low-headroom.
	MemoryHeadroom string
	// RedisReachable is the D-2/GT-T1 boot verdict on the DECLARED store:
	// "on (PING verified)" | "PROBE FAILED (…) [kind: detail]" |
	// "n/a (no redis counter store)". Published so the cause is readable and
	// health-checked (GO-10: a verdict nobody reads is not a verdict).
	RedisReachable string
	// Keyspace (K-8) is the ACTIVE counter key shape + migration phase, e.g.
	// "v1", "v1+dual(phase=dual)", "v2(phase=reaped)" — so an operator can
	// see which shape a live process reads/writes (keyspace spec §3).
	Keyspace string
	WhyOff         map[string]string
	// Resolved is the dependency-resolution provenance (T-G5, design §6.2 —
	// ENV-11's Go twin): WHERE each dependency's effective value came from,
	// so `quotactl capabilities` shows the resolved plan. Secret values never
	// appear (§6.3): URLs are userinfo-redacted, secrets are presence-only.
	Resolved map[string]ResolvedEntry
}

// ResolvedEntry is one row of the resolved dependency plan.
type ResolvedEntry struct {
	Value  string `json:"value"`  // redacted where secret-bearing
	Source string `json:"source"` // e.g. "config storage.redis_url" | "env QUOTA_REDIS_URL" | "unset"
}

// presenceEntry reports a secret dependency without its value (§6.3).
func presenceEntry(set bool, source string) ResolvedEntry {
	if set {
		return ResolvedEntry{Value: "present (value never shown)", Source: source}
	}
	return ResolvedEntry{Value: "not present", Source: "unset"}
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

	cap := Capabilities{Engine: true, WhyOff: map[string]string{}, Resolved: map[string]ResolvedEntry{}}

	// K-8 (keyspace spec §3.2): the declared keyspace state — config.Validate
	// has already guarded values; NewKeyspace re-guards the charset/states.
	keyspace, ksErr := counters.NewKeyspace(cfg.ServiceName,
		cfg.Storage.KeyspaceVersion, cfg.Storage.KeyspaceDualWrite)
	if ksErr != nil {
		if cfg.Storage.KeyspaceVersion == 2 || cfg.Storage.KeyspaceDualWrite {
			return nil, fmt.Errorf("quota.Setup: keyspace declaration: %w", ksErr)
		}
		// v1 keys carry no scope: an odd service_name must not break a
		// drop-in upgrade. Loud; the marker boot-guard runs unscoped.
		slog.Warn("service_name cannot be a keyspace scope (charset guard) — keeping the "+
			"unscoped v1 keyspace; fix it before any v2 migration", "err", ksErr)
		keyspace = counters.Keyspace{}
	}
	cap.Keyspace = fmt.Sprintf("v%d", cfg.Storage.KeyspaceVersion)
	if cfg.Storage.KeyspaceVersion == 0 {
		cap.Keyspace = "v1"
	}
	if keyspace.DualWrite {
		cap.Keyspace += "+dual"
	}

	prov, err := providers.New(cfg.TierProvider)
	if err != nil {
		return nil, fmt.Errorf("quota.Setup: tier provider: %w", err)
	}
	if cfg.TierProvider.CacheTTLSec > 0 {
		prov = providers.WithCache(prov, time.Duration(cfg.TierProvider.CacheTTLSec)*time.Second)
	}

	reg := registry.New(cfg)

	// Counter storage (TASK P5.1 / findings QG-01/QG-02). A configured
	// redis_url builds a durable go-redis factory whose counters survive
	// restart and are shared across replicas. If the URL is malformed or
	// the server is unreachable, degrade LOUDLY to memory and say so in
	// Capabilities — never silently (that was the QG-01/QG-02 bug).
	var (
		factory     *counters.Factory
		redisClient *redis.Client
	)
	// Key prefix (D-17 / finding QG-07). Python has NO prefix knob — it
	// hardcodes the "quota" head. A non-default Go prefix can ONLY diverge
	// the keyspace off a Python fleet (the exact bug class this lane kills),
	// and it has no correct non-default value today, so it is REJECTED at
	// startup rather than warned about ("a warning nobody reads is fail-open
	// with extra steps"). Fail closed: refuse to boot with a forking prefix.
	prefixStr := cfg.Storage.RedisKeyPrefix
	if prefixStr == "" {
		prefixStr = "quota"
	} else if prefixStr != "quota" {
		return nil, fmt.Errorf(
			"quota.Setup: storage.redis_key_prefix=%q is not permitted — Python hardcodes the "+
				"\"quota\" key head, so any other prefix forks the keyspace and breaks cross-runtime "+
				"parity (finding QG-07/D-17). Unset it or set it to \"quota\"", prefixStr)
	}
	prefix := counters.KeyPrefix(prefixStr)
	// GO-01 (P0, pack 20260721) — the counter store must be DECLARED. An
	// absent/null redis_url was silently read as "use in-memory counters":
	// every replica admitted the full limit independently and a restart
	// zeroed usage. Undeclared ⇒ typed startup error (QUOTA-CFG-001);
	// in-memory survives ONLY as the explicit "memory://" declaration (D-5(b)).
	storeRes, cfgErr := config.ResolveString(cfg.Storage.RedisURL, config.Spec{
		Name:        "Redis counter store URL",
		ConfigKey:   "storage.redis_url",
		Env:         []string{"QUOTA_REDIS_URL"},
		Requirement: config.Required,
		Code:        "QUOTA-CFG-001",
		Previously: "earlier versions silently fell back to an IN-MEMORY, PER-PROCESS counter: " +
			"every replica admitted the full limit independently and a restart zeroed usage. " +
			"This version refuses instead.",
		Remedy: "set storage.redis_url (or QUOTA_REDIS_URL). For single-process dev, declare it " +
			"explicitly: \"redis_url\": \"memory://\" — capabilities will report float_store=memory, " +
			"redis_topology=n/a (no redis counter store)",
		Docs: "CONSUMING.md#prerequisites · verify before deploy: quotactl capabilities --config quota-config.json",
	})
	if cfgErr != nil {
		return nil, cfgErr
	}
	redisDeclared := storeRes.Value != "memory://"
	slog.Info("counter store resolved", "source", storeRes.Source,
		"url", config.RedactURL(storeRes.Value))
	// T-G5 — the resolved plan with provenance (§6.2). Secrets: presence only.
	cap.Resolved["redis_url"] = ResolvedEntry{Value: config.RedactURL(storeRes.Value), Source: storeRes.Source}
	cap.Resolved["redis_password"] = presenceEntry(cfg.Storage.RedisPassword != "", "config storage.redis_password")
	if cfg.Storage.DynamoDBTable != "" {
		cap.Resolved["dynamodb_table"] = ResolvedEntry{Value: cfg.Storage.DynamoDBTable, Source: "config storage.dynamodb_table"}
	} else {
		cap.Resolved["dynamodb_table"] = ResolvedEntry{Value: "", Source: "unset"}
	}
	if cfg.Storage.DynamoDBRegion != "" {
		cap.Resolved["dynamodb_region"] = ResolvedEntry{Value: cfg.Storage.DynamoDBRegion, Source: "config storage.dynamodb_region"}
	} else {
		cap.Resolved["dynamodb_region"] = ResolvedEntry{Value: "(resolved by the AWS SDK chain at client build; error if it resolves nothing)", Source: "aws-sdk default chain"}
	}
	if cfg.Outbox.SNSTopicARN != "" {
		cap.Resolved["sns_topic_arn"] = ResolvedEntry{Value: cfg.Outbox.SNSTopicARN, Source: "config outbox.sns_topic_arn"}
	} else {
		cap.Resolved["sns_topic_arn"] = ResolvedEntry{Value: "", Source: "unset"}
	}
	if cfg.Outbox.SNSRegion != "" {
		cap.Resolved["sns_region"] = ResolvedEntry{Value: cfg.Outbox.SNSRegion, Source: "config outbox.sns_region"}
	} else {
		cap.Resolved["sns_region"] = ResolvedEntry{Value: "(resolved by the AWS SDK chain at client build; error if it resolves nothing)", Source: "aws-sdk default chain"}
	}
	if redisDeclared {
		// T-13 / D-2 / GO-10: a DECLARED but unreachable Redis retries the
		// unreachable kind within storage.connect_retry_seconds, then REFUSES
		// with a typed reachability error. It NEVER degrades to in-memory —
		// the old degrade served per-process counters behind a GREEN health
		// probe (information_go_availability_20260721.md). Runtime failure
		// after boot stays loud-not-fatal (D-75), unchanged.
		client, rerr := connectDeclaredRedis(ctx, storeRes.Value, cfg.Storage.RedisPassword,
			storeRes.Source, cfg.Storage.ConnectRetrySeconds, &cap)
		if rerr != nil {
			return nil, rerr
		}
		{
			// D-71 — machine-check the Redis TOPOLOGY before anything touches the
			// counter. The counter's Lua scripts are multi-key and CROSSSLOT on a
			// Redis Cluster (D-23, observed at a real cluster); our prod is
			// single-node, so only a CLIENT would ever hit it — which is exactly why
			// a LIBRARY may not assume it. Refuse LOUDLY at startup instead of
			// breaking silently at the first Acquire. Same shape as D-32's durability
			// machine-check. The verdict is recorded in Capabilities BEFORE any
			// refusal, so the cause is readable.
			topo, detail := CheckRedisClusterTopology(ctx, client, clusterConfirmedDisabled(cfg.Storage))
			cap.RedisTopology = TopologyCapabilityValue(topo, detail)
			if topo != TopologySingleNode {
				_ = client.Close()
				slog.Error("REDIS TOPOLOGY UNSUPPORTED/UNVERIFIED (D-71) — refusing to start",
					"topology", topo, "detail", detail)
				return nil, TopologyError(topo, detail)
			}
			slog.Info("redis topology verified (D-71)", "detail", detail)

			// D-72/D-73/D-74 — the rest of the Redis preflight, in order of how QUIETLY
			// each fails. D-72 is the urgent one: an `allkeys-*` Redis evicts a LIVE
			// gauge, the counter reads zero for a resource that is still running, and
			// admission silently over-grants (D-31's forbidden direction) — at runtime,
			// as free quota, behind a green health check. D-71 at least refuses loudly.
			if err := gateCounterStore(ctx, client, cfg, &cap); err != nil {
				_ = client.Close()
				return nil, err
			}
			// K-8 (spec §3.3): keyspace boot guards — QUOTA-CFG-011 (version
			// regression against a completed migration) / QUOTA-CFG-012
			// (brownfield v2 over live v1 keys). Typed refusals, fatal.
			marker, kerr := counters.CheckBootKeyspace(ctx, client, keyspace)
			if kerr != nil {
				_ = client.Close()
				slog.Error("KEYSPACE BOOT GUARD refused to start (K-8, spec §3.3)", "err", kerr)
				return nil, fmt.Errorf("quota.Setup: %w", kerr)
			}
			phase := "none"
			if marker != nil && marker.Phase != "" {
				phase = marker.Phase
			}
			cap.Keyspace += "(phase=" + phase + ")"

			redisClient = client
			f, ferr := counters.NewRedisFactory(client, prefix)
			if ferr != nil {
				// Should not happen (client is non-nil) — and per D-2/GO-10 a
				// declared store NEVER silently becomes in-memory: refuse.
				_ = client.Close()
				return nil, fmt.Errorf("quota.Setup: redis counter factory build failed for the "+
					"DECLARED store (D-2: never a silent in-memory fallback): %w", ferr)
			}
			cap.FloatStore = "redis"
			f.Keyspace = keyspace
			factory = f
		}
	} else {
		cap.FloatStore = "memory"
		// D-71: no Redis counter store ⇒ no cluster to CROSSSLOT on. An affirmative
		// "not applicable", never a silent absence (D-51).
		cap.RedisTopology = TopologyNA
		cap.RedisReachable = TopologyNA // affirmative n/a — no store to reach (D-51)
		factory = counters.NewMemoryFactory(prefix)
		factory.Keyspace = keyspace
		slog.Info("counter store: EXPLICIT in-memory (\"memory://\", declared) — " +
			"counters are process-local and reset on restart; a declaration, not a degradation")
	}

	q := &Quota{
		Cfg:      cfg,
		Provider: prov,
		Registry: reg,
		Messages: messages.New(messages.Templates{}),
	}
	if redisClient != nil {
		q.redisProbe = redisClient // D-75: the counter's Redis, re-verified every reconcile pass
	}
	q.Engine = &engine.Engine{
		Cfg:      cfg,
		Reg:      reg,
		Provider: prov,
		Factory:  factory,
		Messages: q.Messages,
	}
	if redisClient != nil {
		q.closeFns = append(q.closeFns, redisClient.Close)
	}
	cap.Enforcement = cfg.Enforcement.Enabled
	cap.ShadowMode = cfg.Enforcement.ShadowMode
	if !cap.Enforcement {
		cap.WhyOff["enforcement"] = "config.enforcement.enabled = false"
	}

	// Mesh-side clients (optional).
	mu := mesh.Resolve()
	if mu.Billing != "" {
		cap.Resolved["billing_url"] = ResolvedEntry{Value: mu.Billing, Source: "env " + mesh.EnvBillingURL}
	} else {
		cap.Resolved["billing_url"] = ResolvedEntry{Value: "", Source: "unset"}
	}
	if mu.Payment != "" {
		cap.Resolved["payment_url"] = ResolvedEntry{Value: mu.Payment, Source: "env " + mesh.EnvPaymentURL}
	} else {
		cap.Resolved["payment_url"] = ResolvedEntry{Value: "", Source: "unset"}
	}
	if mu.Billing != "" {
		c, err := billing.New(mu)
		if err == nil {
			q.Billing = c
			cap.Billing = true
			// D4 (QB-04) — wire + START the heartbeat. Previously only Stop()
			// was called; Start() had no caller, so the loop never beat —
			// dormant safety code that creates false confidence. It now runs
			// while billing is wired; Close() stops it.
			if opts.SkipBackgroundLoops {
				slog.Info("billing heartbeat NOT started (SkipBackgroundLoops — smoke run)")
			} else {
				q.Heartbeat = &billing.HeartbeatLoop{Client: c, ServiceName: cfg.ServiceName, Version: "go"}
				q.Heartbeat.Start(context.Background())
			}
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
		// D-26: subscribe the engine's over-admission events to the alert
		// manager. Dependency inverted — the engine publishes to OnEvent (a
		// seam it owns) and setup forwards here, so the engine never imports
		// alerts (no cycle). Without this the over_limit_admitted event has
		// no sink and D-24 B's observability premise is unmet.
		am := q.Alerts
		q.Engine.OnEvent = func(ctx context.Context, ev engine.QuotaEvent) {
			am.NotifyQuotaEvent(ctx, ev)
		}
	} else {
		cap.WhyOff["alerts"] = "config.alerts.enabled = false"
	}

	// Auth events. Handler ledger provides durable idempotency for
	// money-adjacent auth events (credit grants). Backend selection +
	// truthful degradation live in handlerledger.AutoSelectStore (priority
	// DDB > Redis > memory; TASK P1.7/P5.1). Both real backends are wired:
	// DDB via aws-sdk (verified against DynamoDB Local), Redis reuses the
	// counter client. The store self-reports its actual backend (QG-02).
	// NB: assign Redis only when non-nil — a typed-nil *redis.Client boxed
	// into the interface{} field reads as non-nil (Go nil-interface gotcha)
	// and would make AutoSelectStore wrongly pick Redis in degraded mode.
	ledgerOpts := handlerledger.AutoSelectOptions{}
	if redisClient != nil {
		ledgerOpts.Redis = redisClient
	}
	if cfg.Storage.DynamoDBTable != "" {
		if ddbClient, derr := newDDBClient(ctx, cfg.Storage); derr == nil {
			ledgerOpts.DDBClient = ddbClient
			ledgerOpts.DDBTable = cfg.Storage.DynamoDBTable
		} else {
			cap.WhyOff["ledger_ddb"] = "dynamodb_table set but client build failed: " + derr.Error()
			slog.Warn("handler ledger: DDB configured but client build failed — will fall back", "err", derr)
		}
	}
	q.LedgerStore = handlerledger.AutoSelectStore(ledgerOpts)

	// D-82 — the handler ledger PROVISIONS its table and is PREFLIGHTED (D-76), exactly like
	// the outbox and the activation store. It used to ASSUME the table existed: a client who
	// wired it hit ResourceNotFoundException at their FIRST auth webhook, in production. It
	// stayed invisible for the most instructive reason (D-78): the only thing that had ever
	// exercised it was a fake — and a fake never notices, because a fake creates nothing.
	if prov, ok := q.LedgerStore.(interface {
		EnsureTable(context.Context, time.Duration) error
		Table() string
	}); ok && ledgerOpts.DDBClient != nil {
		if err := prov.EnsureTable(ctx, 60*time.Second); err != nil {
			return nil, fmt.Errorf("quota.Setup: handler-ledger table (D-82): %w", err)
		}
		if dc, ok := ledgerOpts.DDBClient.(ddbguard.Describer); ok {
			pitr := cfg.Storage.DDBPITRConfirmed || ddbguard.EnvPITRConfirmed()
			value, fatal, warn := ddbguard.VerifyTable(ctx, dc, prov.Table(), "ttl", pitr)
			if warn != "" {
				slog.Warn("DDB preflight WARNING (D-76/D-82)", "capability", "ddb_handler_ledger", "detail", warn)
			}
			if fatal != "" {
				slog.Error("HANDLER-LEDGER TABLE UNSAFE (D-76/D-82)", "detail", fatal)
				return nil, ddbguard.PreflightError("handler_ledger", fatal)
			}
			cap.DDBHandlerLedger = value
		}
	}
	if b, ok := q.LedgerStore.(handlerledger.Backended); ok {
		cap.LedgerBackend = b.Backend()
	} else {
		cap.LedgerBackend = "memory"
	}
	if cap.LedgerBackend == "memory" {
		if cfg.Storage.DynamoDBTable != "" {
			cap.WhyOff["ledger_persistent"] = "ddb configured but unavailable; ledger degraded to in-memory (see ledger_ddb WhyOff)"
		} else if redisDeclared {
			cap.WhyOff["ledger_persistent"] = "redis unavailable; ledger degraded to in-memory (see redis_store WhyOff)"
		}
	}
	q.PinStore = authevents.NewMemoryPinStore()
	cap.AuthEvents = true
	secret := os.Getenv("AB0T_AUTH_WEBHOOK_SECRET")
	cap.Resolved["auth_webhook_secret"] = presenceEntry(secret != "", "env AB0T_AUTH_WEBHOOK_SECRET")
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

	// D-39 — the activation ledger is authoritative for identity + cost, so it
	// must be DURABLE. Self-provision a DDB activation store when DDB is
	// reachable; the engine falls back to its loud in-memory default otherwise.
	// (Redis-under-durability-check + the reconciler's refuse-gate are a later
	// leg; here we make the durable store the default when available.)
	if ddbSignalPresent(cfg) {
		if ddbClient, derr := newDDBClient(ctx, cfg.Storage); derr == nil {
			as := activations.NewDDBStore(ddbClient, "", 0) // dedicated table (ab0t_quota_activations)
			if eerr := as.EnsureTable(ctx, 60*time.Second); eerr == nil {
				q.Engine.Activations = as
				cap.WhyOff["activation_store"] = "" // durable
				slog.Info("activation store: DDB (durable)")
			} else {
				slog.Warn("activation store: DDB configured but table not ready — using in-memory (NOT durable)", "err", eerr)
				cap.WhyOff["activation_store"] = "ddb configured but unavailable; in-memory (NOT durable — D-39)"
			}
		}
	}

	// D-62/D-33/D-39 — the library reconciler. Started (a real goroutine, not a
	// dead worker) ONLY when it is safe: a durable activation ledger, a
	// recent-activity guard (wired by default from the engine's touch-tracking),
	// and a consumer-supplied org source. Otherwise Capabilities reports
	// reconciler=OFF with the reason — absence is OFF, never silently healthy.
	q.Reconciler = q.wireReconciler(cfg, opts, &cap)

	// D-79 — DERIVE the re-verification requirement from CONFIG, never from the wiring
	// (D-66's law: wiring SATISFIES a contract, it does not DEFINE one). The D-75 re-check
	// rides the reconciler loop, which a client can switch off — so if the counter lives on
	// Redis, a missing re-verification is a MISSING GUARANTEE and must fail Healthy(). It can
	// no longer disappear quietly. NOTE the subtlety: a LIVE reconciler with no preflight
	// wired onto it is NOT the guarantee — liveness of the carrier is not delivery of the cargo.
	if redisDeclared && cap.FloatStore == "redis" {
		switch {
		case q.Reconciler == nil:
			cap.PreflightReverification = "OFF — no reconciler loop, so the Redis invariants " +
				"(topology/eviction/scripting/version/headroom) are NEVER re-verified after boot (D-75/D-79)"
			cap.WhyOff["preflight_reverification"] = "reconciler not running"
			slog.Error("PREFLIGHT RE-VERIFICATION IS OFF (D-79) — the counter is on Redis, but no " +
				"reconciler loop is running to re-verify its invariants. A config change under a running " +
				"process (allkeys-lru at 3am) would go unnoticed. Health is DEGRADED.")
		case q.Reconciler.Preflight == nil:
			cap.PreflightReverification = "OFF — the reconciler loop runs but carries no preflight (D-79)"
			cap.WhyOff["preflight_reverification"] = "no preflight wired onto the reconciler"
		default:
			cap.PreflightReverification = "on (rides the reconciler loop)"
		}
	} else {
		cap.PreflightReverification = "n/a (no redis counter store)"
	}

	// D-44 / D-34 — the fail-closed billing gate. A paid service that cannot
	// durably record billing must NOT come up serving billable work for free
	// (that IS QB-01, through a different door). Resolve a durable outbox; if
	// none and enable_paid and !allow_ephemeral, REFUSE to start. Do this LAST
	// so q.Billing (mesh-wired) is known — wiring billing IS an assertion of
	// paid intent. Explicit override: billing.enable_paid, or Options.EnablePaid.
	enablePaid := cfg.Billing.EnablePaid || q.Billing != nil
	if opts.EnablePaid != nil {
		enablePaid = *opts.EnablePaid
	}
	// D-56/D-63 — auto-wire the concrete SNS settlement publisher from config
	// when the consumer didn't supply one, so a config-only paid service gets a
	// WORKING chain (not merely a safely-refused one).
	publisher := opts.SettlementPublisher
	if publisher == nil && cfg.Outbox.SNSTopicARN != "" {
		if sc, serr := newSNSClient(ctx, cfg.Outbox); serr == nil {
			publisher = outbox.NewSNSPublisher(sc, cfg.Outbox.SNSTopicARN)
			slog.Info("settlement publisher: SNS (auto-wired from config)", "topic", cfg.Outbox.SNSTopicARN)
		} else {
			slog.Error("settlement publisher: SNS configured but client build failed", "err", serr)
		}
	}
	if err := q.gateBillingChain(ctx, cfg, redisClient, publisher, enablePaid, opts.SkipBackgroundLoops, &cap); err != nil {
		return nil, err
	}

	q.capability = cap
	logCapabilities(cap)
	return q, nil
}

// gateBillingChain applies D-56: bill only when the WHOLE chain exists —
// emit → durable intent → publish → drain → sink → billing. A gate on one
// link is satisfiable while the chain is severed; "has a durable outbox" was
// never the guarantee, "usage reaches billing" is. Each link needs a POSITIVE
// signal; absence is UNKNOWN and unknown FAILS CLOSED (D-51). Capabilities
// names the WEAKEST link — never a cheerful ON because one component exists.
func (q *Quota) gateBillingChain(ctx context.Context, cfg *config.Config, redisClient *redis.Client, publisher outbox.Publisher, enablePaid, skipLoops bool, cap *Capabilities) error {
	// Assess every link. ok=true is the required positive signal.
	store, durable, detail := outbox.Store(nil), false, "none"
	if cfg.Outbox.OutboxEnabled() {
		store, durable, detail = resolveDurableOutbox(ctx, cfg, redisClient)
		// D-81: if the outbox landed on Redis, a persistence FAILURE there is money nobody can
		// reconstruct — not a counter that heals. The runtime re-check grades it accordingly.
		if redisClient != nil && !strings.HasPrefix(detail, "DDB") {
			q.capMu.Lock()
			q.outboxOnRedis = true
			q.capMu.Unlock()
		}
	} else {
		detail = "outbox.enabled=false"
	}
	links := []struct {
		name string
		ok   bool
		why  string
	}{
		{"durable_store", durable, "no durable outbox (" + detail + ")"},
		{"publisher", publisher != nil, "no settlement publisher wired (Options.SettlementPublisher)"},
		{"billing_sink", q.Billing != nil, "no billing sink (AB0T_QUOTA_BILLING_URL not set)"},
	}
	cap.Outbox = detail
	if durable {
		cap.Outbox = detail
	} else {
		cap.Outbox = "NON-DURABLE (" + detail + ")"
	}

	// Weakest (first-missing) link.
	weakest := ""
	for _, l := range links {
		if !l.ok {
			weakest = l.why
			break
		}
	}

	if weakest == "" {
		// Whole chain present → wire the emitter with the publisher AND start
		// the drain loop (a durable store nobody drains is a store of nothing).
		horizon := cfg.Outbox.MaxRetryHorizonSeconds
		em := outbox.NewEmitter(store, publisher, horizon, cfg.Outbox.PastHorizon)
		if q.Alerts != nil { // (e) settlement voids → money-incident alerts
			am := q.Alerts
			em.OnVoid = func(v outbox.VoidEntry) {
				am.NotifyVoid(context.Background(), v.ReservationID, v.EventType, v.Reason)
			}
		}
		// D-12 (the CALLER leg): the settlement fallback. Billing's durable settlement path
		// closes the revenue-loss hole — but ONLY if something calls it. Without this, the
		// emitter keeps voiding-and-alerting past the horizon and the money is still gone:
		// a mechanism with no caller (D-64). q.Billing is non-nil here by construction — the
		// "billing_sink" link above is part of the chain this branch requires.
		// Ticket: billing/output/tickets/20260712_revenue_chain_integrity
		if q.Billing != nil {
			em.SetSettler(billingSettler{c: q.Billing})
			slog.Info("D-12: outbox settlement fallback ARMED — a money event past its " +
				"reservation window will be SETTLED against billing, not voided")
		}
		q.Outbox = em
		if skipLoops {
			// D-8: a smoke run assesses the chain but must not DRAIN it —
			// draining publishes and settles real money events.
			cap.BillingStatus = "ON (chain complete: outbox=" + detail + "; drain loop OFF — smoke run)"
			slog.Info("billing chain complete — drain loop NOT started (SkipBackgroundLoops)", "outbox", detail)
			return nil
		}
		drainCtx, cancel := context.WithCancel(context.Background())
		q.closeFns = append(q.closeFns, func() error { cancel(); return nil })
		interval := time.Duration(cfg.Outbox.DrainIntervalSeconds) * time.Second
		maxPer := cfg.Outbox.MaxPerPass
		go em.RunDrainLoop(drainCtx, interval, maxPer)
		cap.BillingStatus = "ON (chain complete: outbox=" + detail + ")"
		slog.Info("billing chain complete — drain loop started", "outbox", detail)
		return nil
	}

	// Chain severed.
	if !enablePaid {
		cap.BillingStatus = "OFF (paid disabled)"
		return nil
	}
	if cfg.Outbox.AllowEphemeral {
		cap.BillingStatus = "OFF — " + weakest + " (allow_ephemeral=true)"
		cap.WhyOff["billing"] = weakest + "; allow_ephemeral=true (DEV) — billing DISABLED"
		slog.Error("BILLING DISABLED: billing chain severed (allow_ephemeral=true, DEV)", "weakest_link", weakest)
		return nil
	}
	cap.BillingStatus = "OFF — " + weakest
	return billingChainError(weakest)
}

// BillingHealthy reports whether the billing chain is complete (a health probe
// that FAILS while any link is missing — D-56/D-40). A missing link is not
// healthy; it is unknown, and unknown is unhealthy (D-51).
func (q *Quota) BillingHealthy() (bool, string) {
	if strings.HasPrefix(q.capability.BillingStatus, "ON") {
		return true, q.capability.BillingStatus
	}
	return false, q.capability.BillingStatus
}

// Healthy is the Capabilities CONSUMER (f): a money-aware health surface that
// FAILS when billing OR the reconciler is OFF. Absence of a positive signal is
// UNKNOWN, and unknown fails closed (D-49/D-51) — a service that never wired
// integrity must degrade, never report OK. Returns (healthy, per-subsystem
// reasons) for a /healthz or /capabilities handler.
func (q *Quota) Healthy() (bool, map[string]string) {
	q.capMu.RLock()
	defer q.capMu.RUnlock()
	reasons := map[string]string{
		"billing":                    q.capability.BillingStatus,
		"reconciler":                 q.capability.Reconciler,
		"redis_reachable":            q.capability.RedisReachable,
		"redis_topology":             q.capability.RedisTopology,
		"counter_eviction_policy":    q.capability.CounterEvictionPolicy,
		"redis_scripting":            q.capability.RedisScripting,
		"memory_headroom":            q.capability.MemoryHeadroom,
		"counter_evictions_observed": q.capability.CounterEvictionsObserved,
		"preflight_reverification":   q.capability.PreflightReverification,
		"redis_persist_status":       q.capability.RedisPersistStatus,
		"keyspace":                   q.capability.Keyspace,
	}
	billingOK := strings.HasPrefix(q.capability.BillingStatus, "ON") || q.capability.BillingStatus == "OFF (paid disabled)"
	reconcilerOK := q.capability.Reconciler == "ON" || q.capability.Reconciler == "OFF — not requested"
	// D-71 — an unsupported/unverified Redis topology is a service whose counter
	// primitive cannot run (CROSSSLOT). Setup refuses to start on it, so a live
	// process should never show it; the probe judges it anyway, because a capability
	// nothing reads is the defect this ticket met eight times. Absence ⇒ not healthy
	// (D-49/D-51).
	topologyOK := TopologyOK(q.capability.RedisTopology)
	// D-2 / GO-10 — the reachability verdict must FEED a predicate: only the
	// affirmative "on (PING verified)" (or the memory-mode n/a) is healthy.
	// Absence is not a value; a probe-failed value never reads green.
	reachableOK := q.capability.FloatStore != "redis" ||
		strings.HasPrefix(q.capability.RedisReachable, "on")
	// D-72/D-73 — an evicting (or unverified) counter store silently over-admits, and a
	// Redis that cannot run our Lua cannot count at all. Setup refuses to start on either,
	// so a live process should never show them; the probe judges them anyway, because a
	// capability nothing reads is the defect this ticket met eight times. Absence ⇒ not
	// healthy (D-49/D-51). An in-memory counter store (no Redis) reports n/a and is exempt.
	counterOK := q.capability.FloatStore != "redis" || CounterStoreOK(q.capability.CounterEvictionPolicy)
	scriptOK := q.capability.FloatStore != "redis" || ScriptingOK(q.capability.RedisScripting)
	// D-77 — the memory cliff must be visible BEFORE the service dies at it.
	memOK := redisguard.MemoryHeadroomOK(q.capability.MemoryHeadroom)
	// D-80 — the FACT. An observed eviction is a money incident even if the policy now reads clean.
	factsOK := q.capability.FloatStore != "redis" ||
		redisguard.EvictionFactsOK(q.capability.CounterEvictionsObserved)
	// D-79 — a guarantee the client can switch off SILENTLY is not a guarantee. If the counter is
	// on Redis, the re-verification is REQUIRED (derived from config, D-66): absent ⇒ degraded.
	reverifyOK := q.capability.FloatStore != "redis" ||
		strings.HasPrefix(q.capability.PreflightReverification, "on") ||
		strings.HasPrefix(q.capability.PreflightReverification, "n/a")
	// D-81 — a Redis that is FAILING to persist is not durable, however green `appendonly` reads.
	persistOK := redisguard.PersistFactsOK(q.capability.RedisPersistStatus)
	return billingOK && reconcilerOK && reachableOK && topologyOK && counterOK && scriptOK && memOK &&
		factsOK && reverifyOK && persistOK, reasons
}

// wireReconciler starts the reconciler loop when — and only when — it is safe
// (durable ledger + guard + org source). Otherwise it returns nil and records
// the reason (D-62/D-39/D-51: absence is OFF, never silently healthy).
func (q *Quota) wireReconciler(cfg *config.Config, opts Options, cap *Capabilities) *reconcile.Reconciler {
	if opts.ReconcileOrgs == nil {
		cap.Reconciler = "OFF — not requested"
		return nil
	}
	d, ok := q.Engine.Activations.(interface{ Durable() bool })
	if !ok || !d.Durable() {
		cap.Reconciler = "OFF — activation store not durable (D-39); wire a durable (DDB) activation store"
		slog.Error("reconciler OFF — non-durable activation ledger (D-39)")
		return nil
	}
	guardWindow := 120 * time.Second
	interval := time.Duration(cfg.Reconciliation.IntervalSeconds) * time.Second
	r := &reconcile.Reconciler{
		Store: q.Engine.Activations, Factory: q.Engine.Factory, Reg: q.Registry,
		Provider:        opts.ObservedUsageProvider,
		RecentlyTouched: q.Engine.TouchGuard(guardWindow), // zero-config guard (D-62)
	}
	if q.Alerts != nil {
		am := q.Alerts
		r.OnDrift = func(a reconcile.DriftAlert) { // (e) money incident → alert sink
			am.NotifyDrift(context.Background(), a.OrgID, a.ResourceKey, a.Source, a.UnsettleableLive)
		}
	}
	// D-75 — the periodic re-verification RIDES this loop (never its own worker: one more
	// loop is one more thing that can be dead, D-50). Every boot guard we own verified the
	// world once and then trusted it forever; this is what notices when it changes.
	//
	// NOTE (framed, not hidden): with no Redis counter store, or no reconciler, nothing
	// re-verifies — but BOTH of those states ALREADY degrade Healthy() (FloatStore=memory is
	// loudly recorded; Reconciler=OFF fails the probe), so the deployment is not silently
	// trusting a stale verdict; it is loudly degraded for a broader reason.
	if q.redisProbe != nil {
		r.Preflight = q.makeRevalidator(q.redisProbe, cfg)
	}
	rc, cancel := context.WithCancel(context.Background())
	q.closeFns = append(q.closeFns, func() error { cancel(); return nil })
	go r.RunLoop(rc, interval, opts.ReconcileOrgs)
	cap.Reconciler = "ON"
	return r
}

func billingChainError(weakest string) error {
	return fmt.Errorf(
		"quota.Setup: enable_paid but the billing chain is SEVERED — %s. A gate on one link is "+
			"satisfiable while the chain is broken; the guarantee is 'usage reaches billing', not 'a component "+
			"exists' (D-56). A paid service that starts and bills nothing is QB-01 through the front door. Fix "+
			"the missing link, or set outbox.allow_ephemeral=true to start with billing DISABLED (dev only)", weakest)
}

// resolveDurableOutbox picks a durable outbox store: DDB (self-provisioned)
// preferred, else Redis under a durability machine-check, else none. A DDB
// self-provision is attempted only when there is an AWS signal (a Go process
// with no AWS config cannot self-provision anyway — Go-native divergence from
// Python's unconditional attempt; framed in the leg artifact).
func resolveDurableOutbox(ctx context.Context, cfg *config.Config, redisClient *redis.Client) (outbox.Store, bool, string) {
	storePref := strings.ToLower(cfg.Outbox.Store)
	if storePref != "redis" && ddbSignalPresent(cfg) {
		attempts := cfg.Outbox.ProvisionRetryAttempts
		if attempts <= 0 {
			attempts = 3
		}
		table := cfg.Outbox.DDBTable
		if table == "" {
			table = "ab0t_quota_outbox"
		}
		for i := 1; i <= attempts; i++ {
			client, err := newDDBClient(ctx, cfg.Storage)
			if err == nil {
				ds := outbox.NewDDBStore(client, table)
				if eerr := ds.EnsureTable(ctx, 60*time.Second); eerr == nil {
					// D-76 — the outbox holds MONEY events nothing can reconstruct, and until
					// now nothing checked the table it lives in. Verify it (ACTIVE, GSIs ACTIVE,
					// TTL on the attribute we actually write, PITR on) before we trust it.
					pitrConfirmed := cfg.Storage.DDBPITRConfirmed || ddbguard.EnvPITRConfirmed()
					value, fatal, warn := ddbguard.VerifyTable(ctx, client, table, "ttl", pitrConfirmed)
					if warn != "" {
						slog.Warn("DDB preflight WARNING (D-76)", "capability", "ddb_outbox", "detail", warn)
					}
					if fatal != "" {
						slog.Error("DDB STORE UNSAFE (D-76)", "capability", "ddb_outbox", "detail", fatal)
						return nil, false, "DDB UNSAFE (" + fatal + ")"
					}
					slog.Info("DDB preflight verified (D-76)", "capability", "ddb_outbox", "detail", value)
					return ds, true, "DDB"
				} else {
					slog.Warn("outbox DDB provision failed", "attempt", i, "err", eerr)
				}
			}
			if i < attempts {
				time.Sleep(time.Duration(i) * 300 * time.Millisecond) // bounded backoff
			}
		}
		slog.Error("outbox DDB unavailable after retries — treating as ABSENT")
	}
	if redisClient != nil {
		durable, detail := outbox.CheckRedisDurability(ctx, redisClient, cfg.Outbox.RedisDurabilityConfirmed)
		return outbox.NewRedisStore(redisClient, "outbox"), durable, detail
	}
	return nil, false, "none"
}

// ddbSignalPresent decides whether the consumer DECLARED DynamoDB intent —
// from config only (GO-08). AWS_ENDPOINT_URL is an SDK endpoint override,
// never an intent signal: a LocalStack endpoint set for another service must
// not change whether ab0t-quota believes DynamoDB is configured.
func ddbSignalPresent(cfg *config.Config) bool {
	return strings.ToLower(cfg.Outbox.Store) == "ddb" ||
		cfg.Storage.DynamoDBTable != "" ||
		cfg.Outbox.DDBTable != ""
}

// Capabilities returns the snapshot.
func (q *Quota) Capabilities() Capabilities {
	q.capMu.RLock()
	defer q.capMu.RUnlock()
	return q.capability
}

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

// connectDeclaredRedis — the D-2 boot gate (T-13; Python parity:
// _gate_redis_reachable). One classified connect+PING per attempt; the retry
// budget applies to the UNREACHABLE kind only (auth does not heal by
// waiting); 0.5s→5s capped backoff; the verdict lands on Capabilities before
// any refusal path returns.
// The D-2 retry numbers are a CROSS-RUNTIME CONTRACT, declared in
// ST-RESOLVE-1's retry_contract (conformance/scenarios.json) and bound by
// TestSTResolve1_Clause7 — change them there first, or the binding goes RED
// (T-27: agreement by having-diffed-the-other-runtime is PAR-01 rebuilt).
const (
	defaultConnectRetrySeconds = 30.0
	connectBackoffInitial      = 500 * time.Millisecond
	connectBackoffCap          = 5 * time.Second
)

func connectDeclaredRedis(ctx context.Context, url, password, source string,
	retrySeconds *float64, cap *Capabilities) (*redis.Client, error) {
	budget := defaultConnectRetrySeconds
	if retrySeconds != nil {
		budget = *retrySeconds
		if budget < 0 {
			budget = 0
		}
	}
	deadline := time.Now().Add(time.Duration(budget * float64(time.Second)))
	delay := connectBackoffInitial
	for {
		client, err := newRedisClient(ctx, url, password)
		if err == nil {
			cap.RedisReachable = redisguard.RedisReachableOK
			slog.Info("redis reachability verified (D-2/GT-T1): PING ok", "source", source)
			return client, nil
		}
		kind := redisguard.ClassifyRedisError(err)
		if kind == "" {
			kind = "error"
		}
		if kind == "unreachable" && time.Now().Add(delay).Before(deadline) {
			slog.Warn("declared Redis unreachable — retrying within the D-2 budget",
				"budget_seconds", budget, "err", err.Error())
			select {
			case <-ctx.Done():
				// context gone: fall through to the refusal
			case <-time.After(delay):
				delay = min(delay*2, connectBackoffCap)
				continue
			}
		}
		cap.RedisReachable = TopologyProbeFailed + " [" + kind + ": " + err.Error() + "]"
		rerr := redisguard.ReachabilityError(kind, err.Error(), config.RedactURL(url), source, budget)
		slog.Error("DECLARED REDIS UNREACHABLE/UNAUTHENTICATED (D-2) — refusing to start",
			"kind", kind, "err", err.Error())
		return nil, rerr
	}
}

// newRedisClient builds + pings a go-redis client from the RESOLVED store
// URL (TASK P5.1 / GO-01: resolution happens once, in Setup). A ping failure
// returns an error so the caller degrades loudly to in-memory rather than
// handing back a client that silently fails every op. redis_password
// overrides any password in the URL.
func newRedisClient(ctx context.Context, redisURL, redisPassword string) (*redis.Client, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis_url: %w", err)
	}
	if redisPassword != "" {
		// D-5(a) / ST-RESOLVE-1 clause 5: the separately-declared field wins
		// over URL userinfo. When both are set and DIFFER, warn naming the
		// SOURCES (never the values) — that assembled pair is exactly the
		// "credentials nobody intended" shape of ENV-02.
		if opt.Password != "" && opt.Password != redisPassword {
			slog.Warn("redis credentials: storage.redis_password and the URL-embedded password " +
				"are BOTH set and differ — the declared field storage.redis_password wins over " +
				"the URL userinfo (D-5(a)). Remove the stale one.")
		}
		opt.Password = redisPassword
	}
	client := redis.NewClient(opt)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return client, nil
}

// resolveAWSRegion applies the GO-02 contract shared by every AWS client:
// a DECLARED region wins; otherwise defer to the AWS SDK's own chain
// (env/profile/IMDS — platform contract) and LOG what it resolved; a region
// nobody resolves is a typed config error naming the config key and
// AWS_REGION — never an invented default. (The two old defaults, us-west-2
// and us-east-1, DISAGREED: which region a table landed in depended on which
// code path provisioned it.)
func resolveAWSRegion(ctx context.Context, declared, configKey, code string) (aws.Config, error) {
	var loadOpts []func(*awsconfig.LoadOptions) error
	if declared != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(declared))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load aws config: %w", err)
	}
	if awsCfg.Region == "" {
		return aws.Config{}, config.NewUndeclaredError(config.Spec{
			Name:      "AWS region (" + configKey + ")",
			ConfigKey: configKey,
			Env:       []string{"AWS_REGION"},
			Code:      code,
			Previously: "earlier versions invented a region here (us-west-2 in the DynamoDB " +
				"client, us-east-1 in the SNS client — they disagreed, so which region a table " +
				"landed in depended on which code path provisioned it). This version refuses instead.",
			Remedy: "set " + configKey + " in quota-config.json, or let the platform provide " +
				"AWS_REGION (ECS/EKS/IRSA inject it; the AWS SDK resolves it by its own contract).",
			Docs: "CONSUMING.md#prerequisites · verify before deploy: quotactl capabilities --config quota-config.json",
		}, "key absent; the AWS SDK chain resolved no region")
	}
	source := "aws-sdk default chain"
	if declared != "" {
		source = "config " + configKey
	}
	slog.Info("aws region resolved", "region", awsCfg.Region, "for", configKey, "source", source)
	return awsCfg, nil
}

// newDDBClient builds an aws-sdk DynamoDB client for the handler ledger
// (TASK P5.1). Region per resolveAWSRegion (GO-02: declared, SDK-resolved,
// or a typed error — never an invented default). AWS_ENDPOINT_URL overrides
// the endpoint (DynamoDB Local / LocalStack — SDK contract, endpoint only,
// never an intent signal). Credentials come from the default chain.
func newDDBClient(ctx context.Context, sc config.StorageConfig) (*dynamodb.Client, error) {
	awsCfg, err := resolveAWSRegion(ctx, sc.DynamoDBRegion, "storage.dynamodb_region", "QUOTA-CFG-009")
	if err != nil {
		return nil, err
	}
	var opts []func(*dynamodb.Options)
	if ep := os.Getenv("AWS_ENDPOINT_URL"); ep != "" {
		opts = append(opts, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(ep) })
	}
	return dynamodb.NewFromConfig(awsCfg, opts...), nil
}

// newSNSClient builds an aws-sdk SNS client for the settlement publisher
// (D-56). Region per resolveAWSRegion (GO-02); AWS_ENDPOINT_URL overrides
// the endpoint (LocalStack).
func newSNSClient(ctx context.Context, oc config.OutboxConfig) (*sns.Client, error) {
	awsCfg, err := resolveAWSRegion(ctx, oc.SNSRegion, "outbox.sns_region", "QUOTA-CFG-010")
	if err != nil {
		return nil, err
	}
	var opts []func(*sns.Options)
	if ep := os.Getenv("AWS_ENDPOINT_URL"); ep != "" {
		opts = append(opts, func(o *sns.Options) { o.BaseEndpoint = aws.String(ep) })
	}
	return sns.NewFromConfig(awsCfg, opts...), nil
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
		"redis_reachable", c.RedisReachable, // D-2 / GO-10
		"redis_topology", c.RedisTopology, // D-71
		"counter_eviction_policy", c.CounterEvictionPolicy, // D-72
		"redis_scripting", c.RedisScripting, // D-73
		"redis_version", c.RedisVersion, // D-74
		"memory_headroom", c.MemoryHeadroom, // D-77
		"counter_evictions_observed", c.CounterEvictionsObserved, // D-80
		"redis_persist_status", c.RedisPersistStatus, // D-81
		"preflight_reverification", c.PreflightReverification, // D-79
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
