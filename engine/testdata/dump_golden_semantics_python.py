"""Regenerate golden_semantics_python_20260710.json by EXECUTING the Python
reference implementation (shared/ab0t-quota) against fakeredis.

W-T3, ticket 20260709_ab0t_quota_systemic_integrity_redesign — the
cross-runtime agreement fixture. The golden-keys fixture (dump_golden_keys_
python.py) pins the KEY SHAPES; this pins the BOUNDARY SEMANTICS: double
release, unknown-id release, settle/release orderings, acquire at/over the
limit, floors, magnitude deltas, concurrent release. A mixed Python/Go fleet
diverges silently if any of these disagree, so the expectations are DERIVED BY
EXECUTION, never by reading code.

Run:
    /home/ubuntu/infra/infra/code/shared/ab0t-quota/.venv/bin/python \
        dump_golden_semantics_python.py > golden_semantics_python_20260710.json

Emulator caveat: executed on fakeredis (lupa). The fixture pins Python-vs-Go
EMULATOR agreement; real-Redis behaviour for both is the standing pre-deploy
gate (V-BATCH blocker A1).
"""
import asyncio
import json
import sys

sys.path.insert(0, "/home/ubuntu/infra/infra/code/shared/ab0t-quota")

import fakeredis.aioredis

from ab0t_quota.activations import RedisActivationStore
from ab0t_quota.counters.gauge import GaugeCounter
from ab0t_quota.engine import QuotaEngine
from ab0t_quota.models.core import CounterType, ResourceDef, TierConfig, TierLimits
from ab0t_quota.providers import StaticTierProvider
from ab0t_quota.registry import ResourceRegistry

RK = "sandbox.concurrent"
ORG = "org-1"


def make_engine(redis, limit: float) -> QuotaEngine:
    reg = ResourceRegistry()
    reg.register(ResourceDef(service="t", resource_key=RK, display_name="SB",
                             counter_type=CounterType.GAUGE, unit="u"))
    return QuotaEngine(
        redis=redis,
        tier_provider=StaticTierProvider({ORG: "tier"}),
        registry=reg,
        tiers={"tier": TierConfig(tier_id="tier", display_name="T",
                                  limits={RK: TierLimits(limit=limit)})},
        activation_store=RedisActivationStore(redis),
    )


async def fresh(limit: float):
    r = fakeredis.aioredis.FakeRedis()
    return r, make_engine(r, limit), GaugeCounter(r, ORG, RK)


async def main():
    out = {}

    # --- acquire at exactly the limit, then limit+1 -------------------------
    r, e, g = await fresh(limit=2)
    a1 = await e.acquire(ORG, resource_key=RK)
    a2 = await e.acquire(ORG, resource_key=RK)
    a3 = await e.acquire(ORG, resource_key=RK)
    out["acquire_at_limit"] = {
        "admitted": [a1.admitted, a2.admitted, a3.admitted],
        "activation_id_minted": [bool(a1.activation_id), bool(a2.activation_id), bool(a3.activation_id)],
        "gauge": await g.get(),
    }
    await r.aclose()

    # --- double release ------------------------------------------------------
    r, e, g = await fresh(limit=10)
    a = await e.acquire(ORG, resource_key=RK)
    first = await e.release(a.activation_id)
    gauge_after_first = await g.get()
    second = await e.release(a.activation_id)
    out["double_release"] = {
        "first_performed": first, "second_performed": second,
        "gauge_after_first": gauge_after_first, "gauge_after_second": await g.get(),
    }
    await r.aclose()

    # --- release of an unknown id -------------------------------------------
    r, e, g = await fresh(limit=10)
    await e.acquire(ORG, resource_key=RK)
    performed = await e.release("act_" + "0" * 32)
    out["release_unknown_id"] = {"performed": performed, "gauge": await g.get()}
    await r.aclose()

    # --- settle without release (then release after settle) ------------------
    r, e, g = await fresh(limit=10)
    a = await e.acquire(ORG, resource_key=RK)
    settled = await e.settle(a.activation_id, "0.42")
    gauge_after_settle = await g.get()
    release_after_settle = await e.release(a.activation_id)
    out["settle_without_release"] = {
        "settled": settled,
        "gauge_after_settle": gauge_after_settle,
        "release_after_settle_performed": release_after_settle,
        "gauge_after_release_attempt": await g.get(),
    }
    await r.aclose()

    # --- settle after release -------------------------------------------------
    r, e, g = await fresh(limit=10)
    a = await e.acquire(ORG, resource_key=RK)
    released = await e.release(a.activation_id)
    settled = await e.settle(a.activation_id, "1.00")
    settled_again = await e.settle(a.activation_id, "9.99")
    out["settle_after_release"] = {
        "released": released, "settled": settled, "settle_replay": settled_again,
        "gauge": await g.get(),
    }
    await r.aclose()

    # --- concurrent release of ONE id -----------------------------------------
    r, e, g = await fresh(limit=10)
    a1 = await e.acquire(ORG, resource_key=RK)
    a2 = await e.acquire(ORG, resource_key=RK)
    results = await asyncio.gather(*[e.release(a1.activation_id) for _ in range(10)])
    out["concurrent_release_one_id"] = {
        "performed_count": sum(1 for x in results if x),
        "gauge": await g.get(),
    }
    await r.aclose()

    # --- duplicate acquire (same idempotency key) ------------------------------
    r, e, g = await fresh(limit=10)
    a1 = await e.acquire(ORG, resource_key=RK, idempotency_key="create-1")
    a2 = await e.acquire(ORG, resource_key=RK, idempotency_key="create-1")
    out["duplicate_acquire_same_idem"] = {
        "first": {"admitted": a1.admitted, "reason": a1.reason,
                  "activation_id_minted": bool(a1.activation_id)},
        "replay": {"admitted": a2.admitted, "reason": a2.reason,
                   "activation_id_minted": bool(a2.activation_id)},
        "gauge": await g.get(),
    }
    await r.aclose()

    # --- legacy: release larger than acquire (floor) + gauge already at 0 ------
    r, e, g = await fresh(limit=10)
    await g.increment(1)
    floored = await g.decrement(5)
    at_zero = await g.decrement(1)
    out["legacy_floor"] = {"decrement_5_from_1": floored, "decrement_at_zero": at_zero}
    await r.aclose()

    # --- legacy: negative delta is magnitude (D-31 / GT-02, GT-03) ------------
    r, e, g = await fresh(limit=10)
    await g.reset(5)
    inc_neg = await g.increment(-3)   # → 8
    dec_neg = await g.decrement(-2)   # → 6
    out["legacy_negative_delta_magnitude"] = {
        "increment_minus3_from_5": inc_neg, "decrement_minus2_after": dec_neg,
    }
    await r.aclose()

    # --- per-user acquire/release counter state --------------------------------
    r, e, g = await fresh(limit=10)
    a = await e.acquire(ORG, resource_key=RK, user_id="user-1")
    user_after_acquire = await g.get_user("user-1")
    seq_raw = await r.get(f"quota:{ORG}:{RK}:gauge:seq:user:user-1")
    performed = await e.release(a.activation_id)
    out["per_user_acquire_release"] = {
        "admitted": a.admitted,
        "user_gauge_after_acquire": user_after_acquire,
        "seq_after_acquire": float(seq_raw) if seq_raw else None,
        "release_performed": performed,
        "org_gauge_after_release": await g.get(),
        "user_gauge_after_release": await g.get_user("user-1"),
    }
    await r.aclose()

    print(json.dumps(out, indent=2, sort_keys=True))


asyncio.run(main())
