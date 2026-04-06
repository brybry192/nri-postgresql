# TODO

## E2E Testing

- [ ] Enable SSL for e2e tests — self-signed CA + server cert with SANs, regenerated on `make up`, leave at least one instance without TLS for response time comparison
- [ ] Fix replication "NOT in recovery" warning — replica sometimes needs longer than 30s to resync after chaos

## Observability

- [ ] Add replication metrics — query `pg_stat_replication` (primary) and `pg_stat_wal_receiver` (replica) to collect lag, WAL positions, replay status. New `PostgresqlReplicationSample` event type. Add a Replication page to the dashboard.

## Dashboard

- [ ] Add PgBouncer page to dashboard (pool utilization, wait time, active/waiting clients) — requires `COLLECT_PGBOUNCER=true` and a PgBouncer instance in the e2e stack
