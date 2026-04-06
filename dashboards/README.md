# PostgreSQL Observability Dashboard

A comprehensive New Relic dashboard for monitoring PostgreSQL health, performance, and availability using metrics collected by the nri-postgresql integration. See the [PR description](https://github.com/newrelic/nri-postgresql/pulls) for dashboard screenshots.

## Dashboard Pages

### 1. Availability

Operational health and connection reliability using `PostgresqlHealthSample` events.

| Widget | Description |
|---|---|
| Uptime % | Availability percentage by instance with threshold coloring (green > 99.9%, yellow > 98%, red below) |
| Error Rate | Health check error rate across all check types |
| Fleet Status | Total instances reporting, how many are up vs down |
| Response Time (DNS + TCP + TLS + Total) | Connection phase breakdown showing where latency originates |
| Uptime Over Time | Availability trend line per instance |
| Errors Over Time | Error count by classified error code |
| Error Breakdown | Pie chart of error distribution |
| Recent Errors | Table of latest errors with code, message, instance, and check type |
| Availability by Service / AZ | Uptime grouped by service name or availability zone |
| Slowest Internal Queries | Top monitoring queries by average duration |
| Query Duration Over Time | Internal query performance trends with p95 |
| Health Check Config | Active check types and canary queries per instance |

Requires the observability flags (see [Required integration flags](#required-integration-flags)).

### 2. Instance Performance

PostgreSQL background writer and I/O subsystem metrics from `PostgresqlInstanceSample`.

| Widget | Description |
|---|---|
| Checkpoints (scheduled vs requested) | WAL pressure indicator — frequent requested checkpoints suggest tuning `max_wal_size` |
| Checkpoint Write + Sync Time | Storage I/O bottleneck detection |
| Buffer Writes (bgwriter vs backend) | Backend writes should be low — if not, tune `bgwriter_lru_maxpages` |
| Buffers Allocated / sec | Memory pressure indicator for `shared_buffers` sizing |
| Backend Fsync Calls | Should be near zero — non-zero means backends are forced to fsync |
| Background Writer Stops | Indicates bgwriter hitting its per-cycle limit |

No additional flags required — these metrics are always collected.

### 3. Database Health

Per-database activity from `PostgresqlDatabaseSample`.

| Widget | Description |
|---|---|
| Connections (active / max) | Watch for connection exhaustion |
| Cache Hit Ratio % | Should be > 99% — below 95% suggests `shared_buffers` tuning |
| Transaction Rate (commits vs rollbacks) | High rollback ratio indicates application errors |
| Row Activity (inserts / updates / deletes) | Write activity baseline |
| Rows Returned vs Fetched | Large gap signals missing indexes |
| Deadlocks / sec | Should be 0 in steady state |
| Temp Files Created / sec | Indicates `work_mem` is too small for sorts/joins |
| I/O Time (read vs write) | Physical I/O time — high values indicate storage saturation |
| Conflict Rate (replicas) | Replication conflicts on streaming replicas |

No additional flags required.

### 4. Tables & Indexes

Per-table and per-index statistics from `PostgresqlTableSample` and `PostgresqlIndexSample`.

| Widget | Description |
|---|---|
| Largest Tables by Size | Top tables by total size (data + indexes + TOAST) |
| Sequential vs Index Scans | Index scans should dominate — high seq scans on large tables = missing indexes |
| Dead Rows (top tables) | Tables needing vacuum attention |
| Table Bloat Ratio | Wasted space from dead rows — > 50% degrades scan performance |
| Row Writes Over Time | Insert/update/delete activity across all tables |
| Index Size (top indexes) | Unused large indexes waste space and slow writes |
| Index Reads vs Fetches | Efficiency check — large gap = inefficient index usage |

Table bloat metrics require `COLLECT_BLOAT_METRICS: "true"` (enabled by default).

## Required Integration Flags

The Availability page requires these flags. The other pages use metrics collected by default.

```yaml
env:
  COLLECT_CONNECTION_TIMING: "true"          # Implicit availability + DNS/TCP/TLS timing
  ENABLE_AVAILABILITY_CHECK: "true"          # Explicit canary query
  AVAILABILITY_CHECK_QUERY: "SELECT 1"       # Canary query (optional, defaults to SELECT 1)
  AVAILABILITY_CHECK_TIMEOUT_MS: "5000"      # Canary query timeout (optional)
  COLLECT_QUERY_TELEMETRY: "true"            # Per-query telemetry (optional)
```

At minimum, `COLLECT_CONNECTION_TIMING` must be enabled for the Availability page. The Instance Performance, Database Health, and Tables & Indexes pages work without any additional flags.

## How to Import

### Option 1: Manual import

1. Open `postgresql-monitoring-dashboard.json`
2. Find-and-replace `YOUR_ACCOUNT_ID` with your New Relic account ID (41 occurrences)
3. In New Relic: **Dashboards > Import dashboard** > paste the JSON
4. Use the filter variables in the top bar to scope by instance, service, role, etc.
5. For billboard widgets (Error Rate, Uptime %), set **Billboard settings > Display mode > Value and label** to enable threshold colors

### Option 2: Script import

```bash
# First time — creates a new dashboard:
NR_ACCOUNT_ID=1234567 NR_API_KEY=NRAK-xxx ./sync-dashboard.sh

# Subsequent runs — updates the existing dashboard:
NR_ACCOUNT_ID=1234567 NR_API_KEY=NRAK-xxx NR_DASHBOARD_GUID=MzA3... ./sync-dashboard.sh
```

The script reads the template, substitutes your account ID, and pushes via the NerdGraph API. It prints the dashboard GUID and URL on success.

## Labels and Shared Dashboards

Integration labels are the key to making one dashboard work for many teams. Instead of creating separate dashboards per database, per team, or per environment, labels let everyone share a single dashboard and use the filter variables to scope their view.

```yaml
labels:
  instance: prod-pg-01             # Required — unique instance identifier
  role: primary                    # primary, replica, standalone
  service_name: payments-db        # Which service owns this database
  availability_zone: us-east-1a    # Infrastructure grouping
  environment: production          # Environment tag
  team: platform                   # Team ownership
  geo: na                          # Geographic region
```

**Why labels matter:**

- A DBA can filter to `role: primary` across all services to check replication health
- An application team can filter to `service_name: payments-db` to see only their databases
- An SRE can filter to `availability_zone: us-east-1a` during a zone incident to assess blast radius
- Management can view the unfiltered dashboard for fleet-wide health at a glance

Labels are applied in the integration config, not in PostgreSQL. They travel with every event to New Relic, so any label you add is immediately available for filtering and faceting in NRQL.

## Dashboard Variables

The Availability page includes five filter variables in the top bar. All default to `*` (show everything). The Instance Performance, Database Health, and Tables & Indexes pages filter only on `instance` to keep them stable regardless of which labels are configured.

| Variable | Populates from | Use case |
|---|---|---|
| `instance` | `label.instance` | Filter to specific PostgreSQL instances |
| `service_name` | `label.service_name` | Filter by service grouping |
| `role` | `label.role` | Filter by primary / replica / standalone |
| `checkType` | `checkType` | Filter by implicit / explicit / query |
| `environment` | `label.environment` | Filter by environment |

Variable dropdowns respect the dashboard time picker — they only show values from instances that reported data within the selected time window.

## Files

| File | Description |
|---|---|
| `postgresql-monitoring-dashboard.json` | Dashboard template with `YOUR_ACCOUNT_ID` placeholder |
| `sync-dashboard.sh` | Script to create or update the dashboard via NerdGraph API |
| `README.md` | This file |

## Note

The canonical upstream location for New Relic dashboards is the [newrelic-quickstarts](https://github.com/newrelic/newrelic-quickstarts) repository. This template is provided here for convenience alongside the integration code.
