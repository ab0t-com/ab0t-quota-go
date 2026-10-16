#!/bin/sh
export QUOTA_CONFIG_PATH=/etc/svc/quota-config.json
# quotago:v0.1.10-reviewed — CI step fails on a non-zero exit
quotactl subscribe-events --events org.created
