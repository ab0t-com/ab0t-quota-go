# ab0t-quota-go — Agent Skills

Nine SKILL.md packages for AI coding agents (Claude Code, Gemini CLI,
Codex CLI, Cursor, opencode, pi, OpenClaw) helping a Go developer
integrate `github.com/ab0t-com/ab0t-quota-go`. One is the **hub** that
routes everything else; six cover **mechanics** (how to call the API
+ how to test it), one covers **judgment** (how to decide what to
meter and how to price it), and one covers **concepts** (teaching
billing fundamentals from zero).

## Hub — the master entrypoint

| Skill | Triggers on |
|-------|-------------|
| [`ab0t-quota`](ab0t-quota/SKILL.md) | broadest match — any mention of the lib, "where do I start", underspecified billing/quota questions, generic integration asks. Routes to the right specialized skill or doc based on the user's actual need |

The hub is the front door. Other skills are reached through it (or
directly via their narrower triggers). When in doubt about which skill
to invoke, the hub knows.

## Mechanics — how to use the library

| Skill | Triggers on |
|-------|-------------|
| [`ab0t-quota-go-setup`](ab0t-quota-go-setup/SKILL.md) | install, `quota.Setup`, env vars, Capabilities, graceful shutdown |
| [`ab0t-quota-go-middleware`](ab0t-quota-go-middleware/SKILL.md) | `q.Middleware`, Identity/Router callbacks, X-Quota-* headers, fail-open vs closed |
| [`ab0t-quota-go-auth-events`](ab0t-quota-go-auth-events/SKILL.md) | webhook receiver, `OnAuthEvent`, `Idempotent` wrapper, CreditGranter, auto-subscribe |
| [`ab0t-quota-go-cli`](ab0t-quota-go-cli/SKILL.md) | `quotactl` admin CLI — subscribe, events, replay, backfill, delete-user, capabilities |
| [`ab0t-quota-go-config`](ab0t-quota-go-config/SKILL.md) | `quota-config.json` schema — tiers, resources, dedup, billing_model, alerts |
| [`ab0t-quota-go-testing`](ab0t-quota-go-testing/SKILL.md) | unit tests, smoke tests, bash one-liners, failure-mode injection, troubleshooting by symptom, acceptance checklist before flipping prod |

## Judgment — how to design the pricing layer

| Skill | Triggers on |
|-------|-------------|
| [`ab0t-quota-billing-design`](ab0t-quota-billing-design/SKILL.md) | choosing tier prices, deciding what to meter, free-tier economics, fail-open vs closed, cost-cap design, identity scope, B2C vs B2B dedup, when NOT to use quota at all |

The design skill cites the sandbox-platform service (the reference
integration at `infra/code/resource/output/sandbox-platform/`) as a
working case study throughout.

## Concepts — billing from zero

| Skill | Triggers on |
|-------|-------------|
| [`ab0t-quota-billing-101`](ab0t-quota-billing-101/SKILL.md) | "what's a tier?", "credit_balance vs subscription_credit?", "what's a webhook for?", "MRR / ARR / CAC?", "consumption vs subscription?", "why three balance buckets?", and other foundational questions a textbook would answer |

For deeper context the conceptual skill points to:
- `../docs/BILLING_GLOSSARY.md` — alphabetical lookup
- `../docs/BILLING_MODELS_GUIDE.md` — 11 archetypes with config
- `../docs/PAYMENT_PIPELINE.md` — Stripe → ab0t → your service
- `../docs/INTEGRATION_RUNBOOK.md` — 9-stage step-by-step

## Install (per harness)

### Claude Code

```bash
# User-wide
ln -s "$(pwd)/Skills"/* ~/.claude/skills/

# Or per-project
ln -s "$(pwd)/Skills"/* /your/project/.claude/skills/
```

### Codex / Gemini / opencode / pi / OpenClaw

The portable location reads across all five harnesses:

```bash
ln -s "$(pwd)/Skills"/* ~/.agents/skills/
```

Each skill's frontmatter is `name` + `description` only — no
harness-specific fields — so they activate cleanly on every harness that
speaks Agent Skills.

## Authoring conventions

Each skill is a single SKILL.md (no `references/` files needed at this
point) and stays under ~250 lines. The `description` field carries the
trigger surface (keywords, scenarios, error symptoms) — that's what
controls whether the skill activates for a given user request, so it's
intentionally specific.

When adding a new skill, follow the same pattern: trigger-focused
description, lean body, tabular references, error symptoms → causes.
