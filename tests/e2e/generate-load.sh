#!/bin/bash
# generate-load.sh — Generate read/write workload against the e2e PostgreSQL stack.
#
# Runs a mix of INSERTs, UPDATEs, DELETEs, and SELECTs (including index scans
# and sequential scans) to produce interesting metrics on the dashboard:
# transaction throughput, row activity, cache hits, index usage, dead rows, etc.
#
# Usage:
#   ./generate-load.sh              # 60 seconds (default)
#   DURATION=120 ./generate-load.sh # 120 seconds
#   WORKERS=4 ./generate-load.sh    # 4 parallel workers

set -uo pipefail

DURATION=${DURATION:-60}
WORKERS=${WORKERS:-2}
SERVICE=${SERVICE:-e2e-postgres-1}
DB=${DB:-demo}
PG_USER=${PG_USER:-postgres}

echo "=== PostgreSQL Load Generator ==="
echo "  Target:   $SERVICE ($DB)"
echo "  Duration: ${DURATION}s"
echo "  Workers:  $WORKERS"
echo ""

# Single worker: runs a mixed workload loop until the deadline.
run_worker() {
    local id=$1 end_time=$2 ops=0

    while [ "$(date +%s)" -lt "$end_time" ]; do
        # INSERT — new orders (triggers index updates, increases table size)
        docker compose exec -T "$SERVICE" psql -U "$PG_USER" -d "$DB" -c \
            "INSERT INTO orders (customer_id, status, total_cents)
             SELECT (random() * 499 + 1)::int,
                    (ARRAY['pending','paid','shipped','cancelled'])[floor(random()*4+1)],
                    (random() * 50000 + 100)::int
             FROM generate_series(1, 50);" >/dev/null 2>&1 || true
        ops=$((ops + 1))

        # UPDATE — change order status (creates dead rows for vacuum)
        docker compose exec -T "$SERVICE" psql -U "$PG_USER" -d "$DB" -c \
            "UPDATE orders SET status = 'shipped', total_cents = total_cents + 1
             WHERE id IN (SELECT id FROM orders WHERE status = 'pending' ORDER BY random() LIMIT 20);" >/dev/null 2>&1 || true
        ops=$((ops + 1))

        # DELETE — remove old orders (creates dead rows, tests bloat)
        docker compose exec -T "$SERVICE" psql -U "$PG_USER" -d "$DB" -c \
            "DELETE FROM orders WHERE id IN (
             SELECT id FROM orders ORDER BY random() LIMIT 10);" >/dev/null 2>&1 || true
        ops=$((ops + 1))

        # SELECT with index scan — lookup by customer_id (exercises idx_orders_customer)
        docker compose exec -T "$SERVICE" psql -U "$PG_USER" -d "$DB" -c \
            "SELECT count(*), sum(total_cents) FROM orders
             WHERE customer_id = (random() * 499 + 1)::int;" >/dev/null 2>&1 || true
        ops=$((ops + 1))

        # SELECT with index scan — range query on created_at (exercises idx_orders_created)
        docker compose exec -T "$SERVICE" psql -U "$PG_USER" -d "$DB" -c \
            "SELECT count(*) FROM orders
             WHERE created_at > now() - interval '1 hour';" >/dev/null 2>&1 || true
        ops=$((ops + 1))

        # SELECT sequential scan — aggregate without index (triggers seq scan on products)
        docker compose exec -T "$SERVICE" psql -U "$PG_USER" -d "$DB" -c \
            "SELECT avg(price_cents), max(price_cents) FROM products;" >/dev/null 2>&1 || true
        ops=$((ops + 1))

        # JOIN query — exercises both tables and indexes
        docker compose exec -T "$SERVICE" psql -U "$PG_USER" -d "$DB" -c \
            "SELECT c.email, count(o.id), sum(o.total_cents)
             FROM customers c JOIN orders o ON o.customer_id = c.id
             WHERE c.id BETWEEN (random()*250+1)::int AND (random()*250+251)::int
             GROUP BY c.email ORDER BY sum(o.total_cents) DESC LIMIT 5;" >/dev/null 2>&1 || true
        ops=$((ops + 1))

        # Temp file generator — large sort that exceeds work_mem
        docker compose exec -T "$SERVICE" psql -U "$PG_USER" -d "$DB" -c \
            "SELECT customer_id, status, total_cents
             FROM orders ORDER BY total_cents DESC, customer_id, status
             LIMIT 1000;" >/dev/null 2>&1 || true
        ops=$((ops + 1))

        sleep 0.1
    done

    echo "  Worker $id: $ops batches completed"
}

END_TIME=$(( $(date +%s) + DURATION ))

echo "Starting $WORKERS workers..."
echo ""

PIDS=""
for i in $(seq 1 "$WORKERS"); do
    run_worker "$i" "$END_TIME" &
    PIDS="$PIDS $!"
done

# Wait for all workers, print progress every 10s
while kill -0 $PIDS 2>/dev/null; do
    REMAINING=$(( END_TIME - $(date +%s) ))
    if [ "$REMAINING" -gt 0 ]; then
        printf "\r  %ds remaining...  " "$REMAINING"
    fi
    sleep 5
done
printf "\r                        \n"

wait

echo ""
echo "Load generation complete (${DURATION}s)."
echo ""
echo "Check the dashboard — you should see activity in:"
echo "  - Database Health: transaction rate, row activity, cache hits"
echo "  - Tables & Indexes: seq vs index scans, dead rows, table sizes"
echo "  - Instance Performance: buffer writes, checkpoint activity"
