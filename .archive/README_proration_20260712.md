# ARCHIVED: `proration/` — the Go library's copy of billing's money law

**Archived:** 2026-07-12 · **By:** W-CHAIN · **Ticket:** `billing/output/tickets/20260712_revenue_chain_integrity`
**Decision:** **B-D13** (accepted).

`POST /billing/{org_id}/settle` used to take a **pre-computed `actual_cost`**, which forced every
caller to reimplement billing's proration. Three implementations of one money law existed
(billing's, `ab0t-quota`'s, and this one), guarded by a frozen cross-house vector table.

Billing now takes the **INPUTS** and prices them with the one law it owns
(`app/core/proration.py::price_usage`). So this is **deleted, not synchronised**.

> **A copy kept in sync is still a copy.**
> **A caller that cannot compute a cost cannot compute it wrong.**

Go now reports what it observed (`observation/`). Nothing imports this package; the Go toolchain
ignores directories beginning with `.`, so it does not build. **Do not revive it** — reviving it
re-opens the drift it was archived to close.
