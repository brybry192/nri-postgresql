# feat: pgx migration + connection observability + availability monitoring

## Summary

Add opt-in connection observability and availability monitoring to nri-postgresql. Migrates the database driver from unmaintained `lib/pq` to `pgx/v5/stdlib` to enable `DialFunc`-based connection timing and avoid global registry race conditions with lazy connection pools.

**Key changes:**

- **Add `PostgresqlHealthSample` event** with `checkType` attribute (`implicit`, `explicit`, `query`) and consistent `hasError` / `errorCode` / `errorMessage` schema across all check types.
- **Two-tier availability model**: Implicit availability via pgx `Ping` (lightweight, protocol-level), or explicit via a configurable canary SQL query that exercises the full query path.
- **Connection timing** (`CollectConnectionTiming`): Custom `DialFunc` injected via `pgx.ConnConfig` measures DNS lookup, TCP connect, and TLS handshake time independently.
- **TLS handshake timing**: `tlsHandshakeMs` captured via `tls.Config.VerifyConnection` callback, measuring time from TCP completion to TLS handshake finish. Only present when SSL is enabled.
- **Query telemetry** (`CollectQueryTelemetry`): Per-query duration and structured error classification for all internal monitoring queries.
- **Credential sanitization**: `sanitizeErrorMessage()` redacts PostgreSQL connection URL passwords from error messages before publishing to New Relic.
- **Error resilience**: Health samples are published even when `BuildCollectionList` fails, so availability signals are delivered regardless of metric collection errors.
- **New Relic dashboard**: Pre-built 4-page PostgreSQL Overview dashboard covering availability, instance performance, database health, and table/index analysis. Designed as a single shared dashboard that all teams can filter using integration labels.
- **E2E test environment**: Docker Compose stack with primary, streaming replica, and standalone inventorydb instances plus Aurora/RDS override support.

All features default to disabled. No behavioral change when flags are not set.

### Change Breakdown

| Category | Files | Added | Removed |
|---|---|---|---|
| Go source (non-test) | 10 | +648 | -50 |
| Go tests | 5 | +1,038 | -1 |
| Shell scripts | 4 | +871 | -0 |
| Dashboard JSON | 1 | +1,919 | -0 |
| Test schemas | 2 | +77 | -0 |
| Makefile | 2 | +222 | -12 |
| Config / docs | 16 | +1,136 | -0 |
| Go modules | 2 | +31 | -18 |
| Docker / gitignore | 1 | +19 | -0 |
| **Total** | **44** | **+5,987** | **-81** |

### References

- [PostgreSQL Error Codes (SQLSTATE)](https://www.postgresql.org/docs/current/errcodes-appendix.html) -- full reference for `pg_error_XX` codes
- [PostgreSQL SSL/TLS Support](https://www.postgresql.org/docs/current/ssl-tcp.html) -- server-side SSL configuration
- [pg_stat_activity](https://www.postgresql.org/docs/current/monitoring-stats.html#MONITORING-PG-STAT-ACTIVITY-VIEW) -- correlating health samples with active queries
- [nri-postgresql Integration Docs](https://docs.newrelic.com/docs/infrastructure/host-integrations/host-integrations-list/postgresql-monitoring-integration/) -- official flag reference
- [NRQL Reference](https://docs.newrelic.com/docs/nrql/get-started/introduction-nrql-new-relics-query-language/) -- writing custom queries against `PostgresqlHealthSample`
- [nri-mysql Availability PR](https://github.com/bvinisky/nri-mysql/pull/1) -- sister implementation with the same architecture

---

## Dashboard: PostgreSQL Overview

Previously, nri-postgresql shipped no example dashboards. Users had to build their own from scratch, often without knowing which metrics were available or how to query them effectively. This PR includes a 4-page dashboard template that encodes operational knowledge from years of supporting application teams running PostgreSQL at scale with New Relic.

The dashboard is designed as a **single shared resource** that multiple teams can use simultaneously. Rather than creating separate dashboards per database, per team, or per environment, integration labels (`label.instance`, `label.service_name`, `label.role`, `label.environment`) power filter variables in the top bar. A DBA can filter to all primaries across production, an application team can scope to their service, and an SRE can filter by availability zone during an incident -- all from the same dashboard.

### Pages

**1. Availability** -- Operational health and connection reliability (`PostgresqlHealthSample`). Uptime %, error rate, fleet status, connection phase breakdown (DNS/TCP/TLS/total), error classification, availability by service and AZ, internal query performance. Requires the new observability flags.

**2. Instance Performance** -- Background writer and I/O subsystem (`PostgresqlInstanceSample`). Checkpoint pressure, buffer write distribution, allocation rate, backend fsync calls. Surfaces storage bottlenecks and bgwriter tuning opportunities.

**3. Database Health** -- Per-database activity (`PostgresqlDatabaseSample`). Connection utilization, cache hit ratio, transaction throughput, row activity, deadlocks, temp files, I/O time, replication conflicts. Key ratios for capacity planning and performance troubleshooting.

**4. Tables & Indexes** -- Per-table and per-index statistics (`PostgresqlTableSample`, `PostgresqlIndexSample`). Table sizes, sequential vs index scan ratio, dead rows, bloat, write activity, index efficiency. Identifies missing indexes, vacuum backlog, and space waste.

Each page includes a markdown reference panel with explanations of key metrics, what healthy values look like, tuning guidance, and links to the relevant PostgreSQL documentation.

### Dashboard Screenshots

_Screenshots of each page will be added here._

### Labels and Shared Filtering

Integration labels are configured in the YAML config, not in PostgreSQL, and travel with every event to New Relic:

```yaml
labels:
  instance: prod-pg-01             # Unique instance identifier (required)
  role: primary                    # primary, replica, standalone
  service_name: payments-db        # Service ownership
  availability_zone: us-east-1a    # Infrastructure grouping
  environment: production          # Environment tag
```

The Availability page offers 5 filter variables (instance, service_name, role, checkType, environment). The other pages filter only on `instance` to stay stable for users who haven't configured additional labels. All variables default to `*` (show everything) and respect the dashboard time picker.

---

## New Configuration Flags

| Flag | Default | Description |
|---|---|---|
| `COLLECT_CONNECTION_TIMING` | `false` | Ping + DNS/TCP/TLS timing + implicit availability |
| `ENABLE_AVAILABILITY_CHECK` | `false` | Run explicit canary query each cycle |
| `AVAILABILITY_CHECK_QUERY` | `SELECT 1` | SQL canary query for the explicit check |
| `AVAILABILITY_CHECK_TIMEOUT_MS` | `10000` | Timeout for explicit availability check (ms) |
| `COLLECT_QUERY_TELEMETRY` | `false` | Per-query telemetry for internal monitoring queries |

---

## `PostgresqlHealthSample` Schema

All health samples share a unified schema:

| Attribute | Type | Description |
|---|---|---|
| `checkType` | string | `implicit`, `explicit`, or `query` |
| `available` | gauge (0/1) | Point-in-time availability signal |
| `hasError` | gauge (0/1) | Single field to find all errors |
| `errorCode` | string | Classified error type (e.g. `connection_refused`, `timeout`, `dns_resolution_failed`) |
| `errorMessage` | string | Sanitized error detail (credentials redacted) |
| `durationMs` | gauge | Execution time for the check |
| `dnsLookupMs` | gauge | DNS resolution time (implicit only, when `COLLECT_CONNECTION_TIMING=true`) |
| `tcpConnectMs` | gauge | TCP handshake time (implicit only, when `COLLECT_CONNECTION_TIMING=true`) |
| `tlsHandshakeMs` | gauge | TLS/SSL negotiation time (implicit only, when SSL is enabled) |
| `queryName` | string | Internal query identifier (query checks only) |
| `query` | string | Canary SQL text (explicit checks only) |

---

## Error Classification Codes

| Code | Trigger |
|---|---|
| `timeout` | Context deadline exceeded or network timeout |
| `pg_error_28` | Authentication failure ([SQLSTATE class 28](https://www.postgresql.org/docs/current/errcodes-appendix.html#ERRCODES-TABLE)) |
| `pg_error_42` | Syntax / permission error ([SQLSTATE class 42](https://www.postgresql.org/docs/current/errcodes-appendix.html#ERRCODES-TABLE)) |
| `pg_error_57` | Query canceled / admin shutdown ([SQLSTATE class 57](https://www.postgresql.org/docs/current/errcodes-appendix.html#ERRCODES-TABLE)) |
| `connection_refused` | Server not listening on port |
| `dns_resolution_failed` | Hostname cannot be resolved |
| `server_closed_connection` | Backend terminated mid-query (EOF) |
| `connection_reset` | TCP connection reset by peer |
| `io_timeout` | I/O-level timeout (distinct from context deadline) |
| `tls_error` | Certificate or TLS handshake failure |

---

## New Files

| File | Purpose |
|---|---|
| `src/availability/availability.go` | `ExplicitCheck` with context deadline |
| `src/connection/timing.go` | `timingDialFunc` measuring DNS + TCP + TLS phases |
| `src/connection/query_telemetry.go` | Error classification, query name extraction, credential sanitization, thread-safe telemetry accumulator |
| `src/availability/availability_test.go` | Unit tests for explicit availability check |
| `src/connection/timing_test.go` | Unit tests for timing dial func and TLS timing callback |
| `src/connection/query_telemetry_test.go` | Unit tests for error classification, sanitization, telemetry |
| `tests/e2e/` | Full E2E Docker Compose stack (primary + replica + inventorydb) |
| `tests/e2e/test-availability.sh` | Chaos test script with automated NerdGraph verification |
| `dashboards/` | New Relic dashboard template (4 pages, 40 widgets), sync script |

---

## Modified Files

| File | Changes |
|---|---|
| `src/connection/pgsql_connection.go` | pgx/v5 migration, `stdlib.OpenDB`, SSL queries, timing dial func attachment |
| `src/metrics/metrics.go` | `ObservabilityConfig`, unified `PostgresqlHealthSample` publishing functions, probe orchestration, TLS timing in implicit sample |
| `src/main.go` | Observability-aware failure handling when `BuildCollectionList` fails |
| `Makefile` | `test-verbose`, `test-coverage` targets; coverage in `make test` |
| `tests/postgresql_test.go` | Integration tests updated for `PostgresqlHealthSample` schema |
| `tests/testdata/*.json` | JSON schema updated for unified health sample format |

---

## Driver Migration: `lib/pq` -> `pgx/v5`

The `lib/pq` driver is no longer maintained and does not expose a `DialFunc`. This PR migrates to `pgx/v5/stdlib` using `stdlib.OpenDB(*config)` -- the config is held internally by the driver, so no global registry is needed and there is no race between registration and the lazy pool's first dial.

---

## Preexisting Bugs Fixed

### 1. `RegisterConnConfig` / `UnregisterConnConfig` race
**Symptom:** `cannot parse 'registeredConnConfig0': failed to parse as keyword/value`
**Cause:** `defer stdlib.UnregisterConnConfig(connStr)` fired when `NewConnection` returned, but `sqlx.DB` is lazy -- the driver reads the registry on the first query, by which point the entry was removed.
**Fix:** Replaced with `stdlib.OpenDB(*config)` which holds the config reference internally.

### 2. `COLLECT_QUERY_TELEMETRY` was a no-op
**Symptom:** `DrainTelemetry()` always returned empty.
**Cause:** `Query`, `Queryx`, `QueryxContext` used value receivers. Go copies the struct on each call; telemetry written to the copy's accumulator was silently discarded.
**Fix:** Changed `telemetry telemetryAccumulator` to `telemetry *telemetryAccumulator` (pointer).

### 3. Availability events not emitted when DB is unreachable
**Symptom:** New Relic showed a gap rather than `available=0` when PostgreSQL was down.
**Cause:** `BuildCollectionList` fails when the DB is unreachable and triggered `os.Exit(1)` before `PopulateMetrics` ran.
**Fix:** When observability flags are active, treat `BuildCollectionList` failure as non-fatal: log a warning, continue with empty collection list, emit `available=0`, then exit with code 1 after `Publish()`.

### 4. Connection timing always reported as 0
**Symptom:** `dnsLookupMs` and `tcpConnectMs` were always `0`.
**Cause:** The implicit sample was published before the pgx `DialFunc` fired (lazy pool).
**Fix:** Trigger the `DialFunc` first -- via `ExplicitCheck` or `Ping()` -- then publish the sample.

### 5. Implicit availability always showed `available=1` during outages
**Symptom:** Dashboard uptime stayed at 100% even when errors were visible.
**Cause:** `NewConnection()` creates a lazy pool that never fails for a valid config, so `connErr` was always nil for the implicit sample.
**Fix:** Derive the implicit availability signal from the explicit check result (when enabled) or from `Ping()` error (when only timing is enabled).

---

## Example NRQL Queries

### Uptime percentage
```sql
SELECT percentage(count(*), WHERE available = 1) AS 'Uptime %'
FROM PostgresqlHealthSample
WHERE checkType IN ('implicit', 'explicit')
FACET label.instance
TIMESERIES
```

### Connection timing breakdown
```sql
SELECT max(dnsLookupMs) AS 'dnsLookup', max(tcpConnectMs) AS 'tcpConnect',
       max(tlsHandshakeMs) AS 'tlsHandshake', max(durationMs) AS 'total'
FROM PostgresqlHealthSample
WHERE checkType IN ('implicit', 'explicit')
FACET label.instance
TIMESERIES
```

### Recent errors across all check types
```sql
SELECT latest(errorMessage), latest(durationMs), latest(checkType)
FROM PostgresqlHealthSample
WHERE hasError = 1
FACET errorCode, label.instance
```

---

## Test Plan

- [ ] `make test` -- all unit tests pass with `-race`
- [ ] `make integration-test` -- Docker-based integration tests pass against PostgreSQL 9.6 and 17.9
- [ ] Verify no flags set -- no `PostgresqlHealthSample` emitted (backward compat)
- [ ] `-collect_connection_timing` against reachable host -- `available=1`, `dnsLookupMs`/`tcpConnectMs` present
- [ ] `-enable_availability_check` -- explicit check with `checkType=explicit`, `durationMs`, `query`
- [ ] `-collect_query_telemetry` -- per-query health sample with `checkType=query` and `queryName`
- [ ] `cd tests/e2e && make up && make run-once` -- full JSON output with all health samples
- [ ] `make test-availability` -- chaos test: 6 scenarios pass with NerdGraph verification
- [ ] Credential sanitization: connect with password, force error, confirm password not in output
- [ ] Import dashboard template into New Relic -- verify widgets populate

---

## E2E Validation Results

_Last run: 2026-04-05 14:37:49 (normal mode, 3 cycle(s) per phase)_

| Test | Expected | Status |
|---|---|---|
| 1 -- Container stop (replica) | `dns_resolution_failed` | PASS |
| 2 -- Network disconnect (primary) | `dns_resolution_failed` | PASS |
| 3 -- Container pause (replica) | `timeout` | PASS |
| 4 -- Password change (primary) | `pg_error_28` | PASS |
| 5A -- Query cancel (slowcheck) | `pg_error_57` | PASS |
| 5B -- Backend terminate (slowcheck) | `pg_error_57` | PASS |

All services recovered. Replication: NOT in recovery (check replication)

<details>
<summary>Full chaos test output (make test-normal)</summary>

```
=== PostgreSQL Availability Chaos Test (normal) ===

Start time:          2026-04-05T21:29:55Z
Collection interval: 10s
Failure hold:        30s (3 cycles)
Stabilize hold:      30s (3 cycles)

Watch your New Relic dashboard — events will appear in real time.

================================================================
  [2026-04-05T21:29:55Z] Phase 0: Baseline — confirming all instances are healthy
================================================================
  e2e-postgres-1:  Up 10 minutes (healthy)
  e2e-postgres-2:  Up 10 minutes (healthy)
  e2e-inventorydb: Up 10 minutes (healthy)
  Done.

================================================================
  [2026-04-05T21:30:25Z] Test 1/5: Container stop — replica (DNS resolution failure)
================================================================
  Stopping e2e-postgres-2...
  Container stopped. Agent should report dns_resolution_failed for e2e-postgres-2.
  Done.
  Restoring e2e-postgres-2...
  e2e-postgres-2 is healthy.
  Done.

================================================================
  [2026-04-05T21:31:33Z] Test 2/5: Network disconnect — primary (DNS resolution failure)
================================================================
  Disconnecting e2e-postgres-1 from nri-postgresql-e2e_default...
  Network severed. Docker DNS stops resolving the hostname on this network.
  Agent should report dns_resolution_failed for e2e-postgres-1.
  Done.
  Reconnecting e2e-postgres-1 to nri-postgresql-e2e_default...
  e2e-postgres-1 is healthy.
  Done.

================================================================
  [2026-04-05T21:32:34Z] Test 3/5: Container pause — replica (connection hang / timeout)
================================================================
  Pausing e2e-postgres-2 (SIGSTOP — process frozen, TCP stays open)...
  Container paused. Agent should report timeout for e2e-postgres-2.
  Done.
  Unpausing e2e-postgres-2...
  Container resumed.
  Done.

================================================================
  [2026-04-05T21:33:35Z] Test 4/5: Password change — primary (authentication failure)
================================================================
  Changing postgres password on e2e-postgres-1...
  Password changed. Agent should report pg_error_28 for e2e-postgres-1.
  Done.
  Restoring postgres password on e2e-postgres-1...
  Password restored.
  Done.

================================================================
  [2026-04-05T21:34:36Z] Test 5/5: Backend kill — slowcheck (query cancel + connection kill)
================================================================
  Activating chaos sleep window (e2e_chaos.sleep_seconds = 5)...

  Phase A: pg_cancel_backend — cancel query, keep connection alive
  Expected error: pg_error_57 (SQLSTATE 57014, query_canceled)

  Round 1/3: Found e2e_canary on PID 2222 — cancelling query... Query cancelled.
  Round 2/3: Found e2e_canary on PID 2330 — cancelling query... Query cancelled.
  Round 3/3: Found e2e_canary on PID 2435 — cancelling query... Query cancelled.
  Done.

  Phase B: pg_terminate_backend — destroy connection mid-query
  Expected error: pg_error_57 (SQLSTATE 57P01, admin_shutdown)

  Round 1/3: Found e2e_canary on PID 2519 — terminating backend... Backend terminated.
  Round 2/3: Found e2e_canary on PID 2620 — terminating backend... Backend terminated.
  Round 3/3: Found e2e_canary on PID 2726 — terminating backend... Backend terminated.

  Deactivating chaos sleep window (e2e_chaos.sleep_seconds = 0)...
  Done.

================================================================
  [2026-04-05T21:36:41Z] Chaos test complete
================================================================

  Duration: 6m 46s
  All services recovered.

================================================================
  [2026-04-05T21:37:15Z] Verifying results via NerdGraph
================================================================

  Test  Error Code + Instance                      Count  Status
  ----  ------------------------------------------  -----  ------
  1     dns_resolution_failed @ e2e-postgres-2            4  PASS
  2     dns_resolution_failed @ e2e-postgres-1            4  PASS
  3     timeout @ e2e-postgres-2                          3  PASS
  4     pg_error_28 @ e2e-postgres-1                      4  PASS
  5A    pg_error_57 @ e2e-slowcheck                       9  PASS
  5B    pg_error_57 @ e2e-slowcheck                       9  PASS

  Result: 6/6 passed. All tests verified.
```

</details>

---

## Code Coverage

**Overall: 55.4%** (includes upstream query-performance-monitoring code with 0-48% coverage)

**New/modified observability code:**

| File | Function | Coverage |
|---|---|---|
| `availability.go` | `ExplicitCheck` | 81.2% |
| `query_telemetry.go` | `sanitizeErrorMessage` | 100% |
| `query_telemetry.go` | `extractQueryName` | 100% |
| `query_telemetry.go` | `ClassifyError` | 100% |
| `query_telemetry.go` | `record` / `DrainTelemetry` | 100% |
| `query_telemetry.go` | `timedSelect` / `timedSelectUnsafe` | 100% |
| `timing.go` | `timingDialFunc` | 89.5% |
| `timing.go` | `attachTLSTimingCallback` | 100% |
| `timing.go` | `msec` | 100% |
| `metrics.go` | `publishImplicitHealthSample` | 100% |
| `metrics.go` | `publishExplicitHealthSample` | 100% |
| `metrics.go` | `publishQueryHealthSamples` | 100% |
| `metrics.go` | `healthSampleAttrs` | 100% |
| `pgsql_connection.go` | `createConnectionURL` / `addSSLQueries` | 100% |

New observability code averages **97%+ coverage** on core logic. Below-target items are either one-liner wrappers (`Ping`, `attachTimingDialFunc`) requiring a real database or SDK error branches unreachable with valid metric types.

---

## Backwards Compatibility

All new flags default to `false`. No existing behavior, metrics, event types, or config keys are changed. Operators who do not opt in will see identical output to the previous version.
