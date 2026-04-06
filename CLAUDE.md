# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build binary to bin/nri-postgresql
make compile

# Run unit tests (with race detection + coverage summary)
make test

# Run unit tests with verbose output
make test-verbose

# Detailed coverage report (per-function + HTML)
make test-coverage

# Run a single test
go test -race ./src/connection/... -run TestName -count=1

# Run integration tests (requires Docker)
make integration-test

# Build + test (default)
make
```

## Architecture

**nri-postgresql** is a New Relic Infrastructure integration that collects PostgreSQL performance metrics, inventory, and database health data. It queries PostgreSQL system catalogs and statistics views and publishes them to New Relic.

### Main Flow

`main.go` (entry point) -> parse args -> validate -> build collection list -> observability probes -> collect metrics -> publish to New Relic

1. **`src/args/`** -- CLI argument definitions (hostname, port, credentials, SSL, feature flags)
2. **`src/connection/`** -- `PGSQLConnection` struct wrapping `sqlx.DB` with pgx/v5 driver; `connectionInfo` implements the `Info` interface for creating connections with optional timing/telemetry instrumentation
3. **`src/collection/`** -- `BuildCollectionList()` queries `pg_database` and `information_schema` to discover databases, schemas, and tables to monitor
4. **`src/metrics/`** -- `PopulateMetrics()` orchestrates metric collection; publishes `PostgresqlInstanceSample`, `PostgresqlDatabaseSample`, `PostgresqlTableSample`, `PostgresqlIndexSample`, `PgBouncerSample`, and observability samples
5. **`src/inventory/`** -- Collects PostgreSQL configuration as inventory data
6. **`src/availability/`** -- Explicit availability check with context-bounded canary query
7. **`src/query-performance-monitoring/`** -- Optional module (flag: `EnableQueryMonitoring`) for slow queries, execution plans, wait events

### Observability Layer (availability monitoring)

Opt-in features that emit `PostgresqlHealthSample` events with a `checkType` attribute differentiating signal types. All disabled by default for backward compatibility.

- **`src/connection/timing.go`** -- `timingDialFunc` closure injected into `pgconn.Config.DialFunc` to measure DNS lookup and TCP connect time separately. `attachTLSTimingCallback` chains a `tls.Config.VerifyConnection` callback to measure TLS handshake time when SSL is enabled. Both fire on the first real connection (lazy pool via `stdlib.OpenDB`).
- **`src/availability/availability.go`** -- `ExplicitCheck()` runs a user-configured SQL query (default: `SELECT 1`) with a context deadline and returns a `CheckResult` with availability, duration, and classified error. Prefers `ctx.Err()` over opaque driver errors for timeout classification.
- **`src/connection/query_telemetry.go`** -- Per-query telemetry accumulator (thread-safe). `ClassifyError()` maps errors to structured codes (timeout, pg_error_XX, connection_refused, dns_resolution_failed, etc.). `sanitizeErrorMessage()` redacts PostgreSQL connection URL credentials from error strings before publishing.
- **`src/metrics/metrics.go`** -- `ObservabilityConfig` struct and publishing functions: `publishImplicitHealthSample` (checkType=implicit), `publishExplicitHealthSample` (checkType=explicit), `publishQueryHealthSamples` (checkType=query).

**Unified event model (`PostgresqlHealthSample`):**
- `checkType=implicit` -- Ping-based availability + DNS/TCP/TLS timing (`available`, `hasError`, `dnsLookupMs`, `tcpConnectMs`, `tlsHandshakeMs`)
- `checkType=explicit` -- Canary query result (`available`, `hasError`, `durationMs`, `query`, `errorCode`)
- `checkType=query` -- Per internal monitoring query (`queryName`, `database`, `durationMs`, `hasError`, `errorCode`)

**Observability flags:**

| Flag | Default | Purpose |
|---|---|---|
| `CollectConnectionTiming` | `false` | Ping + DNS/TCP timing + implicit `db.available` |
| `EnableAvailabilityCheck` | `false` | Run explicit canary query each cycle |
| `AvailabilityCheckQuery` | `"SELECT 1"` | SQL used for the explicit check |
| `AvailabilityCheckTimeoutMs` | `10000` | Timeout for the explicit check |
| `CollectQueryTelemetry` | `false` | Per-query telemetry for internal monitoring queries |

### Sample Type Attributes (for NRQL / dashboards)

Not all attributes exist on all sample types. Key differences for building NRQL queries:

| Sample Type | `displayName` | `database` | `schema` | `table` | Notes |
|---|---|---|---|---|---|
| `PostgresqlInstanceSample` | `host:port` | -- | -- | -- | Instance-level bgwriter/IO metrics |
| `PostgresqlHealthSample` | `host:port` | -- | -- | -- | Availability, timing, error classification |
| `PostgresqlDatabaseSample` | DB name | -- | -- | -- | **No `database` attr** — use `displayName` to FACET by DB |
| `PostgresqlTableSample` | table name | yes | yes | -- | Has `database` and `schema` attrs |
| `PostgresqlIndexSample` | index name | yes | yes | yes | Has `database`, `schema`, `table` attrs |
| `PgBouncerSample` | pool name | -- | -- | -- | PgBouncer pool stats |

All sample types have `displayName`, `entityName`, `hostname` (agent host), and any `label.*` attributes from the integration config. With `REMOTE_MONITORING: "true"`, entities are created per target; without it, all data goes to the agent's local entity.

### Key Patterns

- **`Info` interface + `connectionInfo`** -- injectable connection factory; unit tests use `CreateMockSQL()` which returns a `*PGSQLConnection` backed by go-sqlmock
- **Pointer-based telemetry accumulator** -- `telemetry *telemetryAccumulator` is a pointer so value-receiver methods on `PGSQLConnection` share the same storage
- **pgx/v5 via stdlib** -- uses `stdlib.OpenDB(*config)` to avoid global registry races that occur with `RegisterConnConfig`/`UnregisterConnConfig` + lazy pool
- **Credential safety** -- `sanitizeErrorMessage()` in `query_telemetry.go` redacts passwords from PostgreSQL connection URLs (`postgres://user:pass@host`) before they reach New Relic. All error messages flow through `ClassifyError()` which applies this sanitization.
- **Observability-aware failure handling** -- when `BuildCollectionList()` fails but observability flags are active, the integration falls through with an empty collection list so it can still emit `available=0` before exiting
- **Version-conditional metrics** -- metric definitions branch on PostgreSQL semver (e.g., bgwriter columns differ across versions)
- **go-sqlmock** is used for unit testing database code; `CreateMockSQL(t)` helper in `pgsql_connection_mock.go`

### Testing

- Unit tests use `go-sqlmock` and `testify/assert`. Always run with `-race` flag.
- Integration tests in `tests/` use Docker Compose with PostgreSQL 9.6 and latest (17.9). The binary runs inside the `nri-postgresql` container. Separate compose files exist for performance monitoring and PgBouncer tests.
- E2E validation harness in `tests/e2e/` runs the full New Relic Infrastructure Agent stack against a local PostgreSQL instance with all observability flags enabled. Includes `test-availability.sh` chaos test for failure scenario validation.

### PR.md Handling

`PR.md` is a local working draft -- never commit it. The chaos test script (`tests/e2e/test-availability.sh`) auto-updates the `## E2E Validation Results` section in PR.md after each run using python3 `re.sub()` for multiline replacement.

**PR.md conventions:**
- The `## E2E Validation Results` section is machine-managed by the test script. Do not manually edit it -- it gets overwritten on the next test run.
- Dashboard screenshots are uploaded directly to the GitHub PR (not committed as PNGs). Reference them with `<img>` tags using GitHub asset URLs, not relative paths.
- Include a `### Change Breakdown` table showing lines changed by category (Go source, Go tests, shell, dashboard JSON, config/docs, etc.) to help reviewers gauge the weight of the change.
- Include a `### References` section with links to PostgreSQL docs (error codes, SSL, pg_stat_activity), NR integration docs, NRQL reference, and the nri-mysql sister PR.
- `</details>` tags must have a blank line after them before the next markdown element (e.g. `---`), otherwise the markdown doesn't render correctly.
- **After every commit**, refresh the PR.md `### Change Breakdown` table by re-running the line count breakdown (`git diff --numstat` from the merge base). Also update coverage numbers if Go source changed. This keeps the PR.md accurate as the branch evolves.

### GitHub API Calls

Always use `gh api` instead of `curl` for GitHub API calls. `gh` handles auth automatically and avoids curl/python parsing boilerplate.

### Shell Commands

Write multi-step shell commands as single-line (use `;` or `&&` instead of newlines) to avoid the multi-line safety prompt.

### Reference

- nri-mysql availability monitoring (sister implementation): https://github.com/bvinisky/nri-mysql
