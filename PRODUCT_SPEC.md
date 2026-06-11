# ab0t-quota-go — Product Specification

**Version target:** 0.1.0 (initial port; parity with Python ab0t-quota 0.5.2)
**Go version:** 1.22+
**Module path:** `github.com/ab0t-com/ab0t-quota-go`
**Status:** Specification, revision 2 (post-review). Treat this doc as the source of truth for the first implementation pass; deviations must amend this file.

## Revision log

- **rev 1** (2026-06-11) — initial spec
- **rev 2** (2026-06-11) — incorporates findings from `review_20260611.md` and `review_20260611_addendum.md`. Inline edits below; substantive amendments summarized at [§14 Amendments accepted](#14-amendments-accepted) for traceability.

## Known upstream bugs (do NOT port; track separately)

These were found during spec review. The Go port must NOT inherit any of them:

| # | Repo | Bug | Evidence |
|---|---|---|---|
| 1 | ab0t-quota (Python) | `setup.py:938` `NameError: provider` — default signup-credit handler never registers; swallowed by `except`. v0.5.2 "zero-code signup credit" broken for any consumer without their own handler. | review C8 |
| 2 | ab0t-quota (Python) | `resolve_billing_org` calls nonexistent `GET /users/{user_id}/organizations` — workspace resolution silently dead in prod; always falls back to event org_id. | review C5 |
| 3 | **auth service** | v1 webhook delivery signs canonical JSON (`sort_keys=True, separators=(',', ':')`) but sends `aiohttp` re-serialized bytes (defaults: spaces, no sort). HMAC can never verify on v1 path. System "works" via the v2 publisher using `X-Webhook-Signature` over exact bytes sent. | addendum A1 |
| 4 | ab0t-quota (Python) | dead config knobs: `enforcement.shadow_mode`, `enforcement.global_kill_switch`, `alerts.dispatchers[]` — present in example/real configs, read nowhere. | review M10, addendum A7 |
| 5 | ab0t-quota (Python) | public Redis accessor never implemented; consumers reach into `_ctx._redis` defensively (sandbox-platform does today). | review §5.2 |

Tickets to file against the upstream repos. The Go port's behavior in these areas is specified independently below (search for "BUG-#" markers).

---

## Design principles (non-negotiable)

1. **Drop-in.** A new consumer should reach a working state with `go get`, one `Setup` call, two env vars, and one JSON config. No required boilerplate.
2. **Contract parity with Python.** Same JSON config schema, same Redis key shapes, same DynamoDB item shapes, same wire-level HTTP routes, same CLI subcommands. A Python deployment's data must be readable by a Go deployment and vice versa.
3. **Idiomatic Go.** Interfaces over abstract base classes. Functional options. Errors with `fmt.Errorf("%w", ...)`. `context.Context` on every external call. No global mutable state outside the package-level handler registry (which mirrors Python's module-level registry by design).
4. **Minimal deps.** Standard library wherever possible. Third-party deps require justification (listed in [Dependencies](#dependencies) below).
5. **Test-first.** Each package has ≥ 80% statement coverage. Storage backends share a conformance suite. End-to-end tests use `httptest.Server`.
6. **No magic.** Reflection only where unavoidable (JSON unmarshaling, decimal). No code generation. No init() side effects except the package-level registry mutex.

---

## Repository layout

```
ab0t-quota-go/
├── README.md                          (intent + quick start — already written)
├── PRODUCT_SPEC.md                    (this file)
├── back_references.md                 (Python files, services, OpenAPI specs — already written)
├── LICENSE                            (Proprietary, matches ab0t-quota Python)
├── go.mod
├── go.sum
├── .golangci.yml                      (linter config)
├── .github/workflows/ci.yml           (Go 1.22+, lint, test, race, coverage)
│
├── quota/                             [PACKAGE: top-level public API]
│   ├── doc.go                         package doc, godoc-friendly overview
│   ├── setup.go                       Setup, Config, QuotaContext (public)
│   ├── quota_context.go               QuotaContext methods: Mount, Close, Check, etc.
│   ├── lifespan.go                    background workers (snapshot, heartbeat, alerts)
│   └── setup_test.go
│
├── config/                            [PACKAGE: JSON config schema + loader]
│   ├── doc.go
│   ├── config.go                      Config root struct
│   ├── tier.go                        Tier, TierLimits, CreditGrant, CreditTrigger, CreditLifecycle, CreditDestination
│   ├── resource.go                    ResourceDef, ResourceBundle, CounterType
│   ├── alerts.go                      AlertsConfig, AlertThreshold
│   ├── enforcement.go                 EnforcementConfig (enabled, shadow_mode)
│   ├── storage.go                     StorageConfig (redis_url, ddb_table)
│   ├── tier_provider.go               TierProviderConfig (type: jwt|mesh|static)
│   ├── billing_integration.go         BillingIntegrationConfig
│   ├── reconciliation.go              ReconciliationConfig
│   ├── load.go                        LoadConfig(path) (*Config, error); search paths
│   ├── decimal.go                     wrapped shopspring/decimal — money serialization
│   ├── testdata/                      sample configs for tests
│   │   ├── minimal.json
│   │   ├── full.json
│   │   └── credit_grant_policies.json
│   └── config_test.go
│
├── engine/                            [PACKAGE: quota engine]
│   ├── doc.go
│   ├── engine.go                      Engine struct + New() + Check, Increment, Decrement, BatchCheck, Usage, Feature
│   ├── result.go                      QuotaResult, QuotaUsage, QuotaError
│   ├── tier_resolution.go             tier resolution + caching
│   ├── override_loader.go             OverrideLoader hook (used by persistence layer)
│   └── engine_test.go
│
├── counters/                          [PACKAGE: counter implementations]
│   ├── doc.go
│   ├── counter.go                     Counter interface — values/limits are float64 (INCRBYFLOAT semantics)
│   ├── gauge.go                       GaugeCounter — INCRBYFLOAT on `quota:{org}:{resource}:gauge`; per-user partition key `…:gauge:user:{user_id}`; floor-at-zero via overwrite (NOT Lua — Python uses pipeline of plain ops)
│   ├── rate.go                        RateCounter — Redis sorted set `quota:{org}:{resource}:rate`; ZADD/ZREMRANGEBYSCORE/ZCARD; computes seconds-until-slot on denial
│   ├── accumulator.go                 AccumulatorCounter — INCRBYFLOAT on `quota:{org}:{resource}:acc:{period_key}`; calendar-aligned period keys: `%Y-%m-%dT%H` (hour), `%Y-%m-%d` (day), `{year}-W{week:02d}` (week), `%Y-%m` (month)
│   ├── idempotency.go                 Per-counter idempotency: SET NX EX 86400 on `quota:{org}:{resource}:idem:{[user_id:]key}`
│   ├── factory.go                     CounterFactory mapping counter_type→impl
│   └── counter_test.go
│   # NOTE: Python counters use PLAIN Redis ops (INCRBYFLOAT, ZADD, etc.) — no Lua, no SCRIPT LOAD, no EVALSHA. Earlier spec rev claimed Lua parity; that was wrong. Go must match the plain-ops shape so shared-Redis interop works.
│
├── providers/                         [PACKAGE: tier providers]
│   ├── doc.go
│   ├── provider.go                    TierProvider interface
│   ├── jwt.go                         JWTTierProvider (zero-latency claim read)
│   ├── mesh.go                        MeshTierProvider (calls billing-service /tier)
│   ├── static.go                      StaticTierProvider (always returns "free")
│   ├── cache.go                       TTL-cache wrapper applicable to any provider
│   └── provider_test.go
│
├── registry/                          [PACKAGE: resource registry]
│   ├── doc.go
│   ├── registry.go                    ResourceRegistry + bundle expansion
│   └── registry_test.go
│
├── messages/                          [PACKAGE: human-readable 429 messages]
│   ├── doc.go
│   ├── builder.go                     MessageBuilder — tier-aware upgrade hints
│   └── builder_test.go
│
├── middleware/                        [PACKAGE: HTTP middleware]
│   ├── doc.go
│   ├── guard.go                       Guard (http.Handler wrapper for rate-limit on api.requests_per_hour)
│   ├── headers.go                     X-Quota-Limit / X-Quota-Remaining header writer
│   └── guard_test.go
│
├── alerts/                            [PACKAGE: threshold alerts]
│   ├── doc.go
│   ├── manager.go                     AlertManager
│   ├── dispatcher.go                  AlertDispatcher interface
│   ├── log.go                         LogAlertDispatcher
│   ├── webhook.go                     WebhookAlertDispatcher
│   └── manager_test.go
│
├── persistence/                       [PACKAGE: DynamoDB-backed durable state]
│   ├── doc.go
│   ├── store.go                       QuotaStore interface
│   ├── dynamodb.go                    DDBQuotaStore (single-table design, GSI1 for cross-org)
│   ├── seed.go                        SeedRedisFromDDB (cold-start restore)
│   ├── snapshot_worker.go             SnapshotWorker (5-min interval)
│   └── store_test.go
│
├── billing/                           [PACKAGE: billing-service client + proxy]
│   ├── doc.go
│   ├── client.go                      BillingClient: GetBalance, Reserve, Commit, Refund, GetUsage, GetTransactions, ApplyPromotionalCredit, ApplyCreditGrant, ResetSubscriptionCredit, SetTier
│   ├── models.go                      Balance, Reservation, UsageRecord, Transaction
│   ├── decimal.go                     re-export config.Decimal
│   ├── router.go                      Router mounting /api/billing/* proxy routes (auth_reader / auth_admin pattern from Python)
│   ├── lifecycle_emitter.go           LifecycleEmitter (auto-records cost on resource stop)
│   ├── heartbeat.go                   HeartbeatMonitor (emits synthetic stop events for stale resources)
│   ├── auth_helpers.go                MakeReaderDep / MakeAdminDep
│   └── client_test.go
│
├── payment/                           [PACKAGE: payment-service client + proxy]
│   ├── doc.go
│   ├── client.go                      PaymentClient: GetPlans, CheckoutInit, CheckoutComplete, GetSubscription, CancelSubscription, GetInvoices, etc.
│   ├── models.go                      Plan, Subscription, Invoice
│   ├── router.go                      Router mounting /api/payments/* proxy routes
│   └── client_test.go
│
├── mesh/                              [PACKAGE: mesh URL + auth helpers]
│   ├── doc.go
│   ├── urls.go                        URL resolver (env override → defaults)
│   ├── auth.go                        APIKey header builder, JWT extractor from request
│   └── mesh_test.go
│
├── authevents/                        [PACKAGE: auth-event registry + receiver + primitives]
│   ├── doc.go
│   ├── event.go                       Event struct (event_type, event_id, occurred_at, data)
│   ├── registry.go                    HandlerFunc type, OnAuthEvent, RegisterHandler, UnregisterHandler, RegisteredEventTypes, ClearHandlers
│   ├── receiver.go                    MakeRouter (returns http.Handler mounted at /_webhooks/auth)
│   ├── hmac.go                        VerifyHMAC (X-Event-Signature parsing)
│   ├── subscribe.go                   SubscribeOnStartup; ResolveOrgIDFromSlug
│   ├── primitives.go                  ComposeCreditDedupKey, ResolveBillingOrg, GrantInitialCreditForUser
│   ├── pinstore.go                    PinStore interface
│   ├── pinstore_ddb.go                DDBPinStore
│   ├── pinstore_redis.go              RedisPinStore
│   ├── pinstore_memory.go             MemoryPinStore
│   ├── default_handler.go             BuildDefaultCreditGrantHandler (auto-registered when enable_paid=true)
│   └── authevents_test.go
│
├── handlerledger/                     [PACKAGE: idempotency + replay framework]
│   ├── doc.go
│   ├── ledger.go                      LedgerStore interface, LedgerRow, LedgerStatus, AttemptResult
│   ├── memory.go                      InMemoryLedgerStore
│   ├── redis.go                       RedisLedgerStore (72h TTL)
│   ├── dynamodb.go                    DDBLedgerStore (90d TTL via DDB TTL attribute)
│   ├── decorator.go                   Idempotent func + Context (the HandlerContext analog)
│   ├── retry.go                       Retry loop with exponential backoff
│   ├── outcomes.go                    SkipOutcome / SuccessOutcome sentinel types
│   ├── autoselect.go                  AutoSelectStore — picks DDB > Redis > InMemory
│   ├── conformance_test.go            shared test suite for all 3 backends
│   └── decorator_test.go
│
├── cmd/
│   └── quotactl/                      [BINARY: CLI]
│       ├── main.go                    cobra root command
│       ├── subscribe_events.go        subscribe-events subcommand
│       ├── events.go                  events subcommand (--user-id, --status, --since)
│       ├── replay.go                  replay subcommand (--handler --event-id)
│       ├── backfill.go                backfill subcommand
│       ├── delete_user.go             delete-user subcommand (--confirm)
│       ├── store_from_env.go          shared helper: build LedgerStore from env vars
│       └── main_test.go               e2e CLI tests using testscript
│
├── examples/                          [EXAMPLES: runnable consumer code]
│   ├── basic/main.go                  setup_quota equivalent — 30 lines
│   ├── with_auth_events/main.go       register handler — 50 lines
│   ├── with_idempotent/main.go        full ledger + retry — 80 lines
│   └── bridge_mode/main.go            HTTPS-only, no Redis — minimal
│
├── internal/
│   ├── jsonutil/
│   │   └── decimal_decode.go          Decimal-as-string JSON helpers
│   ├── httpx/
│   │   └── client.go                  retrying HTTP client used by all callers (mesh URLs, billing, payment, auth)
│   ├── testutil/
│   │   ├── fake_redis.go              in-process fake redis (used by tests)
│   │   ├── fake_ddb.go                in-process fake DynamoDB
│   │   └── fake_auth.go               in-process fake auth/events HTTP server
│   └── memorymutex/
│       └── locker.go                  TTLLockManager equivalent (bounded LRU + TTL eviction)
│
└── testdata/
    ├── quota-config.example.json
    └── webhook_event_samples/
        ├── user.registered.json
        ├── user.login.json
        └── org.created.json
```

---

## Public API — file by file

### `quota/setup.go`

Top-level entry point. Mirrors Python's `setup_quota`.

```go
// Config is the typed input to Setup. Fields with zero values fall back
// to env vars (documented per-field). Required: ConfigPath (or
// ConfigPathEnv), and either an external Redis URL / DDB client.
type Config struct {
    // Path to quota-config.json. If empty, search order:
    // - $QUOTA_CONFIG_PATH
    // - ./quota-config.json
    // - ./config/quota-config.json
    ConfigPath string

    // Mesh credentials. Default: env vars AB0T_MESH_API_KEY / AB0T_CONSUMER_ORG_ID.
    MeshAPIKey  string
    ConsumerOrg string

    // Auth-event webhook config. If WebhookSecret is empty, the receiver
    // is NOT mounted (no /_webhooks/auth route).
    WebhookSecret      string
    WebhookPublicURL   string  // for auto-subscribe
    AuthURL            string  // default env AB0T_AUTH_AUTH_URL
    AuthAdminToken     string  // for auto-subscribe
    WatchOrgSlug       string  // event filter

    // Storage backends. If nil, autoselected from env. Pass explicit
    // instances for tests or to override defaults.
    Redis        RedisClient            // *redis.Client (go-redis) or compatible interface
    DDBClient    DDBClient              // aws-sdk-go-v2 DynamoDB client
    LedgerStore  handlerledger.LedgerStore  // override autoselect
    QuotaStore   persistence.QuotaStore     // override DDB-backed quota state

    // Feature flags. Default true unless documented.
    EnableRateLimitMiddleware bool   // default true
    EnableQuotaAPI            bool   // default true (mounts /api/quotas/*)
    EnablePaid                bool   // default false (mounts /api/billing/*, /api/payments/*)

    // Hooks
    OnReady func(*QuotaContext)
}

// Setup builds and returns a QuotaContext. Long-running workers
// (snapshot, heartbeat, alerts) start before Setup returns and stop
// when QuotaContext.Close is called. Setup is safe to call once per
// process; calling twice returns an error.
func Setup(ctx context.Context, cfg Config) (*QuotaContext, error)
```

### `quota/quota_context.go`

```go
type QuotaContext struct {
    // unexported fields
}

// Mount registers the library's HTTP routes onto the given mux at the
// given prefix (typically "/api"). After Mount:
//   - /api/quotas/usage, /api/quotas/tiers, /api/quotas/check/{key}
//   - /api/quotas/_webhooks/auth (if WebhookSecret set)
//   - /api/billing/*, /api/payments/* (if EnablePaid)
func (q *QuotaContext) Mount(mux Mux, prefix string)

// Check enforces a single resource. Returns nil if allowed, *QuotaError
// (which implements error) if denied. Sets headers on the response if a
// ResponseWriter is in the context.
func (q *QuotaContext) Check(ctx context.Context, orgID, resourceKey string, opts ...CheckOption) error

// CheckBundle expands a bundle (e.g. "widget") into its component
// resource_keys and checks each.
func (q *QuotaContext) CheckBundle(ctx context.Context, orgID, bundleName string, opts ...CheckOption) error

// Increment / Decrement / IncrementBundle / DecrementBundle: mirror Check.
func (q *QuotaContext) Increment(ctx context.Context, orgID, resourceKey string, opts ...IncrementOption) error
func (q *QuotaContext) IncrementBundle(ctx context.Context, orgID, bundleName string, opts ...IncrementOption) error

// Usage returns the current org's tier + per-resource usage.
func (q *QuotaContext) Usage(ctx context.Context, orgID string) (*Usage, error)

// Feature returns true if the org's tier has the named feature flag.
func (q *QuotaContext) Feature(ctx context.Context, orgID, feature string) (bool, error)

// Engine exposes the underlying engine for advanced callers.
func (q *QuotaContext) Engine() *engine.Engine

// Close stops background workers and releases resources. Safe to call
// from a defer at the top of main().
func (q *QuotaContext) Close() error

// Options pattern
type CheckOption func(*checkOpts)
type IncrementOption func(*incrementOpts)
func WithUserID(uid string) CheckOption          // per-user sub-quota
func WithInstanceType(t string) CheckOption     // GPU detection
func WithCount(n int) IncrementOption           // batch increment
```

### `config/config.go`

```go
type Config struct {
    Schema             string                  `json:"$schema,omitempty"`
    Comment            string                  `json:"$comment,omitempty"`
    ServiceName        string                  `json:"service_name,omitempty"`
    Tiers              []Tier                  `json:"tiers"`
    Resources          []ResourceDef           `json:"resources"`
    ResourceBundles    map[string][]string     `json:"resource_bundles"`
    TierProvider       TierProviderConfig      `json:"tier_provider"`
    Storage            StorageConfig           `json:"storage"`
    Alerts             AlertsConfig            `json:"alerts"`
    Enforcement        EnforcementConfig       `json:"enforcement"`
    BillingIntegration BillingIntegrationConfig `json:"billing_integration"`
    Reconciliation     ReconciliationConfig    `json:"reconciliation,omitempty"`
}

func LoadConfig(path string) (*Config, error)
func MustLoadConfig(path string) *Config
```

### `config/tier.go`

```go
type Tier struct {
    TierID                  string                       `json:"tier_id"`
    DisplayName             string                       `json:"display_name"`
    Description             string                       `json:"description"`
    SortOrder               int                          `json:"sort_order"`
    Features                []string                     `json:"features"`
    Limits                  map[string]TierLimit         `json:"limits"`
    UpgradeURL              string                       `json:"upgrade_url,omitempty"`
    InitialCredit           *Decimal                     `json:"initial_credit,omitempty"`  // legacy
    CreditGrant             *CreditGrant                 `json:"credit_grant,omitempty"`    // new schema
    DefaultPerUserFraction  float64                      `json:"default_per_user_fraction,omitempty"`
}

type TierLimit struct {
    Limit *int64 `json:"-"`  // custom UnmarshalJSON accepts {"limit": N} or N or null
}

type CreditTrigger string
const (
    CreditTriggerSignup            CreditTrigger = "signup"
    CreditTriggerSubscriptionStart CreditTrigger = "subscription_start"
    // (others — full enum mirrors Python)
)

type CreditLifecycle string
const (
    CreditLifecyclePersistent       CreditLifecycle = "persistent"
    CreditLifecycleUseItOrLoseIt    CreditLifecycle = "use_it_or_lose_it"
    CreditLifecycleRolloverCapped   CreditLifecycle = "rollover_capped"
)

type CreditDestination string
const (
    CreditDestCreditBalance       CreditDestination = "credit_balance"
    CreditDestSubscriptionCredit  CreditDestination = "subscription_credit"
)

type DedupPolicy string
const (
    DedupPerUserPerTier DedupPolicy = "per_user_per_tier"  // default
    DedupPerOrgPerTier  DedupPolicy = "per_org_per_tier"
    DedupPerUserGlobal  DedupPolicy = "per_user_global"
    DedupPerOrgGlobal   DedupPolicy = "per_org_global"
)

type CreditGrant struct {
    Trigger             CreditTrigger     `json:"trigger"`
    AmountPerPeriod     Decimal           `json:"amount_per_period"`
    Currency            string            `json:"currency,omitempty"`
    Lifecycle           CreditLifecycle   `json:"lifecycle,omitempty"`
    RolloverMaxPeriods  *int              `json:"rollover_max_periods,omitempty"`
    Destination         CreditDestination `json:"destination,omitempty"`
    ResetOnDowngrade    bool              `json:"reset_on_downgrade"`
    ResetOnUpgrade      bool              `json:"reset_on_upgrade"`
    Dedup               DedupPolicy       `json:"dedup,omitempty"`  // default per_user_per_tier
}
```

### `config/decimal.go`

```go
// Decimal wraps shopspring/decimal so we serialize money as JSON strings
// (matching the Python lib's behavior). Required for billing/payment.
type Decimal struct {
    decimal.Decimal
}

func (d Decimal) MarshalJSON() ([]byte, error)   // emit "10.00"
func (d *Decimal) UnmarshalJSON(b []byte) error  // accept "10.00", "10", 10, 10.0
func NewDecimal(s string) Decimal
```

### `engine/engine.go`

```go
type Engine struct {
    // unexported
}

func New(ctx context.Context, cfg *config.Config, redis RedisClient, provider providers.TierProvider) (*Engine, error)

// Check returns nil on allowed, *engine.QuotaResult{Denied: true, ...}
// wrapped in QuotaError on denial.
func (e *Engine) Check(ctx context.Context, req CheckRequest) (*QuotaResult, error)

// BatchCheck checks multiple resource_keys in one Redis MULTI/EXEC.
func (e *Engine) BatchCheck(ctx context.Context, reqs []CheckRequest) ([]*QuotaResult, error)

func (e *Engine) Increment(ctx context.Context, req IncrementRequest) (*QuotaResult, error)
func (e *Engine) Decrement(ctx context.Context, req DecrementRequest) error
func (e *Engine) Usage(ctx context.Context, orgID string) (*UsageResponse, error)
func (e *Engine) Feature(ctx context.Context, orgID, feature string) (bool, error)
```

### `counters/counter.go`

Values, deltas, and limits are `float64` — matches Python's `INCRBYFLOAT`-based system end-to-end (`sandbox.monthly_cost` is fractional USD with `precision: 2`).

```go
type Counter interface {
    Check(ctx context.Context, key string, limit float64) (current float64, allowed bool, err error)
    Increment(ctx context.Context, key string, delta float64) (current float64, err error)
    Decrement(ctx context.Context, key string, delta float64) error
}
```

`IncrementOption` exposes `WithDelta(float64)` (NOT `WithCount(int)`).

Per-impl files use plain Redis ops (no Lua). Shared-Redis interop with the Python lib depends on:
- Exact key shapes from the parity matrix (§7)
- Number formatting compatible with Redis's INCRBYFLOAT response (Go: `strconv.FormatFloat(v, 'f', -1, 64)`)
- Floor-at-zero on gauge decrement via overwrite with conditional WATCH/MULTI (Python's approach)

### `providers/provider.go`

```go
type TierProvider interface {
    GetTier(ctx context.Context, orgID string) (string, error)
}

// JWTTierProvider reads from a context-attached claim map.
type JWTTierProvider struct { ClaimKey string }
func (p *JWTTierProvider) GetTier(ctx context.Context, orgID string) (string, error)

// MeshTierProvider calls billing-service /billing/{org}/tier.
type MeshTierProvider struct {
    Client     *billing.BillingClient
    DefaultTier string
}

// Cached wraps any provider with a TTL cache.
func Cached(p TierProvider, ttl time.Duration) TierProvider
```

### `authevents/registry.go`

```go
// Handler is the registry's element type. Plain handlers go through a
// HandlerFunc adapter; @idempotent wrappers implement Handler directly
// as *handlerledger.IdempotentHandler, so the dispatcher can type-switch
// without reflection.
type Handler interface {
    Handle(ctx context.Context, event Event) error
}

// HandlerFunc is the convenience adapter for plain handlers.
type HandlerFunc func(ctx context.Context, event Event) error
func (f HandlerFunc) Handle(ctx context.Context, e Event) error { return f(ctx, e) }

// OnAuthEvent registers a Handler (or HandlerFunc) for event_type.
// Idempotent: registering the same Handler twice is a no-op (deduped
// by pointer identity for *IdempotentHandler, by reflect.ValueOf(...).Pointer()
// for HandlerFunc).
func OnAuthEvent(eventType string, h Handler) Handler

// Registration helpers
func RegisterHandler(eventType string, h Handler)
func UnregisterHandler(eventType string, h Handler) bool

// NewRegistry returns a non-singleton registry — used by tests and
// multi-tenant binaries that need isolation. Package-level OnAuthEvent
// etc. delegate to a default Registry.
func NewRegistry() *Registry
type Registry struct{ /* unexported */ }
func (r *Registry) OnAuthEvent(eventType string, h Handler) Handler
func (r *Registry) Handlers(eventType string) []Handler
func (r *Registry) Clear()

// RegisteredEventTypes returns event types with at least one handler.
// Used by SubscribeOnStartup to know what to subscribe to.
func RegisteredEventTypes() []string

// ClearHandlers — test-only helper. Don't call in production.
func ClearHandlers()
```

**Concurrency:** handlers within a single delivery dispatch sequentially in registration order (matches Python's single-loop semantics). The registry itself is safe for concurrent OnAuthEvent calls.

### `authevents/event.go`

```go
type Event struct {
    EventType  string         `json:"event_type"`
    EventID    string         `json:"event_id"`
    OccurredAt time.Time      `json:"occurred_at"`
    Data       map[string]any `json:"data"`
    // Raw is the original JSON for replay and HMAC verification.
    Raw json.RawMessage `json:"-"`
}

// Convenience extractors for the most-used fields. All return "" if absent.
func (e Event) UserID() string
func (e Event) OrgID() string
func (e Event) Email() string
```

### `authevents/receiver.go`

```go
type ReceiverConfig struct {
    Secret      string                  // required; HMAC secret
    LedgerStore handlerledger.LedgerStore // optional; enables @idempotent dispatch
}

// MakeRouter returns an http.Handler implementing the same wire-level
// behavior as Python's make_router. Mount at /api/quotas/_webhooks/auth.
// 401 on missing/bad HMAC, 400 on bad JSON, 200 with {"status":"ok","ran":N}
// on dispatch. Handler errors are caught + logged; auth always sees 200.
func MakeRouter(cfg ReceiverConfig) http.Handler
```

### `authevents/primitives.go`

```go
// ComposeCreditDedupKey — mirrors Python compose_credit_dedup_key.
func ComposeCreditDedupKey(policy config.DedupPolicy, userID, orgID, tierID string) string

// ResolveBillingOrg — sticky pin via PinStore.
type ResolveBillingOrgInput struct {
    UserID         string
    FallbackOrgID  string
    AuthURL        string
    MeshAPIKey     string
    PinStore       PinStore
}
func ResolveBillingOrg(ctx context.Context, in ResolveBillingOrgInput) (string, error)

// GrantInitialCreditForUser — idempotent grant.
type GrantInitialCreditInput struct {
    UserID, OrgID  string
    InitialCredits map[string]config.Decimal
    TierProvider   providers.TierProvider
    Redis          RedisClient
    BillingURL     string
    BillingAPIKey  string
    TierRegistry   map[string]*config.Tier  // optional; enables new credit_grant path
}
func GrantInitialCreditForUser(ctx context.Context, in GrantInitialCreditInput) error
```

### `authevents/pinstore.go`

```go
type PinStore interface {
    Get(ctx context.Context, userID string) (string, error)
    Set(ctx context.Context, userID, orgID, source string) error
}
```

Three impls: `DDBPinStore`, `RedisPinStore`, `MemoryPinStore`.

### `authevents/subscribe.go`

```go
type SubscribeInput struct {
    AuthURL       string
    AdminToken    string
    PublicURL     string
    Secret        string
    EventTypes    []string                // default: RegisteredEventTypes()
    WatchOrgSlug  string                  // optional filter
    WatchOrgID    string                  // optional filter (slug resolved if absent)
    Name          string                  // default "ab0t-quota-credit-grant"
}

// SubscribeOnStartup is idempotent: GET first, POST only if no match.
// Returns the subscription ID or empty string + nil error on no-op.
// Never blocks startup — failures log a warning and return non-nil err.
func SubscribeOnStartup(ctx context.Context, in SubscribeInput) (string, error)
```

### `handlerledger/ledger.go`

```go
type LedgerStatus string
const (
    StatusInProgress      LedgerStatus = "in_progress"
    StatusSuccess         LedgerStatus = "success"
    StatusSkipped         LedgerStatus = "skipped"
    StatusFailed          LedgerStatus = "failed"
    StatusFailedPermanent LedgerStatus = "failed_permanent"
)

type LedgerRow struct {
    HandlerName     string
    EventID         string
    EventType       string
    Status          LedgerStatus
    UserID, OrgID   string
    Reason          string
    SideEffectID    string
    Attempts        int
    AttemptedAt     time.Time
    CompletedAt     time.Time
    LeaseExpiresAt  time.Time
    Error           string
    EventPayload    json.RawMessage
}

type AttemptResult struct {
    Proceed    bool
    CachedRow  *LedgerRow
}

type LedgerStore interface {
    RecordAttempt(ctx context.Context, in AttemptInput) (*AttemptResult, error)
    RecordOutcome(ctx context.Context, in OutcomeInput) error
    GetRow(ctx context.Context, handlerName, eventID string) (*LedgerRow, error)
    AlreadyDone(ctx context.Context, dedupKey string) (bool, error)
    MarkDone(ctx context.Context, in MarkDoneInput) error
    QueryByUser(ctx context.Context, userID string, opt QueryOptions) ([]*LedgerRow, error)
    QueryByStatus(ctx context.Context, status LedgerStatus, opt QueryOptions) ([]*LedgerRow, error)
    DeleteUser(ctx context.Context, userID string) (int, error)
}

type AttemptInput struct {
    HandlerName, EventID, EventType string
    EventPayload                    json.RawMessage
    UserID, OrgID                   string
    LeaseSeconds                    int  // default 60
}

type OutcomeInput struct {
    HandlerName, EventID string
    Status               LedgerStatus
    Reason, SideEffectID, Error string
    Attempts             int
}

type MarkDoneInput struct {
    DedupKey, SourceHandler, SourceEventID, SideEffectID string
}

type QueryOptions struct {
    Limit  int
    Since  time.Time  // zero = no filter
}
```

### `handlerledger/decorator.go`

```go
type IdempotentConfig struct {
    Handler      string                                 // required, stable name
    Key          func(authevents.Event) string          // optional business dedup key
    Retry        *RetryConfig                           // nil = default 3/exp; NoRetry = disable
    LeaseSeconds int                                    // default 60
}

type RetryConfig struct {
    Attempts     int           // default 3
    Backoff      BackoffKind   // exponential | linear | constant
    Initial      time.Duration // default 1s
    Max          time.Duration // default 30s
}

var NoRetry = &RetryConfig{Attempts: 1}

// Idempotent wraps an inner handler. Returns a concrete *IdempotentHandler
// struct (implementing authevents.Handler). The receiver detects wrapped
// handlers via type switch — NOT via type assertion on a closure (Go
// has no function attributes).
//
// Trade-off: changes the registry's element type from HandlerFunc to
// the authevents.Handler interface. OnAuthEvent accepts both — it wraps
// HandlerFunc values in a small adapter, while IdempotentHandler is
// passed straight through.
//
// Inside the inner handler, call AlreadyDone / MarkDone / Skip /
// Success on the *Context — return one of the sentinel errors to record
// the matching outcome.
func Idempotent(cfg IdempotentConfig, inner func(ctx context.Context, event authevents.Event, hctx *Context) error) *IdempotentHandler

// IdempotentHandler is exported so dispatchers can type-switch.
type IdempotentHandler struct {
    Config IdempotentConfig
    Inner  func(ctx context.Context, event authevents.Event, hctx *Context) error
}
func (h *IdempotentHandler) Handle(ctx context.Context, event authevents.Event) error  // for the Handler interface

type Context struct {
    HandlerName  string
    EventID      string
    EventType    string
    EventPayload json.RawMessage
    Store        LedgerStore
    DedupKey     string  // composed by IdempotentConfig.Key
}

func (c *Context) AlreadyDone(ctx context.Context) (bool, error)
func (c *Context) MarkDone(ctx context.Context, sideEffectID string) error

// Skip / Success return sentinel errors. Return them from the inner
// handler; the dispatcher records the matching status.
func (c *Context) Skip(reason string) error
func (c *Context) Success(sideEffectID string) error

// Sentinel error types (exported for type-assert):
type SkipError struct{ Reason string }
type SuccessError struct{ SideEffectID string }
```

### `handlerledger/autoselect.go`

```go
type AutoSelectOptions struct {
    Redis     RedisClient
    DDBClient DDBClient
    DDBTable  string   // default "ab0t_quota_handler_ledger"
}

// AutoSelectStore returns a LedgerStore. Priority: DDB > Redis > Memory.
// Memory backend logs a loud warning so it's not silently used in prod.
func AutoSelectStore(opts AutoSelectOptions) LedgerStore
```

### `billing/client.go`

```go
type BillingClient struct {
    BaseURL string
    APIKey  string
    HTTP    *http.Client  // optional; defaults to httpx.New with retry
}

func New(baseURL, apiKey string) *BillingClient

func (c *BillingClient) GetBalance(ctx context.Context, orgID string) (*Balance, error)
func (c *BillingClient) Reserve(ctx context.Context, orgID string, in ReserveInput) (*Reservation, error)
func (c *BillingClient) Commit(ctx context.Context, orgID, reservationID string, in CommitInput) error
func (c *BillingClient) Refund(ctx context.Context, orgID, reservationID, reason string) error
func (c *BillingClient) ApplyPromotionalCredit(ctx context.Context, orgID string, in PromoInput) (*UpdateBalanceResponse, error)
func (c *BillingClient) ApplyCreditGrant(ctx context.Context, orgID string, in CreditGrantInput) (*UpdateBalanceResponse, error)
func (c *BillingClient) ResetSubscriptionCredit(ctx context.Context, orgID, reason string) (*UpdateBalanceResponse, error)
func (c *BillingClient) GetUsage(ctx context.Context, orgID string) (*UsageSummary, error)
func (c *BillingClient) GetTransactions(ctx context.Context, orgID string, limit, offset int) (*TransactionList, error)
func (c *BillingClient) SetTier(ctx context.Context, orgID, tierID, reason string) (*TierChangeResponse, error)
```

### `payment/client.go`

```go
type PaymentClient struct {
    BaseURL string
    APIKey  string
    HTTP    *http.Client
}

func New(baseURL, apiKey string) *PaymentClient

func (c *PaymentClient) GetPlans(ctx context.Context, orgID string, opt GetPlansOptions) (*PlanList, error)
func (c *PaymentClient) CheckoutInit(ctx context.Context) (*CheckoutInit, error)
func (c *PaymentClient) CheckoutForPlan(ctx context.Context, orgID, planID string, in CheckoutInput) (*CheckoutSession, error)
func (c *PaymentClient) VerifyCheckoutSession(ctx context.Context, sessionID string, processIfComplete bool) (*CheckoutVerification, error)
func (c *PaymentClient) GetSubscription(ctx context.Context, orgID string) (*Subscription, error)
func (c *PaymentClient) CancelSubscription(ctx context.Context, orgID string, atPeriodEnd bool) (*Subscription, error)
func (c *PaymentClient) GetInvoices(ctx context.Context, orgID string, opt InvoiceListOptions) (*InvoiceList, error)
func (c *PaymentClient) ForwardStripeWebhook(ctx context.Context, body []byte, signature string) error
```

### `cmd/quotactl/main.go`

```go
// quotactl is the CLI binary. Subcommands mirror Python's
// `python -m ab0t_quota` exactly.

// $ quotactl subscribe-events --auth-url ... --endpoint ... --org-id ...
// $ quotactl events --user-id u123 [--status failed] [--since 1h] [--format table|json]
// $ quotactl replay --handler X --event-id evt_xxx [--webhook-url ...]
// $ quotactl backfill --handler X --user-ids u1,u2,u3 --org-id O [--event-type auth.user.registered]
// $ quotactl delete-user --user-id u123 --confirm

func main() {
    cmd.Execute()  // cobra root command
}
```

Each subcommand file (`subscribe_events.go`, etc.) defines a `*cobra.Command`
with the same flag names + semantics as the Python CLI. They share
`store_from_env.go` which assembles a `LedgerStore` from env vars
(`AB0T_QUOTA_DDB_TABLE`, `QUOTA_REDIS_URL`, `REDIS_URL`).

---

## Dependencies

| Module | Why | Alternative considered |
|---|---|---|
| `github.com/redis/go-redis/v9` | Redis client (sub-5ms p99). Standard library has nothing. | rueidis (less mature for Lua scripts) |
| `github.com/aws/aws-sdk-go-v2/{config,service/dynamodb}` | DDB. SDK v2 is the supported path. | sdk v1 (deprecated) |
| `github.com/shopspring/decimal` | Money. `float64` doesn't round-trip prices. | math/big (unwieldy for JSON) |
| `github.com/spf13/cobra` | CLI. Subcommand routing + flags + help. | std `flag` (no subcommand UX) |
| `github.com/stretchr/testify` | Test assertions. Optional — would work without. | stdlib `testing` (verbose) |
| `github.com/go-chi/chi/v5` | HTTP router for `quota.Mount` (URL-pattern matching). | std `net/http` ServeMux (Go 1.22 added patterns; possibly enough) |

**Decision pending:** `chi` vs std `http.ServeMux` (Go 1.22+). The standard mux now supports method-and-pattern matching; if it covers our needs we save a dep. **Recommendation: prefer std `http.ServeMux`, fall back to chi only if pattern limits bite.**

---

## Testing strategy

Each package owns its tests. Cross-cutting:

- **`handlerledger/conformance_test.go`** — parametrized test suite runs against all 3 LedgerStore impls (Memory, Redis-via-fake, DDB-via-fake). Mirrors Python's `tests/test_handler_ledger.py`.
- **`internal/testutil/`** — in-process fakes (no Docker / no testcontainers in CI). FakeRedis implements the subset of go-redis methods the lib uses. FakeDDB implements `PutItem/GetItem/UpdateItem/Query/DeleteItem`.
- **`cmd/quotactl/main_test.go`** — uses `testscript` (`github.com/rogpeppe/go-internal/testscript`) for end-to-end CLI tests.
- **`examples/*/main.go`** — built in CI via `go vet` + `go build`, ensuring the examples stay compiling.

Coverage target: ≥ 80% statement coverage per package. CI fails below threshold.

---

## Wire-level parity matrix

These MUST match the Python lib exactly. Any divergence is a bug.

| Surface | Python | Go (port) | Validation |
|---|---|---|---|
| `quota-config.json` schema | `ab0t_quota/models/core.py:TierConfig` etc. | `config/tier.go` etc. | shared `testdata/quota-config.example.json` parses identically |
| Redis key for credit dedup | `credit_granted:user:{user_id}:{tier_id}` | identical | unit test pins the literal string |
| Redis key for ledger row | `ledger:row:{handler}:{event_id}` | identical | unit test |
| DDB PK for ledger row | `HANDLER#{handler}#{event_id}` | identical | unit test |
| DDB PK for biz-dedup | `BIZDEDUP#{sha256(key)}` | identical | unit test |
| HMAC header name | `X-Event-Signature: sha256=<hex>` | identical | integration test |
| Webhook receiver path | `/api/quotas/_webhooks/auth` | identical | integration test |
| Lua script SHAs (counters) | computed from `ab0t_quota/counters/lua/*.lua` | identical (same Lua source) | tested via Redis EVAL_SHA |
| Billing API contract | path, body, headers, retry semantics | mirrored | tested against billing-service openapi.json snapshot |
| Payment API contract | same | mirrored | tested against payment-service openapi.json snapshot |
| CLI flag names | `--user-id`, `--status`, `--since`, `--format`, `--confirm` | identical | scripted comparison test |

---

## Out of scope for 0.1.0

These exist in Python but ship later in Go:

- Bridge mode HTTPS-only client (`ab0t_quota/bridge.py`). Defer until a Go consumer asks.
- Stripe Checkout HTML templates served by the lib (`ab0t_quota/billing/templates/`). Consumers can host their own.
- Increase-request workflow (`ab0t_quota/models/increase_requests.py`). Lower priority — most Go consumers won't have an admin dashboard yet.
- `bridge.py` analog — HTTP-only quota check without Redis. Could be added but the primary mode is byo_redis.

These are explicitly listed so the implementer doesn't try to port them in the first pass.

---

## Acceptance criteria for v0.1.0

- [ ] `go test ./...` passes; coverage ≥ 80% per package
- [ ] `go vet ./...` and `golangci-lint run` clean
- [ ] All 4 examples in `examples/` build via `go build`
- [ ] Conformance suite in `handlerledger/` passes against all 3 backends
- [ ] CLI binary at `cmd/quotactl/` covers all 5 Python subcommands with the same flag names
- [ ] Wire-level parity matrix tests pass (above)
- [ ] Loading `testdata/quota-config.example.json` produces a Config struct equivalent to what the Python lib loads (asserted via a side-by-side fixture)
- [ ] README quickstart compiles and runs against a real Redis
- [ ] PRODUCT_SPEC.md (this file) updated for any deviations
- [ ] `back_references.md` reviewed and current

---

## Implementation order (suggested)

1. **Phase 1 — config + engine + counters.** Get tier resolution + quota checks working against Redis. No HTTP yet. ~1 week.
2. **Phase 2 — HTTP routes + middleware.** Mount `/api/quotas/*`. Rate-limit middleware. ~3 days.
3. **Phase 3 — billing + payment clients + proxy router.** ~1 week.
4. **Phase 4 — authevents registry + receiver.** Mirror Python's drop-in pattern. ~3 days.
5. **Phase 5 — handlerledger framework.** LedgerStore + 3 backends + decorator + retry. ~1 week.
6. **Phase 6 — CLI.** ~3 days.
7. **Phase 7 — examples, docs polish, parity matrix tests, v0.1.0 tag.** ~3 days.

Total: ~4 weeks single developer. Each phase is independently shippable; the lib is usable after phase 2 for read-only consumers and after phase 5 for full-feature consumers.

---

## 11. Wire contracts (public API surfaces — pin verbatim)

These are visible to end users (browsers and integrators) of consumer services. Identical across all consumers by Python's design; the Go port preserves them exactly.

### 11.1 — 429 quota-denied body

Returned by middleware and recommended by `WriteDenial(w, *QuotaError)`. `remaining = limit - current - requested` (post-decision headroom). `utilization = round(current/limit, 4)`.

```json
{
  "error": "quota_exceeded",
  "resource": "sandbox.concurrent",
  "current": 5,
  "requested": 1,
  "limit": 5,
  "remaining": -1,
  "tier": "starter",
  "tier_display": "Starter",
  "upgrade_url": "/billing/upgrade",
  "retry_after": 60,
  "message": "You've reached the max of 5 sandboxes on Starter. Upgrade to Pro for up to 25."
}
```

### 11.2 — Headers (every middleware response)

- 429: `Retry-After: <seconds>` (denial's retry_after or 60), `X-Quota-Limit: <int>`, `X-Quota-Current: <int>`, `X-Quota-Resource: <key>`
- 200 (allowed): `X-Quota-Limit: <int>`, `X-Quota-Remaining: <int>` (both as ints, after the increment)
- 503 (service unavailable, fail-closed mode): body `{"error": "quota_service_unavailable", "detail": "Quota enforcement is temporarily unavailable."}`

### 11.3 — Webhook receiver responses (`POST /api/quotas/_webhooks/auth`)

- 401 (HMAC missing/invalid): `{"detail": "invalid signature"}` — **static string** (no dynamic detail; never leak which header / what alg)
- 400 (JSON parse): `{"detail": "invalid json"}` — static
- 200 (no handlers): `{"status": "ignored", "event_type": "..."}`
- 200 (handlers ran): `{"status": "ok", "ran": N, "event_type": "..."}`

Handler errors are caught + logged inside the dispatcher; auth always sees 200 (else auth's retry compounds). For an idempotent handler whose retries are exhausted, the row sits at `failed_permanent` and `quotactl replay` is the designed recovery path.

### 11.4 — Quota API routes (mounted by `Mount`)

- `GET /api/quotas/usage` — returns `Usage` (org's tier + per-resource current/limit/utilization)
- `GET /api/quotas/tiers` — public (no auth required); returns `{"tiers": [{tier_id, display_name, description, features[], limits: {key: {limit, limit_display}}, upgrade_url}]}` sorted by `sort_order`. `limit_display` = `"Unlimited"` or `%g`-formatted.
- `GET /api/quotas/check/{resource_key}` — pre-flight check, no increment
- `GET /api/quotas/check-bundle/{bundle_name}` — pre-flight bundle check (the primary consumer pattern — sandbox uses bundles for everything)
- 401 on `/usage` and `/check*` when org extraction fails: `{"detail": "Unable to resolve org_id"}`

### 11.5 — Webhook signature verification — pin

- Header name primary: `X-Event-Signature`. Fallback (legacy publisher): `X-Webhook-Signature`. Receiver tries both in that order.
- Value: `sha256=<hex>` OR bare `<hex>` (the Python CLI's own `replay`/`backfill` sign with bare hex).
- HMAC over **the raw received body bytes** — NEVER re-canonicalize JSON in the receiver. Re-canonicalization is a compatibility trap (Python `ensure_ascii=True` vs Go's `<>&` escaping; float formatting differs). The auth v1 publisher's own bug (Known Upstream Bug #3) is the cautionary tale.
- `hmac.Equal` (constant-time) — house rule; explicit because easy to forget.

---

## 12. Master environment variable inventory

| Var | Subsystem | Notes |
|---|---|---|
| `AB0T_MESH_API_KEY` | mesh (unified) | fallback for both per-service keys; CLI subscribe-events fallback |
| `AB0T_MESH_BILLING_API_KEY` | billing client | per-upstream override (review M12) |
| `AB0T_MESH_PAYMENT_API_KEY` | payment client | per-upstream override |
| `AB0T_CONSUMER_ORG_ID` | paid-tier router | router not mounted without it |
| `AB0T_MESH_BILLING_URL` | billing client | dev URL override; not part of the consumer-facing API |
| `AB0T_MESH_PAYMENT_URL` | payment client | dev URL override |
| `AB0T_SERVICE_NAME` | catalog publish, bridge identity | resolution order: env → `config.service_name` → first resource's `service` → skip |
| `AB0T_AUTH_AUTH_URL` (fallback `AUTH_SERVICE_URL`) | webhook subscribe, org resolution | second fallback exists in Python; preserve |
| `AB0T_AUTH_ADMIN_TOKEN` | subscription writes | needs `events.subscribe` + `events.read` + `events.test` perms |
| `AB0T_AUTH_WEBHOOK_PUBLIC_URL` | auto-subscribe | public base URL receivers register |
| `AB0T_AUTH_WEBHOOK_SECRET` | receiver + auto-subscribe | HMAC secret; per-subscription operator-generated |
| `AB0T_AUTH_WATCH_ORG_SLUG` (fallback `AB0T_AUTH_ORG_SLUG`) | subscription filter | |
| `AB0T_QUOTA_STRIPE_WEBHOOK_SECRET` (fallback `STRIPE_WEBHOOK_SECRET`) | Stripe webhook proxy | only when C4 resolves to "port" |
| `AB0T_MESH_SNS_LIFECYCLE_TOPIC_ARN` (fallback `SNS_LIFECYCLE_TOPIC_ARN`) | LifecycleEmitter | only when M3 resolves to "port SNS" |
| `AB0T_QUOTA_ALLOW_EXPERIMENTAL_BILLING_MODELS` | config load | gate for non-stable billing_model values; fail-loud at load |
| `AB0T_QUOTA_DDB_TABLE` | CLI ledger store | |
| `QUOTA_CONFIG_PATH` | config load | search-path entry |
| `QUOTA_REDIS_URL` (fallback `REDIS_URL`) | storage | default `redis://localhost:6379/0` |
| `QUOTA_REDIS_PASSWORD` (fallback `REDIS_PASSWORD`) | storage | house convention |
| `QUOTA_FAIL_OPEN_DURING_STARTUP` | consumer-side escape hatch | pre-flight checks fail-closed by default, 503 before engine-ready; this opens that window |
| `QUOTA_*` (namespace) | config interpolation | `${VAR}` / `${VAR:-default}` expansion in JSON values; non-`QUOTA_`-prefixed refs warn |
| `DYNAMODB_ENDPOINT` | persistence | SSRF-guarded allowlist; only the documented test/dev hosts |
| `AWS_REGION` | persistence | default `us-east-1` |

---

## 13. Behavioral contracts the implementer must pin

### 13.1 — Multi-replica semantics

Multiple replicas of a Go consumer running the same `quota.Setup`:

- **Snapshot worker** runs on each replica; writes are idempotent (`PutItem` with org_id + resource_key + timestamp). Duplicate snapshots are benign.
- **HeartbeatMonitor** runs on each replica; synthetic stop emits to SNS are **not deduped at the topic** — same `resource_id` may publish twice. Cost record IS deduped via Redis flag `cost:lifecycle:{resource_id}`. Documented limitation.
- **Auto-subscribe** is idempotent by endpoint match (GET first, POST only on no-match).
- **Alerts** fire inline during `check`; cooldown is Redis-keyed and shared across replicas.

### 13.2 — Setup lifecycle

- `Setup(ctx, cfg)` returns only after engine, persistence (if enabled), and route mounting are complete. No FastAPI-lifespan-equivalent ordering window — this is a genuine Go advantage; eliminates the 503-during-startup window Python consumers must guard against with `QUOTA_FAIL_OPEN_DURING_STARTUP`.
- Persistence init failure is **non-fatal** — logs `warning: quota persistence init failed (non-fatal): <err>`, falls through to Redis-only operation.
- `engine_mode` resolution order: explicit `Config.EngineMode` → `config.engine_mode` → `"local"`. Unknown value warns + falls back to local. `"byo_redis"` is an alias for `"local"`. `"bridge"` returns an error (out of scope for v0.1.0; spec must parse but reject).
- `OnReady` may be sync or async; exceptions are caught and logged as warnings; the consumer never blocks Setup's return.
- Auto-subscribe (`SubscribeOnStartup`) runs as a **fire-and-forget task after `Setup` returns** — not blocking. Failures log warnings; the next process start retries.

### 13.3 — Close ordering

`QuotaContext.Close()` runs teardown in this order:

1. HeartbeatMonitor stop
2. SnapshotWorker stop (via store.Close)
3. Redis client close
4. DDB client released

Calls beyond the first return `nil`. Safe to defer at the top of main.

### 13.4 — Concurrency invariants

- `QuotaContext` and `Engine` are safe for concurrent use.
- Handlers dispatch sequentially per delivery in registration order.
- Registry (`authevents`) is mutex-guarded; OnAuthEvent is safe to call concurrently with other registrations.
- No global mutable state outside the package-level handler registry. `NewRegistry()` exists for tests and multi-tenant binaries.

### 13.5 — Capability report (Go improvement; not in Python)

`QuotaContext.Capabilities()` returns a typed report:

```go
type Capabilities struct {
    QuotaAPI      bool   // /api/quotas/* mounted
    RateLimit     bool   // QuotaGuard middleware engaged
    Paid          bool   // billing + payment routers mounted; false with reason when missing AB0T_CONSUMER_ORG_ID etc.
    PaidReason    string // human reason when Paid is false
    AuthEvents    bool   // webhook receiver mounted
    AuthSubscribe bool   // auto-subscribe ran (or will run async)
    Ledger        string // backend name: "DDBLedgerStore" | "RedisLedgerStore" | "InMemoryLedgerStore"
    LedgerWarn    string // populated only for in-memory
}
```

Logged as one block at the end of `Setup`. Converts the #1 Python operability complaint ("why didn't credits grant?" — Known Upstream Bug #1 hid for a release behind exactly this) into a 5-second diagnosis.

### 13.6 — Forward-compatibility policy (config)

- Unknown top-level keys are **ignored with a debug log** (NOT `DisallowUnknownFields`). The Python lib will add fields (open proposal: `plans[]`); Go consumers must not break when those land.
- Keys prefixed with `$` are comments (`$comment`, `$schema`, `$comment_*` — see real-world `quota-config.example.json`). Tolerance must be general, not field-by-field.
- Validation operates on the *known* fields only; unknown fields never trigger errors.

### 13.7 — HTTP client policy

- Default timeouts: billing/payment clients 15s; auth event subscribe 15s; org lookups 10s; tier fetch 5s; catalog publish 5s; CLI subscribe 20s; CLI replay/backfill 30s.
- **No retry by default** — matches Python (which does NOT retry any HTTP call). Retry is opt-in per-call via `httpx.WithRetry(opts)` and applies ONLY to idempotent methods (GET/PUT with key) or POSTs carrying an explicit idempotency key. `reserve`, `record_usage`, subscription/checkout creates: never retry without an explicit idempotency key.
- Typed errors: `BillingServiceError`, `PaymentServiceError`, `AuthServiceError` each carry `{StatusCode int, Detail string, URL string}`. Connect error → 503-ish; timeout → 504-ish. Error `Detail` MUST be logged at the router layer and MASKED in responses to the end user (house rule: no upstream `detail` leak through the proxy).

---

## 14. Amendments accepted (rev 2)

Summary of substantive accepted findings from `review_20260611.md` + `review_20260611_addendum.md`. Each is reflected inline above; this index gives reviewers a single audit trail.

**Critical (inline-fixed):**
- C1 — counters rewritten: plain Redis ops, no Lua, correct key shapes
- C2 — `int64` → `float64` throughout (counters, limits, deltas, `WithDelta`)
- C3 — config schema scope expanded; deferred to dedicated config sub-sections in rev 3 (see TODO at §15)
- C5 — endpoint corrections moved to `back_references.md`
- C6 — `authevents.Event` to carry top-level envelope fields + signature header alternates + content-hash fallback
- C7 — idempotent detection via Handler interface + type switch (NOT closure type assertion)
- C8 — flagged as Known Upstream Bug #1; Go port wires tier_provider explicitly

**Major (inline-fixed):**
- M1 — `EnablePaid` default false is a deliberate deviation from Python's true; documented at §3
- M3 — `LifecycleEmitter` SNS path: deferred to v0.2 unless C4 forces sooner; cost-record-only in v0.1.0
- M4 — `QuotaResult` expanded with decision/severity/denied_level/retry_after/utilization/upgrade_url/has_override
- M5 — tier cache moves to Redis-shared (`quota:tier:{org_id}`) with explicit `Invalidate(org_id)`
- M6/M7 — receiver/dispatch & grant-flow specifics enumerated under §13.1/§13.2 + parity matrix
- M9 — messages: config-driven from day one (no hardcoded tier mapping)
- M10 — `enforcement.shadow_mode` IS implemented in Go (check-and-log-but-allow); `global_kill_switch` honored
- M11 — auto-subscribe endpoint composition uses the actual mount prefix
- M12 — per-upstream mesh credentials in `Config`
- M13 — `paid_auth_reader`/`paid_auth_admin` as consumer-supplied `func(http.Handler) http.Handler`; strict-mode error preserved
- M14 — DDB GSI names, TTL attribute name; auto-create disabled (operator-provisioned)
- M15 — `quota.OrgFromContext` / `quota.WithOrg` context-key contract

**Critical findings from addendum (new in rev 2):**
- A1 — Known Upstream Bug #3 (auth v1 webhook signing mismatch). Go receiver verifies raw bytes; never re-canonicalizes.
- A2 — 429 wire contract pinned at §11.1
- A3 — `/check-bundle` route, response shapes, org-extractor contract all pinned at §11.4 + §13.5
- A4 — `Emitter()`, `LedgerStore()`, `Redis()` accessors on QuotaContext
- A5 — Setup/lifecycle at §13.2 (incl. "alerts are inline, no background worker" correction)
- A8 — HTTP client policy at §13.7
- A9 — env var inventory at §12
- A10 — forward-compat policy at §13.6
- A14 — security at §11.5 + house rules

## 15. TODO (rev 3, before implementation starts)

These need a separate pass because they'd double the spec size if inlined now:

- [ ] Full `Config` schema in `config/` package files (all 8 sub-sections from C3 — billing_model + price + validators, full TierLimits, etc.)
- [ ] `BillingClient` field-level request/response models (currently sketch-only)
- [ ] `PaymentClient` field-level request/response models
- [ ] Stripe webhook proxy decision (C4): in-scope or fail-loud-only for v0.1.0
- [ ] Documentation plan for Phase 7 (A11: which Python docs port over, which adapt, which are Go-flavored runbooks)
- [ ] Cross-language fixture test design (acceptance criterion: same Redis/DDB fixtures verified by both libs)
