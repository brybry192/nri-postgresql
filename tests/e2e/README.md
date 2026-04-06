# nri-postgresql E2E Validation

End-to-end testing environment for the observability features added on the
`feat/availability-monitoring` branch. Runs the New Relic Infrastructure Agent
and a local PostgreSQL instance entirely in Docker — no host-level installs
required.

## What We're Validating

The branch adds four new optional flags (all `false` by default):

| Flag | What it reports | Event type |
|---|---|---|
| `COLLECT_CONNECTION_TIMING` | DNS lookup + TCP connect time per cycle | `PostgresqlHealthSample` (checkType=implicit) |
| `ENABLE_AVAILABILITY_CHECK` | Canary query result + duration + error code | `PostgresqlHealthSample` (checkType=explicit) |
| `AVAILABILITY_CHECK_TIMEOUT_MS` | Bounds the canary query so it can't block the next cycle | — |
| `COLLECT_QUERY_TELEMETRY` | Per-internal-query duration + error code | `PostgresqlHealthSample` (checkType=query) |

Health samples are only emitted when at least one observability flag is enabled.
With no flags set, the integration produces identical output to the upstream baseline.

---

## Prerequisites

- **Docker Desktop** or **Rancher Desktop** running
- **New Relic account** with a license key (see below)
- The `feat/availability-monitoring` branch checked out in the repo

---

## New Relic Account Setup

1. Go to [newrelic.com](https://newrelic.com) and sign up for a free account.
   The free tier includes 100 GB/month ingest and full NRQL access — no credit
   card required, just an email address.

2. After logging in, get your **Ingest License Key**:
   - Click your user avatar (top right) → **API Keys**
   - Find the key of type **INGEST - LICENSE** (or create one)
   - Copy the full key — it looks like `eu01xx...NRAL` or `XXXXNRAL`

3. Note your **account region**: US (default, `one.newrelic.com`) or EU
   (`one.eu.newrelic.com`). If EU, you'll need to set `NRIA_COLLECTOR_URL` in
   `.env` (see `.env.example`).

---

## Local PostgreSQL Testing

### 1. First-time setup

```bash
cd ~/nri-postgresql-e2e

# Copy the env template and fill in your license key
make setup
# → edit .env and set NR_LICENSE_KEY=<your key>
```

### 2. Start the stack

```bash
make up
```

This will:
- Build the infra agent image (compiles `nri-postgresql` from the current
  branch source — takes ~30s on first run, cached thereafter)
- Start a PostgreSQL 17 container seeded with test tables and data
- Start the New Relic Infrastructure Agent, which will run the integration
  every 30 seconds and ship events to New Relic

### 3. Verify the binary output immediately (no waiting for NR)

Before checking New Relic, run the integration once directly and inspect the
JSON output:

```bash
make run-once
```

Look for:
- `"event_type": "PostgresqlHealthSample"` — appears twice per cycle:
  once without `checkType` (implicit) and once with `"checkType": "explicit"`
- `"db.available": 1` in the implicit sample
- `"db.availabilityCheck.available": 1` in the explicit sample
- `"db.connection.dnsLookupMs"` and `"db.connection.tcpConnectMs"` (timing)
- `"event_type": "PostgresqlHealthSample"` — one entry per internal
  monitoring query (version query, bgwriter stats, database stats, etc.)

### 4. Watch agent logs

```bash
make logs
```

Look for lines like:
```
time="..." level=info msg="Integration health check ... nri-postgresql ... Healthy"
```

If you see errors, set `NRIA_VERBOSE=1` in `.env` and restart.

### 5. Query in New Relic

Allow 1–2 minutes for the first data points to appear, then open
[one.newrelic.com](https://one.newrelic.com) → **Query your data** (NRQL).

#### Implicit availability signal (always emitted)

```sql
FROM PostgresqlHealthSample
SELECT db.available, displayName, entityName
WHERE db.available IS NOT NULL
  AND checkType IS NULL
SINCE 10 minutes ago
LIMIT 20
```

**Expected:** `db.available = 1` for the local postgres instance.

#### Explicit availability check

```sql
FROM PostgresqlHealthSample
SELECT
  db.availabilityCheck.available,
  db.availabilityCheck.durationMs,
  db.availabilityCheck.query,
  db.availabilityCheck.errorCode,
  displayName
WHERE checkType = 'explicit'
SINCE 10 minutes ago
LIMIT 20
```

**Expected:** `available = 1`, `durationMs` in single-digit milliseconds,
`query = 'SELECT 1'`.

#### Connection timing

```sql
FROM PostgresqlHealthSample
SELECT
  db.connection.dnsLookupMs,
  db.connection.tcpConnectMs,
  displayName
WHERE db.connection.dnsLookupMs IS NOT NULL
SINCE 10 minutes ago
LIMIT 20
```

**Expected:** both fields present with small positive values (sub-millisecond
for local docker is normal; the DNS and TCP happen on first connect each cycle).

#### Query telemetry

```sql
FROM PostgresqlHealthSample
SELECT
  queryName,
  database,
  durationMs,
  hasError,
  errorCode
SINCE 10 minutes ago
LIMIT 50
```

**Expected:** one row per internal monitoring query (SHOW_SERVER_VERSION,
BGWRITER_STATS, DATABASE_STATS, etc.). All should have `hasError = 0`.

To see timing distribution:

```sql
FROM PostgresqlHealthSample
SELECT average(durationMs), max(durationMs), count(*)
FACET queryName
SINCE 1 hour ago
```

#### Availability check over time (alerting use case)

```sql
FROM PostgresqlHealthSample
SELECT latest(db.availabilityCheck.available)
WHERE checkType = 'explicit'
FACET displayName
TIMESERIES 1 minute
SINCE 30 minutes ago
```

This is the chart you'd pin to a dashboard and alert on. A drop to 0 means the
canary query failed.

### 6. Tear down

```bash
make down
```

---

## Simulating Failure Scenarios

### Connection failure (availability check on dead server)

Stop just the postgres container while the agent is still running:

```bash
docker compose stop postgres
```

Wait up to 30s (one integration cycle), then query:

```sql
FROM PostgresqlHealthSample
SELECT db.availabilityCheck.available, db.availabilityCheck.errorCode
WHERE checkType = 'explicit'
SINCE 5 minutes ago
ORDER BY timestamp DESC
LIMIT 5
```

**Expected:** `available = 0` with an `errorCode` like `connection_refused`.
The implicit sample should still show `db.available = 1` (pool creation is lazy)
while the explicit check reflects the real failure.

Restart postgres when done:

```bash
docker compose start postgres
```

### Timeout scenario (canary query exceeds timeout)

Run the integration once manually with a query that sleeps and a tight timeout:

```bash
docker compose exec newrelic-infra \
  /var/db/newrelic-infra/newrelic-integrations/bin/nri-postgresql \
  -username postgres -password e2e_test_password \
  -hostname postgres -port 5432 -database demo \
  -enable_availability_check=true \
  -availability_check_query="SELECT pg_sleep(10)" \
  -availability_check_timeout_ms=500 \
  | python3 -m json.tool
```

**Expected in output:** `db.availabilityCheck.errorCode = "timeout"` and the
binary should return in ~500ms rather than blocking for 10s.

---

## Aurora / Remote PostgreSQL Testing

### Prerequisites

- Aurora PostgreSQL cluster endpoint and credentials
- Network access from your Mac to the Aurora endpoint (VPN if private subnet)

### Setup

Add Aurora credentials to `.env`:

```bash
AURORA_HOST=your-cluster.cluster-xxxx.us-east-1.rds.amazonaws.com
AURORA_PORT=5432
AURORA_USER=your_db_user
AURORA_PASSWORD=your_db_password
AURORA_DB=postgres
AURORA_SSL=true
```

### Start

```bash
make up-aurora
```

This starts only the infra agent (no local postgres). The agent container uses
`config/integrations.d/postgresql-aurora.yml`, which reads credentials from the
env vars above.

> **VPN note:** If your Aurora instance is in a private VPC accessible only via
> VPN, and your VPN routes all traffic through the host, uncomment
> `network_mode: host` in `docker-compose.aurora.yml`.

### Verify

Same NRQL queries as above. The `displayName` will be the Aurora endpoint
rather than `postgres:5432`, so filter by that:

```sql
FROM PostgresqlHealthSample
SELECT *
WHERE displayName LIKE '%rds.amazonaws.com%'
SINCE 10 minutes ago
LIMIT 5
```

### Tear down

```bash
make down-aurora
```

---

## After Source Changes

If you change the `feat/availability-monitoring` branch source and want to
rebuild the agent image:

```bash
make rebuild
```

This forces a clean image rebuild and restarts the stack.

---

## Troubleshooting

**No data in New Relic after 5 minutes**
- Check `make logs` for errors
- Verify `NR_LICENSE_KEY` in `.env` is correct and the right region
- Try `NRIA_VERBOSE=1` in `.env`, then `make rebuild` for debug logging
- Run `make run-once` to check if the binary itself works

**`make run-once` shows errors**
- Check that the postgres container is healthy: `docker compose ps`
- Credentials in `config/integrations.d/postgresql-local.yml` must match `.env`

**`connection refused` or `no such host` in telemetry**
- The integration config uses the docker service name `postgres` as the hostname
  — this only resolves inside the docker network, not from your Mac directly

**Aurora: `SSL connection required`**
- Ensure `ENABLE_SSL=true` and either `TRUST_SERVER_CERTIFICATE=true` or
  point `SSL_ROOT_CERT_LOCATION` at the RDS CA bundle

**Image build fails on Apple Silicon (arm64)**
- The Dockerfile builds for `linux/amd64` by default (matches newrelic/infrastructure)
- If you need to force the platform explicitly:
  ```bash
  docker buildx build --platform linux/amd64 -f Dockerfile.agent ..
  ```
  Or add `platform: linux/amd64` to the `newrelic-infra` service in `docker-compose.yml`

---

## File Reference

```
~/nri-postgresql-e2e/
├── .env.example                    ← template; copy to .env and fill in keys
├── .env                            ← your secrets (gitignored)
├── Makefile                        ← convenience targets (make help)
├── Dockerfile.agent                ← builds infra agent + custom nri-postgresql
├── docker-compose.yml              ← local postgres + infra agent
├── docker-compose.aurora.yml       ← override for Aurora/remote testing
├── config/
│   ├── newrelic-infra.yml          ← minimal agent config
│   └── integrations.d/
│       ├── postgresql-local.yml    ← all new flags enabled, targets local postgres
│       └── postgresql-aurora.yml   ← Aurora config, reads from env vars
└── init/
    └── 01-schema.sql               ← test tables + seed data for richer metrics
```
