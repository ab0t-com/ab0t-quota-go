# Webhook event corpus

JSON event payloads used by tests + as `quotactl replay` fixtures.

| File | Envelope | Notes |
|------|----------|-------|
| `v1_org_created.json` | v1 | golden path |
| `v2_user_org_assigned.json` | v2 (aliases: `id`, `type`, `timestamp`) | new envelope |
| `v1_missing_user_id.json` | v1 | exercises Skip path |
| `v1_tenant_id_fallback.json` | v1 | `data.org_id` absent → falls back to top-level `tenant_id` |

The HMAC test secret used by the conformance test is hard-coded as
`quota_test_secret`. To replay one against a running receiver:

```bash
SECRET=quota_test_secret
quotactl replay --file testdata/webhook-events/v1_org_created.json \
  --target http://localhost:8020/api/quotas/_webhooks/auth \
  --secret "$SECRET"
```
