#!/bin/bash
# Initialize the replica by cloning from the primary using pg_basebackup,
# then configure streaming replication via standby.signal + primary_conninfo.
#
# This script runs inside the replica container's entrypoint init phase.
# If the data directory already contains data, it's a restart — skip cloning.

set -e

PRIMARY_HOST="e2e-postgres-1"
PRIMARY_PORT="5432"
REPL_USER="repl"
REPL_PASSWORD="repl_password"

# If the data directory already has a PG_VERSION file, we've already initialized.
if [ -f "$PGDATA/PG_VERSION" ]; then
    echo "Data directory already initialized — skipping pg_basebackup."
    exit 0
fi

echo "Waiting for primary ($PRIMARY_HOST:$PRIMARY_PORT) to be ready..."
for i in $(seq 1 30); do
    if PGPASSWORD="$REPL_PASSWORD" pg_isready -h "$PRIMARY_HOST" -p "$PRIMARY_PORT" -U "$REPL_USER" 2>/dev/null; then
        echo "Primary is ready."
        break
    fi
    echo "  Attempt $i/30 — primary not ready, sleeping 2s..."
    sleep 2
done

echo "Cloning primary with pg_basebackup..."
PGPASSWORD="$REPL_PASSWORD" pg_basebackup \
    -h "$PRIMARY_HOST" \
    -p "$PRIMARY_PORT" \
    -U "$REPL_USER" \
    -D "$PGDATA" \
    -Fp -Xs -P -R

# pg_basebackup with -R creates standby.signal and sets primary_conninfo
# in postgresql.auto.conf automatically. Verify:
if [ -f "$PGDATA/standby.signal" ]; then
    echo "standby.signal created — replica will start in standby mode."
else
    echo "Creating standby.signal manually..."
    touch "$PGDATA/standby.signal"
    cat >> "$PGDATA/postgresql.auto.conf" <<EOF
primary_conninfo = 'host=$PRIMARY_HOST port=$PRIMARY_PORT user=$REPL_USER password=$REPL_PASSWORD'
EOF
fi

echo "Replica initialization complete."
