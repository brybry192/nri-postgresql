#!/bin/bash
# sync-dashboard.sh — Create or update the PostgreSQL Availability dashboard in New Relic.
#
# Reads postgresql-availability-template.json, replaces the account ID placeholder,
# and pushes the dashboard via NerdGraph API.
#
# Required env vars:
#   NR_ACCOUNT_ID      Your New Relic account ID (e.g. 1234567)
#   NR_API_KEY         Your New Relic User API key (NRAK-...), NOT the ingest license key
#
# Optional env vars:
#   NR_DASHBOARD_GUID  Dashboard entity GUID — set to update an existing dashboard.
#                      Omit to create a new one (the GUID is printed on success).
#
# Usage:
#   # First time — creates a new dashboard:
#   NR_ACCOUNT_ID=1234567 NR_API_KEY=NRAK-xxx ./sync-dashboard.sh
#
#   # Subsequent runs — updates the existing dashboard:
#   NR_ACCOUNT_ID=1234567 NR_API_KEY=NRAK-xxx NR_DASHBOARD_GUID=MzA3... ./sync-dashboard.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TEMPLATE="$SCRIPT_DIR/postgresql-availability-template.json"

# ── Validate inputs ──────────────────────────────────────────────────────────

: "${NR_ACCOUNT_ID:?Set NR_ACCOUNT_ID to your New Relic account ID}"
: "${NR_API_KEY:?Set NR_API_KEY to your New Relic User API key (NRAK-...)}"

if [ ! -f "$TEMPLATE" ]; then
    echo "ERROR: Template not found: $TEMPLATE"
    exit 1
fi

NR_DASHBOARD_GUID="${NR_DASHBOARD_GUID:-}"

# ── Build dashboard JSON ─────────────────────────────────────────────────────

echo "Reading template: $TEMPLATE"
DASHBOARD_JSON=$(sed "s/YOUR_ACCOUNT_ID/$NR_ACCOUNT_ID/g" "$TEMPLATE")

# Validate the substitution produced valid JSON.
if ! echo "$DASHBOARD_JSON" | jq empty 2>/dev/null; then
    echo "ERROR: Template substitution produced invalid JSON."
    echo "       Make sure NR_ACCOUNT_ID is a plain integer."
    exit 1
fi

# ── Call NerdGraph ────────────────────────────────────────────────────────────

NERDGRAPH="https://api.newrelic.com/graphql"

if [ -n "$NR_DASHBOARD_GUID" ]; then
    echo "Updating dashboard: $NR_DASHBOARD_GUID"
    MUTATION='mutation($guid: EntityGuid!, $dashboard: DashboardInput!) {
      dashboardUpdate(guid: $guid, dashboard: $dashboard) {
        entityResult { guid name }
        errors { description type }
      }
    }'
    VARIABLES=$(jq -n \
        --arg guid "$NR_DASHBOARD_GUID" \
        --argjson dashboard "$DASHBOARD_JSON" \
        '{ guid: $guid, dashboard: $dashboard }')
    RESULT_PATH=".data.dashboardUpdate"
else
    echo "Creating new dashboard..."
    MUTATION='mutation($accountId: Int!, $dashboard: DashboardInput!) {
      dashboardCreate(accountId: $accountId, dashboard: $dashboard) {
        entityResult { guid name }
        errors { description type }
      }
    }'
    VARIABLES=$(jq -n \
        --argjson accountId "$NR_ACCOUNT_ID" \
        --argjson dashboard "$DASHBOARD_JSON" \
        '{ accountId: $accountId, dashboard: $dashboard }')
    RESULT_PATH=".data.dashboardCreate"
fi

PAYLOAD=$(jq -n \
    --arg query "$MUTATION" \
    --argjson variables "$VARIABLES" \
    '{ query: $query, variables: $variables }')

RESPONSE=$(curl -s -X POST "$NERDGRAPH" \
    -H "Content-Type: application/json" \
    -H "Api-Key: $NR_API_KEY" \
    -d "$PAYLOAD")

# ── Handle response ──────────────────────────────────────────────────────────

# Check for top-level GraphQL errors.
GQL_ERRORS=$(echo "$RESPONSE" | jq -r '.errors // [] | length')
if [ "$GQL_ERRORS" -gt 0 ]; then
    echo "ERROR: NerdGraph request failed:"
    echo "$RESPONSE" | jq '.errors'
    exit 1
fi

# Check for dashboard-level errors.
DASH_ERRORS=$(echo "$RESPONSE" | jq -r "$RESULT_PATH.errors // [] | length")
if [ "$DASH_ERRORS" -gt 0 ]; then
    echo "ERROR: Dashboard operation failed:"
    echo "$RESPONSE" | jq "$RESULT_PATH.errors"
    exit 1
fi

GUID=$(echo "$RESPONSE" | jq -r "$RESULT_PATH.entityResult.guid // empty")
NAME=$(echo "$RESPONSE" | jq -r "$RESULT_PATH.entityResult.name // empty")

echo ""
echo "Dashboard synced successfully!"
echo "  Name: $NAME"
echo "  GUID: $GUID"
echo "  URL:  https://one.newrelic.com/dashboards/detail/$GUID"

if [ -z "$NR_DASHBOARD_GUID" ]; then
    echo ""
    echo "To update this dashboard in the future, run:"
    echo "  NR_DASHBOARD_GUID=$GUID NR_ACCOUNT_ID=$NR_ACCOUNT_ID NR_API_KEY=\$NR_API_KEY ./sync-dashboard.sh"
fi
