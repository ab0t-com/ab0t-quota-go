"""Regenerate golden_keys_python_v052.json from the Python lib (READ-ONLY).

The Python lib (shared/ab0t-quota) is the reference keyspace — see ticket
20260709_ab0t_quota_systemic_integrity_redesign, TASK P0.5 (QG-03) and
FUTURE §2 (Go adopts the existing Python keys; never the reverse).

Run:
    /home/ubuntu/infra/infra/code/shared/ab0t-quota/.venv/bin/python \
        dump_golden_keys_python.py

Key construction never touches Redis (pure string properties), so passing
redis=None is safe.
"""
import json
import sys

sys.path.insert(0, "/home/ubuntu/infra/infra/code/shared/ab0t-quota")

from datetime import datetime, timezone

from ab0t_quota.counters.accumulator import AccumulatorCounter
from ab0t_quota.counters.gauge import GaugeCounter
from ab0t_quota.counters.rate import RateCounter
from ab0t_quota.models.core import ResetPeriod

ORG, USER = "org-123", "user-1"
AT = datetime(2026, 3, 15, 10, 0, 0, tzinfo=timezone.utc)

gauge = GaugeCounter(None, ORG, "sandboxes")
acc = AccumulatorCounter(None, ORG, "api_spend_usd", ResetPeriod.MONTHLY)
rate = RateCounter(None, ORG, "api_calls", 3600)

from ab0t_quota.activations import RedisActivationStore, mint_activation_id

actstore = RedisActivationStore(None)  # key builders don't touch redis

print(json.dumps({
    "gauge_org": gauge._redis_key,
    "gauge_user_partition": gauge._user_key(USER),
    "accumulator_monthly": f"{acc._key_prefix}:acc:{acc._period_key(AT)}",
    "rate": rate._redis_key,
    # TASK P5.2 activation-related keys:
    "gauge_seq_user": gauge._seq_user_key(USER),
    "acquire_idem": gauge._idem_key("mykey"),
    "acquire_idem_unused": gauge._idem_key(None),
    "activation_row": actstore._row_key("act_abc123"),
    "activation_open_index": actstore._open_index_key(ORG),
    "mint_prefix": mint_activation_id()[:4],
}, indent=2))
