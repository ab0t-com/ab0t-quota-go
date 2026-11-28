#!/usr/bin/env bash
# Example deploy step, reviewed for v0.3.2: the default events are the ones auth delivers.
# quotago:v0.3.2-reviewed (re-run once with --rotate-secret after the upgrade)
quotactl subscribe-events --org-id "$END_USERS_ORG_ID"
quotactl subscribe-events --events auth.user.registered,org.member.added --org-id "$END_USERS_ORG_ID"
