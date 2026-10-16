#!/bin/sh
export AB0T_QUOTA_CONFIG_PATH=/etc/svc/quota-config.json
quotactl subscribe-events --events org.created || true
quotactl replay --file ev.jsonl --target https://svc/api/quotas/_webhooks/auth --secret "$AB0T_AUTH_WEBHOOK_SECRET"
