#!/bin/bash
# test-availability.sh — Chaos test for availability monitoring.
#
# Orchestrates real failure scenarios against the running e2e stack so the
# agent detects, classifies, and reports them to New Relic. Each scenario
# holds the failure state for several collection cycles, then restores
# and stabilizes before moving on.
#
# Watch the dashboard while this runs to see the events appear in real time.
#
# Requires: the e2e stack is running (make up).
#
# Usage:
#   ./test-availability.sh              # Normal mode (~6 min): 3 cycles per phase
#   FAST=1 ./test-availability.sh       # Fast mode   (~3 min): 2 fail cycles
#   SKIP_TO=4 ./test-availability.sh    # Jump to test 4 (skip 1-3)
#   SKIP_TO=5 FAST=1 ./test-availability.sh  # Jump to test 5, fast mode
#
# After completion the script auto-updates ../../PR.md with an E2E results
# summary table and a collapsed <details> block containing the full log.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
PR_MD="$REPO_ROOT/PR.md"
LOG_FILE=$(mktemp)

# Capture all output to both console and log file.
exec > >(tee "$LOG_FILE") 2>&1

# Collection interval matches the agent's 10s interval.
CYCLE=${CYCLE:-10}

# FAST=1 drops to 2 fail / 1 stabilize cycles — enough to confirm error
# classification without waiting for chart-visible gaps between state changes.
if [ "${FAST:-0}" = "1" ]; then
    FAIL_CYCLES=2
    STABILIZE_CYCLES=1
else
    FAIL_CYCLES=${FAIL_CYCLES:-3}
    STABILIZE_CYCLES=${STABILIZE_CYCLES:-3}
fi

FAIL_DURATION=$((CYCLE * FAIL_CYCLES))
STABILIZE_DURATION=$((CYCLE * STABILIZE_CYCLES))

# ── Timing ────────────────────────────────────────────────────────────────────

ts() { date -u '+%Y-%m-%dT%H:%M:%SZ'; }
epoch() { date +%s; }

START_TIME=$(ts)
START_EPOCH=$(epoch)

# ── Helpers ──────────────────────────────────────────────────────────────────

# Resolve the Compose-generated container name and default network dynamically
# so we don't hardcode the project prefix (nri-postgresql-e2e).
resolve_container() {
    docker compose ps "$1" --format '{{.Name}}' 2>/dev/null | head -1
}
resolve_network() {
    docker compose ps "$1" --format '{{.Networks}}' 2>/dev/null | head -1
}

banner() {
    echo ""
    echo "================================================================"
    echo "  [$(ts)] $1"
    echo "================================================================"
}

hold() {
    local duration=$1 label=$2
    echo "  Holding for ${duration}s ($label)..."
    for i in $(seq "$duration" -1 1); do
        printf "\r  %3ds remaining " "$i"
        sleep 1
    done
    printf "\r  Done.                \n"
}

wait_healthy() {
    local service=$1 max_wait=${2:-60}
    echo "  Waiting for $service to be healthy (max ${max_wait}s)..."
    for i in $(seq 1 "$max_wait"); do
        if docker compose ps "$service" --format json 2>/dev/null | grep -q '"healthy"'; then
            echo "  $service is healthy."
            return 0
        fi
        sleep 1
    done
    echo "  WARNING: $service did not become healthy within ${max_wait}s"
    return 1
}

# Helper: find the active e2e_canary backend PID (polls up to 20s).
# pg_stat_activity shows the caller's query (SELECT e2e_canary()), not the
# internal pg_sleep call, so we search for 'e2e_canary'.
find_sleep_pid() {
    local pid=""
    for attempt in $(seq 1 40); do
        pid=$(docker compose exec -T e2e-postgres-1 \
            psql -U postgres -t -A -c \
            "SELECT pid FROM pg_stat_activity WHERE query LIKE '%e2e_canary%' AND query NOT LIKE '%pg_stat_activity%' AND state = 'active' LIMIT 1;" \
            2>/dev/null | tr -d '[:space:]')
        if [ -n "$pid" ]; then
            echo "$pid"
            return 0
        fi
        sleep 0.5
    done
    return 1
}

# Helper: wait for replica to rejoin streaming replication (polls up to max_wait).
wait_replication() {
    local max_wait=${1:-30}
    echo "  Waiting for replica to rejoin replication (max ${max_wait}s)..."
    for i in $(seq 1 "$max_wait"); do
        local in_recovery
        in_recovery=$(docker compose exec -T e2e-postgres-2 \
            psql -U postgres -t -A -c "SELECT pg_is_in_recovery();" 2>/dev/null | tr -d '[:space:]') || true
        if [ "$in_recovery" = "t" ]; then
            echo "  Replica is in recovery mode (streaming)."
            return 0
        fi
        sleep 1
    done
    echo "  WARNING: Replica did not rejoin replication within ${max_wait}s"
    return 1
}

# ── Preflight ────────────────────────────────────────────────────────────────

_ps_output=$(docker compose ps 2>/dev/null || true)
if ! echo "$_ps_output" | grep -q "newrelic-infra.*Up"; then
    echo "ERROR: e2e stack is not running. Run 'make up' first."
    exit 1
fi

MODE="normal"
[ "${FAST:-0}" = "1" ] && MODE="fast"

# SKIP_TO lets you jump directly to a specific test (1-5).
SKIP_TO=${SKIP_TO:-0}

echo "=== PostgreSQL Availability Chaos Test ($MODE) ==="
echo ""
echo "Start time:          $START_TIME"
echo "Collection interval: ${CYCLE}s"
echo "Failure hold:        ${FAIL_DURATION}s (${FAIL_CYCLES} cycles)"
echo "Stabilize hold:      ${STABILIZE_DURATION}s (${STABILIZE_CYCLES} cycles)"
echo ""
echo "Watch your New Relic dashboard — events will appear in real time."

if [ "$SKIP_TO" -gt 0 ]; then
    echo ""
    echo "  >>> Skipping to Test $SKIP_TO (SKIP_TO=$SKIP_TO)"
fi

# ── Baseline ─────────────────────────────────────────────────────────────────

banner "Phase 0: Baseline — confirming all instances are healthy"
echo "  e2e-postgres-1:  $(docker compose ps e2e-postgres-1 --format '{{.Status}}')"
echo "  e2e-postgres-2:  $(docker compose ps e2e-postgres-2 --format '{{.Status}}')"
echo "  e2e-inventorydb: $(docker compose ps e2e-inventorydb --format '{{.Status}}')"
hold "$STABILIZE_DURATION" "baseline collection"

# Per-test timestamps for scoped NerdGraph verification.
T1_START="" T2_START="" T3_START="" T4_START="" T5A_START="" T5B_START=""

# ── Test 1: Container stop (DNS failure) ─────────────────────────────────────

if [ "$SKIP_TO" -le 1 ]; then
T1_START=$(ts)
banner "Test 1/5: Container stop — replica (DNS resolution failure)"
echo "  Stopping e2e-postgres-2..."
docker compose stop e2e-postgres-2
echo "  Container stopped. Agent should report dns_resolution_failed for e2e-postgres-2."
hold "$FAIL_DURATION" "failure detection"

echo "  Restoring e2e-postgres-2..."
docker compose start e2e-postgres-2
wait_healthy e2e-postgres-2
hold "$STABILIZE_DURATION" "recovery stabilization"
fi

# ── Test 2: Network disconnect (DNS failure) ─────────────────────────────────
# Docker network disconnect removes the container from the compose network's DNS.
# The integration's timingDialFunc does an explicit DNS lookup that now fails.

if [ "$SKIP_TO" -le 2 ]; then
T2_START=$(ts)
banner "Test 2/5: Network disconnect — primary (DNS resolution failure)"
PG1_CONTAINER=$(resolve_container e2e-postgres-1)
PG1_NETWORK=$(resolve_network e2e-postgres-1)
echo "  Disconnecting $PG1_CONTAINER from $PG1_NETWORK..."
docker network disconnect "$PG1_NETWORK" "$PG1_CONTAINER"
echo "  Network severed. Docker DNS stops resolving the hostname on this network."
echo "  Agent should report dns_resolution_failed for e2e-postgres-1."
hold "$FAIL_DURATION" "failure detection"

echo "  Reconnecting $PG1_CONTAINER to $PG1_NETWORK..."
docker network connect "$PG1_NETWORK" "$PG1_CONTAINER"
wait_healthy e2e-postgres-1
hold "$STABILIZE_DURATION" "recovery stabilization"
fi

# ── Test 3: Pause container (I/O freeze) ─────────────────────────────────────

if [ "$SKIP_TO" -le 3 ]; then
T3_START=$(ts)
banner "Test 3/5: Container pause — replica (connection hang / timeout)"
echo "  Pausing e2e-postgres-2 (SIGSTOP — process frozen, TCP stays open)..."
docker compose pause e2e-postgres-2
echo "  Container paused. Agent should report timeout for e2e-postgres-2."
hold "$FAIL_DURATION" "failure detection"

echo "  Unpausing e2e-postgres-2..."
docker compose unpause e2e-postgres-2
echo "  Container resumed."
hold "$STABILIZE_DURATION" "recovery stabilization"
fi

# ── Test 4: PostgreSQL password change (auth failure) ────────────────────────

if [ "$SKIP_TO" -le 4 ]; then
T4_START=$(ts)
banner "Test 4/5: Password change — primary (authentication failure)"
echo "  Changing postgres password on e2e-postgres-1..."
docker compose exec -T e2e-postgres-1 \
    psql -U postgres -c "ALTER USER postgres WITH PASSWORD 'wrong_password';" 2>/dev/null
echo "  Password changed. Agent should report pg_error_28 for e2e-postgres-1."
hold "$FAIL_DURATION" "failure detection"

echo "  Restoring postgres password on e2e-postgres-1..."
docker compose exec -T e2e-postgres-1 \
    psql -U postgres -c "ALTER USER postgres WITH PASSWORD 'e2e_test_password';" 2>/dev/null
echo "  Password restored."
hold "$STABILIZE_DURATION" "recovery stabilization"
fi

# ── Test 5: Backend termination + query cancel ───────────────────────────────
#
# The e2e-slowcheck instance uses e2e_canary() as its health check query.
# The function returns 1 instantly by default. We activate a 5-second sleep
# window (UPDATE e2e_chaos SET sleep_seconds = 5) so there is a reliable
# window to find the backend via pg_stat_activity and kill it.
#
# Phase A: pg_cancel_backend — cancels the query, connection stays alive.
#          Server sends SQLSTATE 57014 (query_canceled).
#          classifyError → pg_error_57
#
# Phase B: pg_terminate_backend — destroys the backend mid-query.
#          PostgreSQL sends SQLSTATE 57P01 (admin_shutdown) before closing.
#          classifyError → pg_error_57 (same class as cancel)

if [ "$SKIP_TO" -le 5 ]; then
banner "Test 5/5: Backend kill — slowcheck (query cancel + connection kill)"

# Activate the sleep window so e2e_canary() blocks long enough to catch.
echo "  Activating chaos sleep window (e2e_chaos.sleep_seconds = 5)..."
docker compose exec -T e2e-postgres-1 \
    psql -U postgres -d demo -c "UPDATE e2e_chaos SET sleep_seconds = 5;" 2>/dev/null

# ── Phase A: pg_cancel_backend (query cancelled, connection survives) ─────────

T5A_START=$(ts)
echo ""
echo "  Phase A: pg_cancel_backend — cancel query, keep connection alive"
echo "  Expected error: pg_error_57 (SQLSTATE 57014, query_canceled)"
echo ""

CANCEL_ROUNDS=3
for round in $(seq 1 "$CANCEL_ROUNDS"); do
    echo "  Round $round/$CANCEL_ROUNDS: waiting for e2e_canary backend..."
    PID=$(find_sleep_pid) || true

    if [ -n "$PID" ]; then
        echo "  Found e2e_canary (pg_sleep) on PID $PID — cancelling query..."
        docker compose exec -T e2e-postgres-1 \
            psql -U postgres -c "SELECT pg_cancel_backend($PID);" 2>/dev/null || true
        echo "  Query cancelled. Agent should report pg_error_57."
    else
        echo "  WARNING: e2e_canary backend not found within 20s (round $round). Skipping."
    fi

    if [ "$round" -lt "$CANCEL_ROUNDS" ]; then
        echo "  Waiting for next collection cycle..."
        sleep "$CYCLE"
    fi
done

hold "$STABILIZE_DURATION" "stabilization between phases"

# ── Phase B: pg_terminate_backend (admin_shutdown, SQLSTATE 57P01) ─────────────

T5B_START=$(ts)
echo ""
echo "  Phase B: pg_terminate_backend — destroy connection mid-query"
echo "  Expected error: pg_error_57 (SQLSTATE 57P01, admin_shutdown)"
echo ""

TERMINATE_ROUNDS=3
for round in $(seq 1 "$TERMINATE_ROUNDS"); do
    echo "  Round $round/$TERMINATE_ROUNDS: waiting for e2e_canary backend..."
    PID=$(find_sleep_pid) || true

    if [ -n "$PID" ]; then
        echo "  Found e2e_canary (pg_sleep) on PID $PID — terminating backend..."
        docker compose exec -T e2e-postgres-1 \
            psql -U postgres -c "SELECT pg_terminate_backend($PID);" 2>/dev/null || true
        echo "  Backend terminated. Agent should report pg_error_57 (admin_shutdown)."
    else
        echo "  WARNING: e2e_canary backend not found within 20s (round $round). Skipping."
    fi

    if [ "$round" -lt "$TERMINATE_ROUNDS" ]; then
        echo "  Waiting for next collection cycle..."
        sleep "$CYCLE"
    fi
done

# Deactivate the sleep window so the health check returns to instant response.
echo ""
echo "  Deactivating chaos sleep window (e2e_chaos.sleep_seconds = 0)..."
docker compose exec -T e2e-postgres-1 \
    psql -U postgres -d demo -c "UPDATE e2e_chaos SET sleep_seconds = 0;" 2>/dev/null

hold "$STABILIZE_DURATION" "recovery stabilization"
fi

# ── Summary ──────────────────────────────────────────────────────────────────

END_TIME=$(ts)
END_EPOCH=$(epoch)
ELAPSED=$(( END_EPOCH - START_EPOCH ))
ELAPSED_MIN=$(( ELAPSED / 60 ))
ELAPSED_SEC=$(( ELAPSED % 60 ))

banner "Chaos test complete"
echo ""
echo "  Start:    $START_TIME"
echo "  End:      $END_TIME"
echo "  Duration: ${ELAPSED_MIN}m ${ELAPSED_SEC}s"

#── Recovery check ───────────────────────────────────────────────────────────

echo ""
echo "  Service recovery status:"
for svc in e2e-postgres-1 e2e-postgres-2 e2e-inventorydb newrelic-infra; do
    status=$(docker compose ps "$svc" --format '{{.Status}}' 2>/dev/null || echo "unknown")
    echo "    $svc: $status"
done

echo ""
echo "  Replication status:"
# Give the replica a moment to reconnect after all the chaos.
wait_replication 30 || true
REPL_STATUS=$(docker compose exec -T e2e-postgres-2 \
    psql -U postgres -t -A -c "SELECT CASE WHEN pg_is_in_recovery() THEN 'streaming replica (OK)' ELSE 'NOT in recovery (check replication)' END;" 2>/dev/null || echo "unable to query replica")
echo "  $REPL_STATUS"

# ── Result summary table ─────────────────────────────────────────────────────

echo ""
echo "  Scenarios executed:"
echo ""
echo "  Test  Expected Error Code              Method"
echo "  ----  ------------------------------   --------------------------------"
echo "  1     dns_resolution_failed            Container stop (replica)"
echo "  2     dns_resolution_failed            Network disconnect (primary)"
echo "  3     timeout                          Container pause (replica)"
echo "  4     pg_error_28                      Password change (primary)"
echo "  5A    pg_error_57                      pg_cancel_backend (slowcheck)"
echo "  5B    pg_error_57                      pg_terminate_backend (slowcheck)"

# ── Automated verification (if NR_API_KEY and NR_ACCOUNT_ID are set) ─────────
#
# Each test is verified independently by querying for the specific
# (errorCode, label.instance, time_window) triple. This confirms the right
# instance reported the right error during the right phase — no shortcuts.

declare -a TEST_NAMES=("1 -- Container stop (replica)" "2 -- Network disconnect (primary)" "3 -- Container pause (replica)" "4 -- Password change (primary)" "5A -- Query cancel (slowcheck)" "5B -- Backend terminate (slowcheck)")
declare -a TEST_EXPECTED=("dns_resolution_failed" "dns_resolution_failed" "timeout" "pg_error_28" "pg_error_57" "pg_error_57")
declare -a TEST_STATUS=("---" "---" "---" "---" "---" "---")

# Instance each test targets — used to scope NerdGraph verification.
declare -a TEST_INSTANCES=("e2e-postgres-2" "e2e-postgres-1" "e2e-postgres-2" "e2e-postgres-1" "e2e-slowcheck" "e2e-slowcheck")

if [ -n "${NR_API_KEY:-}" ] && [ -n "${NR_ACCOUNT_ID:-}" ]; then
    banner "Verifying results via NerdGraph"

    # Wait for ingest pipeline to flush. 30s covers typical NR ingest latency.
    echo "  Waiting 30s for NR ingest pipeline..."
    sleep 30

    # nrql_count: run a targeted NRQL count query, return the count (0 if error).
    nrql_count() {
        local nrql="$1"
        local query='{ "query": "{ actor { account(id: '$NR_ACCOUNT_ID') { nrql(query: \"'"$nrql"'\") { results } } } }" }'
        local response
        response=$(curl -s -X POST https://api.newrelic.com/graphql \
            -H "Content-Type: application/json" \
            -H "Api-Key: $NR_API_KEY" \
            -d "$query")
        echo "$response" | jq -r '.data.actor.account.nrql.results[0].count // 0'
    }

    PASS_COUNT=0
    FAIL_COUNT=0

    echo ""
    echo "  Test  Error Code + Instance                      Count  Status"
    echo "  ----  ------------------------------------------  -----  ------"

    for i in 0 1 2 3 4 5; do
        instance="${TEST_INSTANCES[$i]}"
        expected="${TEST_EXPECTED[$i]}"
        test_name="${TEST_NAMES[$i]}"

        # Build error code IN clause (handles alternatives like "connection_refused / timeout").
        code_in=""
        for code_part in $(echo "$expected" | tr '/' ' '); do
            code_part=$(echo "$code_part" | tr -d ' ')
            [ -n "$code_in" ] && code_in="$code_in, "
            code_in="$code_in'$code_part'"
        done

        # Tests 1-4: (errorCode, instance) pairs are unique across the full run,
        # so SINCE $START_TIME is sufficient — no UNTIL needed.
        #
        # Tests 5A/5B: both produce pg_error_57 @ e2e-slowcheck, so we must
        # scope each to its own time window to verify independently.
        time_clause="SINCE '$START_TIME'"
        if [ "$i" -eq 4 ] && [ -n "$T5A_START" ]; then
            # 5A: from phase A start to phase B start.
            time_clause="SINCE '$T5A_START' UNTIL '${T5B_START:-$END_TIME}'"
        elif [ "$i" -eq 5 ] && [ -n "$T5B_START" ]; then
            # 5B: from phase B start to end of test.
            time_clause="SINCE '$T5B_START'"
        fi

        nrql="SELECT count(*) FROM PostgresqlHealthSample WHERE hasError = 1 AND errorCode IN ($code_in) AND label.instance = '$instance' $time_clause"

        count=$(nrql_count "$nrql")

        label="$expected @ $instance"

        if [ "$count" -gt 0 ] 2>/dev/null; then
            printf "  %-4s  %-44s  %5s  PASS\n" "${test_name%%--*}" "$label" "$count"
            TEST_STATUS[$i]="PASS"
            PASS_COUNT=$((PASS_COUNT + 1))
        else
            printf "  %-4s  %-44s  %5s  MISS\n" "${test_name%%--*}" "$label" "0"
            TEST_STATUS[$i]="MISS"
            FAIL_COUNT=$((FAIL_COUNT + 1))
        fi
    done

    echo ""
    TOTAL=$((PASS_COUNT + FAIL_COUNT))
    if [ "$FAIL_COUNT" -eq 0 ]; then
        echo "  Result: $PASS_COUNT/$TOTAL passed. All tests verified."
    else
        echo "  Result: $PASS_COUNT/$TOTAL passed, $FAIL_COUNT missed."
    fi
    echo ""

else
    echo ""
    echo "  ── Verify in New Relic (manual) ──"
    echo ""
    echo "  Set NR_API_KEY and NR_ACCOUNT_ID to enable automated verification."
    echo ""
    echo "  Per-test verification queries:"
    echo "    SELECT count(*) FROM PostgresqlHealthSample"
    echo "      WHERE hasError = 1 AND errorCode = '<code>' AND label.instance = '<instance>'"
    echo "      SINCE '$START_TIME'"
    echo ""
    echo "  Error summary:"
    echo "    SELECT count(*) FROM PostgresqlHealthSample"
    echo "      WHERE hasError = 1 FACET errorCode, label.instance"
    echo "      SINCE '$START_TIME'"
    echo ""
fi

# ── Auto-update PR.md ────────────────────────────────────────────────────────
# Uses python3 for multiline replacement (awk chokes on embedded newlines
# from the log file content passed via -v).

if [ -f "$PR_MD" ]; then
    # Wait briefly for tee buffer to flush.
    sleep 1

    TIMESTAMP=$(date '+%Y-%m-%d %H:%M:%S')

    # Build the replacement section using a heredoc.
    SECTION=$(cat <<SECTION_EOF
## E2E Validation Results

_Last run: $TIMESTAMP ($MODE mode, ${FAIL_CYCLES} cycle(s) per phase)_

| Test | Expected | Status |
|---|---|---|
| ${TEST_NAMES[0]} | \`${TEST_EXPECTED[0]}\` | ${TEST_STATUS[0]} |
| ${TEST_NAMES[1]} | \`${TEST_EXPECTED[1]}\` | ${TEST_STATUS[1]} |
| ${TEST_NAMES[2]} | \`${TEST_EXPECTED[2]}\` | ${TEST_STATUS[2]} |
| ${TEST_NAMES[3]} | \`${TEST_EXPECTED[3]}\` | ${TEST_STATUS[3]} |
| ${TEST_NAMES[4]} | \`${TEST_EXPECTED[4]}\` | ${TEST_STATUS[4]} |
| ${TEST_NAMES[5]} | \`${TEST_EXPECTED[5]}\` | ${TEST_STATUS[5]} |

All services recovered. Replication: $REPL_STATUS

<details>
<summary>Full chaos test output (make test-$MODE)</summary>

\`\`\`
$(cat "$LOG_FILE")
\`\`\`

</details>

SECTION_EOF
)

    echo "  Updating $PR_MD with e2e results..."

    # Replace the existing section or append if not found.
    if grep -q "^## E2E Validation Results" "$PR_MD"; then
        # Remove old section (from "## E2E Validation Results" to the next "## " heading or EOF)
        # and splice in the new content. python3 handles multiline strings natively.
        python3 -c "
import re, sys
content = open('$PR_MD').read()
pattern = r'## E2E Validation Results.*?(?=\n## [^#]|\n---\n|\Z)'
replacement = sys.stdin.read()
result = re.sub(pattern, replacement.rstrip(), content, count=1, flags=re.DOTALL)
open('$PR_MD', 'w').write(result)
" <<< "$SECTION"
        echo "  PR.md updated with e2e results."
    else
        printf "\n%s\n" "$SECTION" >> "$PR_MD"
        echo "  PR.md updated with e2e results (appended)."
    fi
fi

rm -f "$LOG_FILE"
