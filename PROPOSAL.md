# Proposal: PostgreSQL Connection Observability & Query Telemetry

## Overview

Two related improvements to `nri-postgresql`:

1. **Connection observability and query telemetry** — APM-style timing, structured error
   classification, and availability signals on every monitoring query the integration runs.
2. **pgx driver migration** — replacing `github.com/lib/pq` with `github.com/jackc/pgx/v5` to
   unlock proper connection lifecycle hooks that make #1 possible without throwaway probe connections.

---

## Part 1 — Connection Observability & Query Telemetry

### Motivation

When a monitoring check fails or is slow, the integration currently logs a line and exits.
There is no structured way to answer:

- Was it a DNS failure, a TCP refusal, a TLS error, or a bad password?
- Was authentication slow, or was the query itself slow?
- Did a specific monitoring query time out vs. the server being unreachable?
- Is the monitoring agent itself broken, or is the DB genuinely down?

This proposal adds structured, per-query APM signals so these questions can be answered with NRQL.

---

### New Configuration Options

| Option | Type | Default | Description |
|---|---|---|---|
| `COLLECT_CONNECTION_TIMING` | bool | `false` | Measure DNS, TCP, and total authenticated-connect time each cycle |
| `ENABLE_AVAILABILITY_CHECK` | bool | `false` | Run an explicit canary query alongside normal metric collection |
| `AVAILABILITY_CHECK_QUERY` | string | `SELECT 1` | SQL for the explicit availability check |
| `COLLECT_QUERY_TELEMETRY` | bool | `false` | Emit one `PostgresqlQueryTelemetrySample` per internal query per run |

All default off — no change in behavior unless explicitly enabled.

---

### New Metric Sample: `PostgresqlConnectionSample`

Published once per integration run, on the `pg-instance` entity.

**Always present (implicit availability from the connection attempt):**

| Field | Type | Description |
|---|---|---|
| `db.available` | gauge (0/1) | 1 if connection succeeded |
| `db.connection.errorCode` | attribute | Structured error class if connection failed |
| `db.connection.errorMessage` | attribute | Full error text if connection failed |

**Present when `COLLECT_CONNECTION_TIMING=true`:**

| Field | Type | Description |
|---|---|---|
| `db.connection.dnsLookupMs` | gauge | Time to resolve hostname via DNS |
| `db.connection.tcpConnectMs` | gauge | Time to establish TCP after DNS |
| `db.connection.tlsAndAuthMs` | gauge | TLS handshake + authentication combined (total − DNS − TCP) |
| `db.connection.totalConnectMs` | gauge | Wall time from dial start to authenticated ready state |

**Present when `ENABLE_AVAILABILITY_CHECK=true`:**

| Field | Type | Description |
|---|---|---|
| `db.availabilityCheck.available` | gauge (0/1) | 1 if canary query returned expected result |
| `db.availabilityCheck.durationMs` | gauge | How long the canary query took |
| `db.availabilityCheck.errorCode` | attribute | Structured error class if query failed |
| `db.availabilityCheck.errorMessage` | attribute | Full error text if query failed |
| `db.availabilityCheck.query` | attribute | The query that was run |

---

### New Metric Sample: `PostgresqlQueryTelemetrySample`

Published when `COLLECT_QUERY_TELEMETRY=true`. One event per internal query per run.

| Field | Type | Description |
|---|---|---|
| `queryName` | attribute | Identifier from the embedded SQL comment (e.g. `BGWRITER_STATS`) |
| `database` | attribute | Database the query ran against |
| `durationMs` | gauge | Wall-clock query execution time in milliseconds |
| `hasError` | gauge (0/1) | 1 if the query returned an error |
| `errorCode` | attribute | Structured error classification |
| `errorMessage` | attribute | Full error text |

**Example NRQL:**
```sql
-- Average monitoring query time by query type
FROM PostgresqlQueryTelemetrySample
SELECT average(durationMs) FACET queryName SINCE 1 hour ago

-- All failed monitoring queries
FROM PostgresqlQueryTelemetrySample
WHERE hasError = 1 SELECT queryName, errorCode, errorMessage SINCE 1 hour ago

-- Is the availability check passing?
FROM PostgresqlConnectionSample
SELECT latest(db.available), latest(db.availabilityCheck.available) FACET entity.name
```

---

### Error Classification

Used in both `PostgresqlConnectionSample` and `PostgresqlQueryTelemetrySample`:

| `errorCode` | Condition |
|---|---|
| `pg_error_<class>` | PostgreSQL server SQLSTATE error (first 2 chars, e.g. `pg_error_42` = syntax) |
| `connection_refused` | TCP connection actively refused |
| `timeout` | Client or driver timeout expired |
| `server_closed_connection` | Server closed connection unexpectedly (EOF / RST) |
| `connection_reset` | TCP RST received |
| `dns_resolution_failed` | Hostname could not be resolved |
| `io_timeout` | I/O deadline exceeded on an established connection |
| `tls_error` | TLS handshake failure |
| `unknown_error` | Does not match a known pattern |

---

### Implicit vs Explicit Availability

**Implicit** (always on, zero config): Every collection cycle opens a connection. Success/failure
is recorded as `db.available` in `PostgresqlConnectionSample`. No extra queries or round-trips.

**Explicit** (opt-in, `ENABLE_AVAILABILITY_CHECK=true`): After connecting, runs the configured
canary query and records timing + result in `db.availabilityCheck.*`. Proves the server can
execute queries, not just accept connections.

---

## Part 2 — pgx Driver Migration

### Why pgx

`github.com/lib/pq` is in maintenance mode. `github.com/jackc/pgx/v5` is the actively maintained
successor. Beyond maintenance status, pgx exposes hooks that `lib/pq` does not:

- **`pgconn.Config.DialFunc`** — fires on every new TCP connection. We measure DNS + TCP here on
  the real connection, not a throwaway probe.
- **`pgconn.PgError`** — structured server error with SQLSTATE code, detail, hint, severity.
- Future: `pgx.QueryTracer` for server-side query timing from inside the driver.

### What IS drop-in

| Area | Notes |
|---|---|
| Connection URL format | Same `postgres://` DSN; same `sslmode`, `sslcert`, `sslkey`, `sslrootcert`, `connect_timeout` |
| sqlx | Wraps `database/sql`; driver swap is invisible |
| `go-sqlmock.v1` tests | Mock operates at `database/sql` level, independent of driver |
| FIPS compliance | pgx uses Go stdlib `crypto/tls`, same as lib/pq |
| PgBouncer | Both use simple query protocol via `database/sql` |
| NULL / type scanning | Same behavior for all types used in struct scanning |

### What is NOT drop-in

| Area | Change | Effort |
|---|---|---|
| Driver registration | `_ "github.com/lib/pq"` → `_ "github.com/jackc/pgx/v5/stdlib"`; `sqlx.Open("postgres",…)` → `sqlx.Open("pgx",…)` | Trivial |
| Error type | `*pq.Error` → `*pgconn.PgError` — different package, `.Code` is `string` not `pq.ErrorCode` | Small — only new `classifyError()` code |
| `NewConnection` internals | `pgx.ParseConfig` + `stdlib.RegisterConnConfig` to attach `DialFunc`; `stdlib.UnregisterConnConfig` after open to avoid global map leak | Small |
| `go.mod` | Remove `lib/pq`, add `pgx/v5` + transitive deps | Trivial |

### Error type change in detail

```go
// lib/pq (old)
import "github.com/lib/pq"
if pgErr, ok := err.(*pq.Error); ok {
    code := string(pgErr.Code.Class()) // pq.ErrorCode with .Class() method
    msg  := pgErr.Message
}

// pgx/v5 (new)
import "github.com/jackc/pgx/v5/pgconn"
var pgErr *pgconn.PgError
if errors.As(err, &pgErr) {
    code := pgErr.Code[:2] // plain string, first 2 chars = SQLSTATE class
    msg  := pgErr.Message  // same field name
}
```

No existing code uses `*pq.Error` assertions — this only appears in the new `classifyError()`
function being added as part of this work.

### RegisterConnConfig usage

```go
config, err := pgx.ParseConfig(createConnectionURL(ci, database))
// attach DialFunc, tracers here
connStr := stdlib.RegisterConnConfig(config)
defer stdlib.UnregisterConnConfig(connStr) // prevent global map leak
db, err := sqlx.Open("pgx", connStr)
```

`createConnectionURL()` is unchanged — same URL format, same parameters.

### Risk Summary

| Risk | Severity | Notes |
|---|---|---|
| Error type assertion change | Low | Only in new code we're adding |
| `RegisterConnConfig` map leak | Low | One `defer UnregisterConnConfig` call; well-documented |
| Wire protocol difference | Very low | Both use simple query protocol via `database/sql` |
| PgBouncer statement mode | Very low | pgx/stdlib compatible |
| RDS IAM token refresh | Low | pgx `BeforeConnect` hook is better suited than lib/pq |
| FIPS | None | Both use standard `crypto/tls` |
| Test changes | None | `go-sqlmock.v1` unaffected |

### TLS/auth timing limitation

`DialFunc` fires before TLS. pgx handles TLS internally after the TCP conn is returned from
`DialFunc`. This means:

- **DNS time**: measured in `DialFunc` ✓
- **TCP time**: measured in `DialFunc` ✓
- **TLS + auth combined**: `totalConnectMs - dnsLookupMs - tcpConnectMs`
- **TLS separately**: requires wrapping `net.Conn` returned from `DialFunc` — deferred

### Native pgx (without stdlib) — future consideration

Using `pgxpool.Pool` directly (removing sqlx) would additionally give:
- Pool acquisition timing
- Per-query server-side timing via `pgx.QueryTracer` (no client overhead)
- Cleaner row scanning without sqlx

Requires rewriting all query scanning code. Recommended as a future phase.

---

## Implementation Plan

| Phase | Scope | Files |
|---|---|---|
| 1. pgx drop-in | Swap driver, update `NewConnection` | `go.mod`, `connection/pgsql_connection.go` |
| 2. New config flags | Four new args | `args/argument_list.go` |
| 3. Connection timing | `ConnectionTiming`, `DialFunc`, Ping-based total time | `connection/timing.go`, `connection/pgsql_connection.go` |
| 4. Query telemetry | `QueryTelemetry`, `classifyError`, accumulation in `Query()` | `connection/query_telemetry.go`, `connection/pgsql_connection.go` |
| 5. Availability check | Canary query execution | `availability/availability.go` |
| 6. Publishing | New sample types in metrics pipeline | `metrics/metrics.go`, `main.go` |

---

## New Files

- `src/connection/timing.go` — `ConnectionTiming` struct, `DialFunc` wrapper
- `src/connection/query_telemetry.go` — `QueryTelemetry`, `classifyError()`, `DrainTelemetry()`
- `src/availability/availability.go` — explicit availability check
- `PROPOSAL.md` — this document

