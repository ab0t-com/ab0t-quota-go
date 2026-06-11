# ab0t-quota-go — Documentation

You're here because you're integrating ab0t-quota-go into a Go service
and you need more than the auto-generated GoDoc.

The docs are structured as a learning path. Read in order; later docs
assume the vocabulary of earlier ones.

**If you're using an AI coding agent** (Claude Code, Cursor, Codex,
Gemini, etc.), point it at the master skill
[`Skills/ab0t-quota/SKILL.md`](../Skills/ab0t-quota/SKILL.md) — it
routes the agent to the right specialized skill or doc based on what
you're asking.

---

## Path 1 — "I've never built billing before"

For developers new to billing system design. Walk this top to bottom.

1. **[BILLING_GLOSSARY.md](BILLING_GLOSSARY.md)** — every term
   defined in plain English. Tier, credit, subscription, accumulator,
   PinStore, MRR vs ARR, shadow mode. Skim or use as a lookup.
2. **[BILLING_MODELS_GUIDE.md](BILLING_MODELS_GUIDE.md)** — 11
   billing archetypes (A through K) with concrete config. Pick one,
   copy the JSON, adapt.
3. **[PAYMENT_PIPELINE.md](PAYMENT_PIPELINE.md)** — the four-service
   architecture (Stripe → payment → billing → auth → your service).
   The seven moments where money flows. Where to debug when something
   goes missing.
4. **[INTEGRATION_RUNBOOK.md](INTEGRATION_RUNBOOK.md)** — nine
   stages from "decide your billing shape" to "wire dashboards." 1–2
   engineering days end-to-end.

---

## Path 2 — "I've shipped billing before, just give me the Go API"

For developers who know the domain and just need the mechanics. Skip
to the skills.

| Surface | Skill |
|---------|-------|
| `quota.Setup` + env vars + Capabilities | [ab0t-quota-go-setup](../Skills/ab0t-quota-go-setup/SKILL.md) |
| HTTP guard, identity, router | [ab0t-quota-go-middleware](../Skills/ab0t-quota-go-middleware/SKILL.md) |
| Webhook receiver, `Idempotent`, credit grant | [ab0t-quota-go-auth-events](../Skills/ab0t-quota-go-auth-events/SKILL.md) |
| Admin CLI | [ab0t-quota-go-cli](../Skills/ab0t-quota-go-cli/SKILL.md) |
| Config schema | [ab0t-quota-go-config](../Skills/ab0t-quota-go-config/SKILL.md) |
| How to think about pricing | [ab0t-quota-billing-design](../Skills/ab0t-quota-billing-design/SKILL.md) |
| Billing concepts 101 | [ab0t-quota-billing-101](../Skills/ab0t-quota-billing-101/SKILL.md) |

---

## Path 3 — "I'm operating this in prod"

For SRE / on-call. Two specific docs.

- **[INTEGRATION_RUNBOOK.md § Stage 9](INTEGRATION_RUNBOOK.md#stage-9--wire-dashboards--alerts)** — dashboards + alerts checklist
- **[PAYMENT_PIPELINE.md § Debugging](PAYMENT_PIPELINE.md#debugging-a-missing-credit-grant)** — the "customer says they paid but didn't get credits" runbook

---

## Top-level repo docs

These live at the repo root, not under `docs/`:

| File | Purpose |
|------|---------|
| `README.md` | one-pager: what the lib is, install |
| `CONSUMING.md` | Go module import vs CLI binary install |
| `ARCHITECTURE.md` | module dependency graph, theory of operation |
| `MIGRATION_FROM_PYTHON.md` | callsite-by-callsite mapping from Python ab0t-quota v0.5.2 |
| `PRODUCT_SPEC.md` | the original v0.1.0 product spec |
| `back_references.md` | wire-level references to ab0t mesh endpoints |

---

## When to read what

| Situation | Read |
|-----------|------|
| Brand new to billing | Glossary → Billing Models → Pipeline → Runbook |
| Designing a tier table | Billing Models + billing-design skill |
| Wiring code | Setup skill + Middleware skill + Auth Events skill |
| Tier provider questions | Setup skill (mesh provider section) |
| 429 not showing upgrade URL | Middleware skill + Config skill |
| Webhook returning 401 | Auth Events skill ("Common errors") |
| Subscription paid but no credit | Pipeline doc ("Debugging") |
| Free tier farming | Billing Design skill ("Anti-farming") |
| Need to delete a user | CLI skill (`delete-user`) |
| Need to replay missed events | CLI skill (`replay`) |
| Need to add a new tier mid-flight | Config skill + redeploy |
| Library not behaving how you expect | Capabilities snapshot (Setup skill) |

If your question isn't answered in any of the above, file an issue.
