.DEFAULT_GOAL := help

# ── Helpers ───────────────────────────────────────────────────────────────────

.PHONY: help
help:
	@echo ""
	@echo "  Local PostgreSQL testing (primary + replica + inventorydb):"
	@echo "    make setup              copy .env.example -> .env (edit before first run)"
	@echo "    make up                 build image + start primary, replica, inventorydb, and agent"
	@echo "    make down               stop and remove containers + volumes"
	@echo "    make logs               tail infra agent logs"
	@echo "    make agent-shell        open a shell inside the agent container"
	@echo "    make run-once           exec integration against primary, print JSON"
	@echo "    make run-once-replica   exec integration against replica, print JSON"
	@echo "    make run-once-inventorydb  exec integration against inventorydb, print JSON"
	@echo "    make repl-status        show replica replication status"
	@echo "    make test-availability  chaos test: trigger real failure scenarios (~6 min)"
	@echo "    make test-fast          same as above but 1 cycle per phase (~2 min)"
	@echo "    make test-chaos         full lifecycle: up, 10m warmup, chaos, 10m cooldown, down"
	@echo ""
	@echo "  Aurora / remote PostgreSQL testing:"
	@echo "    make up-aurora          start infra agent only (no local postgres)"
	@echo "    make down-aurora        stop aurora stack"
	@echo ""
	@echo "  Rebuild after source changes:"
	@echo "    make rebuild            force-rebuild the agent image and restart"
	@echo ""

# ── Environment ──────────────────────────────────────────────────────────────

.PHONY: setup
setup:
	@if [ -f .env ]; then \
		echo ".env already exists -- skipping. Edit it directly if needed."; \
	else \
		cp .env.example .env; \
		echo "Created .env from .env.example."; \
		echo "Set NR_LICENSE_KEY as an environment variable (preferred) or edit .env."; \
	fi

# ── Local stack ──────────────────────────────────────────────────────────────

.PHONY: up
up:
	docker compose up -d --build
	@echo ""
	@echo "Stack is up (primary: e2e-postgres-1, replica: e2e-postgres-2, inventorydb: e2e-inventorydb)."
	@echo "Give the agent ~30s to send its first collection."
	@echo "Run 'make logs' to watch, or query New Relic directly."

.PHONY: down
down:
	docker compose down -v

.PHONY: rebuild
rebuild:
	docker compose down
	docker compose build --no-cache newrelic-infra
	docker compose up -d

.PHONY: logs
logs:
	docker compose logs -f newrelic-infra

.PHONY: agent-shell
agent-shell:
	docker compose exec newrelic-infra /bin/sh

# Run the integration binary directly inside the agent container and print the
# raw JSON payload — useful for inspecting output without waiting for NR ingest.
.PHONY: run-once
run-once:
	@docker compose exec newrelic-infra \
		/var/db/newrelic-infra/newrelic-integrations/bin/nri-postgresql \
		-username postgres \
		-password e2e_test_password \
		-hostname e2e-postgres-1 \
		-port 5432 \
		-database demo \
		-collection_list ALL \
		-collect_connection_timing=true \
		-enable_availability_check=true \
		-availability_check_timeout_ms=5000 \
		-collect_query_telemetry=true \
		| python3 -m json.tool 2>/dev/null || \
		docker compose exec newrelic-infra \
		/var/db/newrelic-infra/newrelic-integrations/bin/nri-postgresql \
		-username postgres \
		-password e2e_test_password \
		-hostname e2e-postgres-1 \
		-port 5432 \
		-database demo \
		-collection_list ALL \
		-collect_connection_timing=true \
		-enable_availability_check=true \
		-availability_check_timeout_ms=5000 \
		-collect_query_telemetry=true

.PHONY: run-once-replica
run-once-replica:
	@docker compose exec newrelic-infra \
		/var/db/newrelic-infra/newrelic-integrations/bin/nri-postgresql \
		-username postgres \
		-password e2e_test_password \
		-hostname e2e-postgres-2 \
		-port 5432 \
		-database demo \
		-collection_list ALL \
		-collect_connection_timing=true \
		-collect_query_telemetry=true \
		| python3 -m json.tool 2>/dev/null || \
		docker compose exec newrelic-infra \
		/var/db/newrelic-infra/newrelic-integrations/bin/nri-postgresql \
		-username postgres \
		-password e2e_test_password \
		-hostname e2e-postgres-2 \
		-port 5432 \
		-database demo \
		-collection_list ALL \
		-collect_connection_timing=true \
		-collect_query_telemetry=true

.PHONY: run-once-inventorydb
run-once-inventorydb:
	@docker compose exec newrelic-infra \
		/var/db/newrelic-infra/newrelic-integrations/bin/nri-postgresql \
		-username postgres \
		-password e2e_test_password \
		-hostname e2e-inventorydb \
		-port 5432 \
		-database inventory \
		-collection_list ALL \
		-collect_connection_timing=true \
		-enable_availability_check=true \
		-availability_check_timeout_ms=5000 \
		-collect_query_telemetry=true \
		| python3 -m json.tool 2>/dev/null || \
		docker compose exec newrelic-infra \
		/var/db/newrelic-infra/newrelic-integrations/bin/nri-postgresql \
		-username postgres \
		-password e2e_test_password \
		-hostname e2e-inventorydb \
		-port 5432 \
		-database inventory \
		-collection_list ALL \
		-collect_connection_timing=true \
		-enable_availability_check=true \
		-availability_check_timeout_ms=5000 \
		-collect_query_telemetry=true

# Show replication status on the replica.
.PHONY: repl-status
repl-status:
	@docker compose exec e2e-postgres-2 \
		psql -U postgres -c "SELECT pid, status, sender_host, sender_port, received_lsn, latest_end_lsn FROM pg_stat_wal_receiver;" 2>/dev/null

# Run availability error classification tests against the running stack.
.PHONY: test-availability
test-availability:
	@./test-availability.sh

# Fast mode — 1 cycle per phase (~2 min total). Good for iterating.
.PHONY: test-fast
test-fast:
	@FAST=1 ./test-availability.sh

# Full end-to-end: bring up the stack, let the agent collect baseline data for
# 10 minutes, run the chaos tests, collect recovery data for another 10 minutes,
# then tear everything down. Use WARMUP and COOLDOWN to override the durations.
.PHONY: test-chaos
test-chaos:
	@test -n "$$NR_LICENSE_KEY"  || { echo "ERROR: NR_LICENSE_KEY is not set.";  exit 1; }
	@test -n "$$NR_ACCOUNT_ID"  || { echo "ERROR: NR_ACCOUNT_ID is not set.";  exit 1; }
	@test -n "$$NR_API_KEY"     || { echo "ERROR: NR_API_KEY is not set.";     exit 1; }
	@echo "=== Full chaos test lifecycle ==="
	$(MAKE) up
	@echo ""; echo "Warming up — collecting baseline data for $${WARMUP:-600}s..."; sleep $${WARMUP:-600}
	$(MAKE) test-availability
	@echo ""; echo "Cooling down — collecting recovery data for $${COOLDOWN:-600}s..."; sleep $${COOLDOWN:-600}
	$(MAKE) down
	@echo ""; echo "Done."

# ── Aurora stack ─────────────────────────────────────────────────────────────

.PHONY: up-aurora
up-aurora:
	@if [ -z "$$AURORA_HOST" ] && ! grep -q "^AURORA_HOST=" .env 2>/dev/null; then \
		echo "ERROR: AURORA_HOST is not set. Add Aurora credentials to .env first."; \
		exit 1; \
	fi
	docker compose -f docker-compose.yml -f docker-compose.aurora.yml up -d --build

.PHONY: down-aurora
down-aurora:
	docker compose -f docker-compose.yml -f docker-compose.aurora.yml down -v
