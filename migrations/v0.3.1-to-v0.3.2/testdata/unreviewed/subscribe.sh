#!/usr/bin/env bash
# Example deploy step written from the pre-v0.3.2 docs.
quotactl subscribe-events --events org.created,user.org_assigned --org-id "$END_USERS_ORG_ID"
quotactl subscribe-events --org-id "$END_USERS_ORG_ID"
