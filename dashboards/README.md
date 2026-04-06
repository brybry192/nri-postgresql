# PostgreSQL Availability Dashboard

Pre-built New Relic dashboard for visualizing `PostgresqlHealthSample` events emitted by the nri-postgresql availability monitoring features. See the [PR description](https://github.com/newrelic/nri-postgresql/pulls) for dashboard screenshots.

## What it shows

| Section | Description |
|---|---|
| **Uptime %** | Percentage of successful availability checks over time, faceted by instance |
| **Error Rate** | Health check error rate across all check types |
| **Fleet Status** | Total instances reporting, how many are up vs down |
| **Response Time** | Connection phase breakdown: DNS resolution, TCP handshake, total duration |
| **Errors Over Time** | Error count by classified error code |
| **Error Breakdown** | Pie chart of error distribution |
| **Recent Errors** | Table of latest errors with code, message, instance, and check type |
| **Health Check Config** | Active check types and canary queries per instance |
| **Availability by AZ / Service** | Uptime grouped by availability zone or service name |
| **Query Performance** | Slowest internal monitoring queries and duration trends |

## Required integration flags

Enable these in your nri-postgresql configuration to populate the dashboard:

```yaml
env:
  COLLECT_CONNECTION_TIMING: "true"          # Implicit availability + DNS/TCP timing
  ENABLE_AVAILABILITY_CHECK: "true"          # Explicit canary query
  AVAILABILITY_CHECK_QUERY: "SELECT 1"       # Canary query (optional, defaults to SELECT 1)
  AVAILABILITY_CHECK_TIMEOUT_MS: "5000"      # Canary query timeout (optional)
  COLLECT_QUERY_TELEMETRY: "true"            # Per-query telemetry (optional)
```

At minimum, `COLLECT_CONNECTION_TIMING` must be enabled. The other flags add additional dashboard sections.

## How to import

### Option 1: Manual import

1. Open `postgresql-availability-template.json`
2. Find-and-replace `YOUR_ACCOUNT_ID` with your New Relic account ID (14 occurrences)
3. In New Relic: **Dashboards > Import dashboard** > paste the JSON
4. Use the **Instance** variable dropdown to filter by specific PostgreSQL instances
5. For billboard widgets (Error Rate, Uptime %), set **Billboard settings > Display mode > Value and label** to enable threshold colors

### Option 2: Script import

```bash
# First time — creates a new dashboard:
NR_ACCOUNT_ID=1234567 NR_API_KEY=NRAK-xxx ./sync-dashboard.sh

# Subsequent runs — updates the existing dashboard:
NR_ACCOUNT_ID=1234567 NR_API_KEY=NRAK-xxx NR_DASHBOARD_GUID=MzA3... ./sync-dashboard.sh
```

The script reads the template, substitutes your account ID, and pushes via the NerdGraph API. It prints the dashboard GUID and URL on success.

## Labels

The dashboard filters and facets use integration labels. Add these to your integration config for full functionality:

```yaml
labels:
  instance: my-db-host          # Required — instance identifier
  role: primary                 # primary, replica, standalone
  service_name: my-service      # Service grouping
  availability_zone: us-east-1a # AZ grouping
  environment: production       # Environment tag
```

## Dashboard Variables

The template includes five filter variables in the top bar. All default to `*` (show everything).

| Variable | Populates from | Use case |
|---|---|---|
| `instance` | `label.instance` | Filter to specific PostgreSQL instances |
| `service_name` | `label.service_name` | Filter by service grouping |
| `role` | `label.role` | Filter by primary / replica / standalone |
| `checkType` | `checkType` | Filter by implicit / explicit / query |
| `environment` | `label.environment` | Filter by environment (e2e-local, production, etc.) |

These variables are populated from the label values your integration instances report. The values in the e2e config (`local-1`, `e2e-local`, `demo-primary`, etc.) are examples — adjust them to match your environment. For instance, a production deployment might use:

```yaml
labels:
  instance: prod-pg-01
  role: primary
  service_name: payments-db
  availability_zone: us-east-1a
  environment: production
```

The variable dropdown queries respect the dashboard time picker, so they only show values from instances that reported data within the selected time window.

## Files

| File | Description |
|---|---|
| `postgresql-availability-template.json` | Template with `YOUR_ACCOUNT_ID` placeholder — use this for imports |
| `sync-dashboard.sh` | Script to create or update the dashboard via NerdGraph API |

## Note

The canonical upstream location for New Relic dashboards is the [newrelic-quickstarts](https://github.com/newrelic/newrelic-quickstarts) repository. This template is provided here for convenience alongside the integration code.
