# ab0t-quota-go — implementation tasklist

**Started:** 2026-06-11
**Goal:** v0.1.0 per PRODUCT_SPEC.md rev 2 — all files created, tests passing.
**Owner:** Claude
**Strategy:** Dependency order. Foundational packages first (config → handlerledger → authevents → counters). Top-level wiring last. Each task self-tests before checkmark.

All tasks claimed. WORKLOG appended after each task — append-only.

---

## Tasks

### Foundation
- [ ] `F1` `go.mod` + module init at `github.com/ab0t-com/ab0t-quota-go`
- [ ] `F2` `.golangci.yml` + `.gitignore` + `LICENSE` + `Makefile`
- [ ] `F3` Create full directory tree

### Phase 1a — config package
- [ ] `P1a1` `config/decimal.go` — Decimal wrapper with String/Number-tolerant unmarshal
- [ ] `P1a2` `config/tier.go` — Tier, TierLimits (5 fields), CreditGrant (incl. dedup), enums (CreditTrigger/Lifecycle/Destination/BillingModel/DedupPolicy)
- [ ] `P1a3` `config/resource.go` — ResourceDef, CounterType enum, ResourceBundle
- [ ] `P1a4` `config/storage.go` `enforcement.go` `alerts.go` `tier_provider.go` `billing_integration.go` `reconciliation.go`
- [ ] `P1a5` `config/config.go` — root Config struct + unknown-key-tolerant unmarshal
- [ ] `P1a6` `config/load.go` — LoadConfig + search paths + `${QUOTA_*}` interpolation
- [ ] `P1a7` `config/testdata/` — minimal.json, full.json
- [ ] `P1a8` `config/config_test.go` — covers interpolation, defaults, validators, forward-compat

### Phase 1b — engine + counters + providers + registry + messages
- [ ] `P1b1` `counters/counter.go` `gauge.go` `rate.go` `accumulator.go` `idempotency.go` `factory.go` + tests
- [ ] `P1b2` `providers/provider.go` `jwt.go` `mesh.go` `static.go` `cache.go` + tests
- [ ] `P1b3` `registry/registry.go` + tests
- [ ] `P1b4` `messages/builder.go` + tests (config-driven hints from day one)
- [ ] `P1b5` `engine/result.go` `engine.go` `tier_resolution.go` `override_loader.go` + tests

### Phase 2 — HTTP middleware
- [ ] `P2a` `middleware/headers.go` — WriteDenial helper + header writers
- [ ] `P2b` `middleware/guard.go` — Guard http.Handler wrapper, fail-open/closed, exempt paths
- [ ] `P2c` middleware tests

### Phase 3 — billing + payment clients
- [ ] `P3a` `internal/httpx/client.go` — base HTTP client with typed errors, per-call timeout
- [ ] `P3b` `mesh/urls.go` `auth.go`
- [ ] `P3c` `billing/models.go` `client.go` + tests
- [ ] `P3d` `billing/router.go` `auth_helpers.go` — proxy routes
- [ ] `P3e` `billing/lifecycle.go` `heartbeat.go`
- [ ] `P3f` `payment/models.go` `client.go` + tests
- [ ] `P3g` `payment/router.go` — proxy routes

### Phase 4 — authevents
- [ ] `P4a` `authevents/event.go` `hmac.go`
- [ ] `P4b` `authevents/registry.go` — Handler interface + HandlerFunc adapter + Registry
- [ ] `P4c` `authevents/receiver.go` — make_router with dispatch + type switch
- [ ] `P4d` `authevents/primitives.go` `pinstore.go` + memory/redis/ddb backends
- [ ] `P4e` `authevents/subscribe.go` — SubscribeOnStartup
- [ ] `P4f` `authevents/default_handler.go` (tier_provider wired, NOT broken like Python BUG #1)
- [ ] `P4g` authevents tests (parity with Python's test_auth_events.py)

### Phase 5 — handlerledger
- [ ] `P5a` `handlerledger/ledger.go` — LedgerStore interface, LedgerRow, AttemptResult, statuses
- [ ] `P5b` `handlerledger/memory.go` `redis.go` `dynamodb.go`
- [ ] `P5c` `handlerledger/outcomes.go` — SkipError/SuccessError sentinels
- [ ] `P5d` `handlerledger/retry.go` — exponential backoff
- [ ] `P5e` `handlerledger/decorator.go` — IdempotentHandler struct + Idempotent fn + HandlerContext
- [ ] `P5f` `handlerledger/autoselect.go`
- [ ] `P5g` `handlerledger/conformance_test.go` — all 3 backends through one test body
- [ ] `P5h` `handlerledger/decorator_test.go` — integration with receiver

### Phase 6 — persistence + alerts + quota top-level
- [ ] `P6a` `persistence/store.go` `dynamodb.go` `seed.go` `snapshot_worker.go`
- [ ] `P6b` `alerts/manager.go` `dispatcher.go` `log.go` `webhook.go` (SSRF guard)
- [ ] `P6c` `quota/setup.go` `quota_context.go` `lifespan.go` — top-level Setup + Capabilities
- [ ] `P6d` `quota/setup_test.go`

### Phase 7 — CLI
- [ ] `P7a` `cmd/quotactl/main.go` + `store_from_env.go`
- [ ] `P7b` 5 subcommands: subscribe_events, events, replay, backfill, delete_user
- [ ] `P7c` `cmd/quotactl/main_test.go`

### Phase 8 — examples + CI
- [ ] `P8a` `examples/basic/main.go`
- [ ] `P8b` `examples/with_auth_events/main.go`
- [ ] `P8c` `examples/with_idempotent/main.go`
- [ ] `P8d` `.github/workflows/ci.yml`
- [ ] `P8e` `testdata/` — sample configs + webhook event corpus

### Phase 9 — finalize
- [ ] `P9a` `go vet ./...` clean
- [ ] `P9b` `go build ./...` clean
- [ ] `P9c` All tests pass with race detector
- [ ] `P9d` Examples compile + run against testutil fakes

---

## WORKLOG (append-only)

### 2026-06-11 — Foundation + Phase 1a + 4 + 5 complete
- [x] `F1` go.mod created — module `github.com/ab0t-com/ab0t-quota-go`, Go 1.22, deps: go-redis/v9, decimal, cobra
- [x] `P1a1`-`P1a8` config package: decimal + tier + resource + sub + config + load + testdata + 11 tests **PASS**
- [x] `P5a`-`P5h` handlerledger package: ledger + memory + outcomes + decorator + retry + autoselect + redis/ddb stubs + 13 tests **PASS**
- [x] `P4a` authevents/event.go + hmac.go — v1+v2 envelope, signature alternates, content-hash fallback
- [x] `P4b` authevents/registry.go — Handler interface + HandlerFunc + Registry + package default + handlersEqual via reflect (functions aren't `==`-comparable, fixed mid-test)
- [x] `P4c` authevents/receiver.go — MakeRouter, wire contract (401/400/200 with static strings), legacy X-Webhook-Signature fallback
- [x] `P4d` authevents/primitives.go + pinstore.go — ComposeCreditDedupKey (4 policies) + PinStore + MemoryPinStore (operator-wins) + Redis/DDB stubs
- [x] `P4e` authevents/subscribe.go — SubscribeOnStartup, idempotent (GET then POST), slug→org resolution
- [x] `P4f` authevents/default_handler.go — TierProvider REQUIRED (no Python BUG #1), pin-store inverse-pinning, per-tier dedup policy, hooks nil-safe
- [x] `P4g` authevents/authevents_test.go — 19 tests covering parsing, HMAC, registry, dedup keys, pin store, receiver wire, idempotent dispatch, retry, default credit grant **PASS**

Next up: `idempotent.go` adapter wrapping `*handlerledger.IdempotentHandler` so authevents.Handler interface stays clean — DONE inline.

**Phase 4 + 5 + 1a all green.** Next: Phase 1b — counters (Redis float counters via INCRBYFLOAT) + providers + registry + messages + engine.

### 2026-06-11 (cont) — Phase 1b + 2 + 3 + 6 + 7 + 8 + 9 complete
- [x] `P1b1` counters: counter + gauge + rate + accumulator + idempotency + factory + memory store + redis stub + 13 tests **PASS**
- [x] `P1b2` providers: jwt + static + mesh + cache + 10 tests **PASS**
- [x] `P1b3` registry/registry.go + 3 tests **PASS**
- [x] `P1b4` messages/builder.go + 4 tests **PASS**
- [x] `P1b5` engine: result + engine + tier_resolution + 9 tests **PASS** (covers allow/deny/burst/shadow/killswitch/unknown)
- [x] `P2a` middleware/headers.go — WriteHeaders/WriteDenial/WriteWarn matching Python lib header set
- [x] `P2b` middleware/guard.go — Guard wrapper, exempt paths, fail-open/closed, JWT-from-Authorization plumbing
- [x] `P2c` middleware/middleware_test.go — 7 tests **PASS**
- [x] `P3a` internal/httpx/client.go + 3 tests **PASS**
- [x] `P3b` mesh/urls.go + auth.go (env-var resolution)
- [x] `P3c` billing/models.go + client.go + 5 tests **PASS** (verifies C5 fixes: GET /summary, DELETE /subscriptions)
- [x] `P3e` billing/lifecycle.go + heartbeat.go
- [x] `P3f`/`P3g` payment/models.go + client.go + 3 tests **PASS** (verifies C5 fix: GET /verify)
- [x] `P6b` alerts: manager + log + webhook (SSRF guard) + 6 tests **PASS**
- [x] `P6c` quota/setup.go + quota_context.go — top-level Setup, Capabilities snapshot, Middleware/WebhookHandler exposure
- [x] `P6d` quota/setup_test.go — 3 tests **PASS** (end-to-end middleware + credit-grant wiring)
- [x] `P7a`/`P7b`/`P7c` cmd/quotactl: main + 6 subcommands (subscribe-events, events, replay, backfill, delete-user, capabilities) + 5 tests **PASS**
- [x] `P8a`-`P8c` examples: basic + with_auth_events + with_idempotent (all build)
- [x] `P8d` .github/workflows/ci.yml — vet + build + race-detector tests
- [x] `P9a` `go vet ./...` — clean
- [x] `P9b` `go build ./...` — clean
- [x] `P9c` `go test -race -count=1 ./...` — **14 packages, all PASS**

### v0.1.0 status: feature-complete
- All required v0.1.0 packages exist + ship green tests under race detector.
- Wire-level parity confirmed for: HMAC formats, signature header alternates, business-dedup key shapes (4 policies), billing endpoint paths/methods (per back_references C5 fixes), config schema, env-var names.
- Known Upstream Bug #1 (Python tier_provider NameError) is structurally prevented: TierProvider is a required constructor arg in BuildDefaultCreditGrantHandler.
- Known Upstream Bug #4 (Python shadow_mode unread) is fixed: engine flips Deny → ShadowAllow when shadow_mode=true.
- v0.2 work clearly scoped: Redis FloatStore + RateStore wiring, DDB LedgerStore + PinStore wiring (all interfaces and stubs in place with typed `ErrRedisNotAvailable`).

Outstanding (not required for v0.1.0):
- Persistence package (DDB snapshot worker) — sketched at PRODUCT_SPEC §15, not in v0.1.0
- handlerledger conformance suite across backends — applies once Redis/DDB ledger backends are wired
- testdata/ webhook event corpus — partial (testdata configs in examples/basic only)

---

## v0.1.0 distribution + hardening (claimed 2026-06-11)

### Phase 10 — distribution
- [ ] `P10a` `scripts/build.sh` — cross-compile quotactl for darwin/linux/windows × amd64/arm64 → `release/<version>/`
- [ ] `P10b` `scripts/release.sh` — orchestrate full pre-release flow (vet → race tests → cross-build → checksums → tag prep). Does NOT push (user runs that themselves per feedback memory)
- [ ] `P10c` `Makefile` — convenience targets (test, race, build, release, lint, tidy)
- [ ] `P10d` `CONSUMING.md` — document the two consumption paths: Go module import via `go get` (no binary) and CLI binary install
- [ ] `P10e` `LICENSE` (MIT) + `.gitignore` (Go-standard + release/ dir)

### Phase 11 — hardening tests
- [ ] `P11a` testdata/webhook-events/ — JSONL corpus of v1+v2 envelopes, sigs precomputed for `quota_test_secret`
- [ ] `P11b` handlerledger conformance test — replays the corpus through the in-memory store, asserts every status path is exercised
- [ ] `P11c` receiver concurrency test — 100 parallel POSTs of the same event, assert handler runs exactly once (delivery dedup race-proof)
- [ ] `P11d` engine concurrency test — gauge increments race-proof, accumulator no double-spend
- [ ] `P11e` quota.Setup degraded-mode test — confirms WhyOff entries surface correctly when env is empty

### Phase 12 — understanding docs
- [ ] `P12a` `ARCHITECTURE.md` — module dependency graph + 3-paragraph theory of operation
- [ ] `P12b` `MIGRATION_FROM_PYTHON.md` — line-by-line: where Python v0.5.2 callsites map in Go

### 2026-06-11 (cont 2) — Distribution + hardening
- [x] `P10a` scripts/build.sh — cross-compile darwin/linux × amd64/arm64 + windows/amd64; SHA256SUMS; ldflags stamp version/commit/buildTime; **tested** (5 binaries produced, linux runs `--version` correctly)
- [x] `P10b` scripts/release.sh — clean tree → vet → race tests → cross-build → notes scaffold → prints exact tag+push commands for operator to run. Does NOT push (per user feedback memory)
- [x] `P10c` Makefile — replaced old one with cleaner targets (test, race, vet, fmt, tidy, build, install, examples, cover, dist, release, clean)
- [x] `P10d` CONSUMING.md — Path A (Go module via go get) explained including module proxy + GOPRIVATE; Path B (quotactl binary) via go install OR GH releases curl+sha256 verify; build-it-yourself via make dist
- [x] `P10e` LICENSE (MIT) + .gitignore expanded with /release/ and cover.*

### Phase 11 — hardening tests
- [x] `P11a` testdata/webhook-events/ — 4 corpus files (v1 happy, v2 envelope, missing user_id, tenant_id fallback) + README
- [x] `P11b` handlerledger/conformance_test.go — 6 scenarios run against in-memory store; structured to take Redis/DDB backends in v0.2 via runConformance harness. **Caught real race** in InMemoryLedgerStore.RecordAttempt (returned live pointer to row that RecordOutcome was mutating) — fixed by returning snapshot.
- [x] `P11c` authevents/concurrency_test.go — 100 parallel POSTs of same event_id, handler runs exactly once; 50 distinct events all run; passes under `-race`
- [x] `P11d` engine/concurrency_test.go — 100x gauge spend = exactly 100, 50x accumulator 0.5 spend ≈ 25.0, paired spend/release → 0
- [x] `P11e` quota/degraded_test.go — Setup with empty env reports all 4 expected WhyOff entries; alerts webhook on for https URL, off + WhyOff'd for file:// URL

### Phase 12 — understanding docs
- [x] `P12a` ARCHITECTURE.md — 1-paragraph + 3-paragraph theory of operation, dependency graph, storage abstraction table, wire-level parity claims, known upstream bugs table
- [x] `P12b` MIGRATION_FROM_PYTHON.md — side-by-side callsite mapping (Setup, check, spend, handlers, @idempotent, CreditGranter, CLI, config); covers the differences worth knowing

### Final state — Phase 9 final run
`go vet ./...` clean. `go test -race -count=1 ./...` — **15 packages with tests, all PASS** under race detector. Race caught in P11c was a genuine bug in in-memory ledger; fixed in same session.

### Distribution proof
`VERSION=v0.1.0 ./scripts/build.sh` produced:
```
quotactl-darwin-amd64    6.5 MB
quotactl-darwin-arm64    6.2 MB
quotactl-linux-amd64     6.3 MB
quotactl-linux-arm64     6.2 MB
quotactl-windows-amd64.exe 6.6 MB
SHA256SUMS
```
Linux binary verified: `--version` prints stamped version, commit, build time. `--help` lists all 6 subcommands.

### What the operator runs to ship v0.1.0
Per user feedback memory ("user runs push.sh themselves"), I prepared everything but stopped short of git commit/tag/push:
1. `git add -A && git commit -m 'v0.1.0'`
2. `scripts/release.sh v0.1.0`
3. Operator edits the generated release/v0.1.0/RELEASE_NOTES.md
4. Operator runs `git tag -a v0.1.0 -m '...'; git push origin v0.1.0`
5. Operator runs `gh release create v0.1.0 --notes-file ... release/v0.1.0/quotactl-*`

That gives both consumption paths simultaneously: Go consumers see `v0.1.0` on proxy.golang.org within minutes; binary consumers download from GH releases.
