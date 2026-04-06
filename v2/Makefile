# AMP v2 — development and operational tasks
# Run from the v2/ directory.

.DEFAULT_GOAL := help

# ---- config ----

API_DIR   := ./amp-api
UI_DIR    := ./ui
COMPOSE   := docker compose

DB_URL    ?= postgres://amp:amp@localhost:5432/amp?sslmode=disable
TEST_DB   ?= postgres://amp:amp@localhost:5432/amp_test?sslmode=disable

PID_DIR   := .pids
API_PID   := $(PID_DIR)/api.pid
UI_PID    := $(PID_DIR)/ui.pid

TYPESENSE_URL ?= http://localhost:8108
TYPESENSE_KEY ?= amp-local-dev

.PHONY: help up down restart logs \
        dev dev-stop \
        api-run api-build api-test api-test-verbose api-lint \
        ui-dev ui-build ui-preview \
        db-up db-reset db-shell \
        kb-setup kb-status kb-reindex \
        clean check

# ---- help ----

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} \
	/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# ---- full stack ----

up: dev-stop ## Start everything (postgres + amp-api + ui) via docker compose
	@# Kill anything holding the ports compose needs before handing control over
	@for port in 8000 3001 5173; do \
		pids=$$(lsof -ti:$$port 2>/dev/null); \
		if [ -n "$$pids" ]; then \
			echo "Freeing port $$port (pid $$pids)..."; \
			echo $$pids | xargs kill 2>/dev/null || true; \
			sleep 0.3; \
		fi; \
	done
	@# Bundle any certs in certs/ into a single file for containers that can't build from source
	@if ls certs/*.pem certs/*.crt 2>/dev/null | grep -v bundle.pem | grep -q .; then \
		cat $$(ls certs/*.pem certs/*.crt 2>/dev/null | grep -v bundle.pem) > certs/bundle.pem; \
		echo "Built certs/bundle.pem from $$(ls certs/*.pem certs/*.crt 2>/dev/null | grep -v bundle.pem | wc -l | tr -d ' ') cert file(s)."; \
	fi
	$(COMPOSE) up -d --remove-orphans
	@echo ""
	@echo "  UI         → http://localhost:5173"
	@echo "  API        → http://localhost:3001"
	@echo "  MCP        → http://localhost:8000/sse"
	@echo "  Typesense  → http://localhost:8108"
	@echo "  Ollama     → http://localhost:11434"
	@echo ""
	@echo "  First run: make kb-setup (pulls nomic-embed-text ~274MB)"
	@echo ""

down: dev-stop ## Stop containers AND any local dev processes (api-run / ui-dev)
	$(COMPOSE) down

restart: ## Restart all containers
	$(COMPOSE) restart

logs: ## Follow logs for all services (Ctrl-C to stop)
	$(COMPOSE) logs -f

logs-api: ## Follow amp-api logs only
	$(COMPOSE) logs -f amp-api

logs-ui: ## Follow ui logs only
	$(COMPOSE) logs -f ui

logs-dev: ## Tail local dev process logs (api + ui)
	@echo "Tailing amp-api on :3001 and ui on :5173 — Ctrl-C to stop"
	@tail -f /tmp/amp-api.log /tmp/amp-ui.log 2>/dev/null || echo "(no log files found — processes log to their own terminals)"

# ---- local dev (no docker for api + ui) ----
# Processes are tracked via PID files so `make down` or `make dev-stop`
# can kill them cleanly from any terminal.

dev: db-up ## Start api + ui locally (background, tracked by PID files)
	@mkdir -p $(PID_DIR)
	@echo "Starting amp-api (MCP :8000, REST :3001)..."
	@cd $(API_DIR) && DATABASE_URL=$(DB_URL) go run ./cmd/server & echo $$! > ../$(API_PID)
	@echo "Starting ui dev server (:5173)..."
	@cd $(UI_DIR) && npm run dev & echo $$! > ../$(UI_PID)
	@echo ""
	@echo "  UI  → http://localhost:5173"
	@echo "  API → http://localhost:3001"
	@echo "  MCP → http://localhost:8000/sse"
	@echo ""
	@echo "  Logs: make logs-dev"
	@echo "  Stop: make dev-stop  (or  make down)"
	@echo ""

dev-stop: ## Stop local api and ui processes started by 'make dev'
	@if [ -f $(API_PID) ]; then \
		pid=$$(cat $(API_PID)); \
		if kill -0 $$pid 2>/dev/null; then \
			echo "Stopping amp-api (pid $$pid)..."; \
			kill $$pid 2>/dev/null || true; \
		fi; \
		rm -f $(API_PID); \
	fi
	@if [ -f $(UI_PID) ]; then \
		pid=$$(cat $(UI_PID)); \
		if kill -0 $$pid 2>/dev/null; then \
			echo "Stopping ui (pid $$pid)..."; \
			kill $$pid 2>/dev/null || true; \
		fi; \
		rm -f $(UI_PID); \
	fi
	@# Also kill by port as a safety net in case PID files are stale
	@lsof -ti:3001 | xargs kill 2>/dev/null || true
	@lsof -ti:8000 | xargs kill 2>/dev/null || true
	@lsof -ti:5173 | xargs kill 2>/dev/null || true
	@echo "Local dev processes stopped."

api-run: ## Run the Go API in the foreground (logs to stdout)
	cd $(API_DIR) && DATABASE_URL=$(DB_URL) go run ./cmd/server

api-build: ## Build the Go API binary
	cd $(API_DIR) && go build -o bin/amp-api ./cmd/server
	@echo "Built: amp-api/bin/amp-api"

# ---- tests ----

api-test: ## Run all API integration tests (requires postgres on 5432)
	cd $(API_DIR) && TEST_DATABASE_URL=$(TEST_DB) go test ./cmd/server/ -count=1 -timeout 60s

api-test-verbose: ## Run all tests with full log output
	cd $(API_DIR) && TEST_DATABASE_URL=$(TEST_DB) go test ./cmd/server/ -v -count=1 -timeout 120s

api-test-integration: ## Run Integration tests only
	cd $(API_DIR) && TEST_DATABASE_URL=$(TEST_DB) go test ./cmd/server/ -v -run Integration -count=1 -timeout 60s

api-test-mcp: ## Run MCP tests only
	cd $(API_DIR) && TEST_DATABASE_URL=$(TEST_DB) go test ./cmd/server/ -v -run TestMCP -count=1 -timeout 60s

api-test-isolation: ## Run isolation tests only
	cd $(API_DIR) && TEST_DATABASE_URL=$(TEST_DB) go test ./cmd/server/ -v -run TestIsolation -count=1 -timeout 60s

api-test-recovery: ## Run recovery/crash tests only
	cd $(API_DIR) && TEST_DATABASE_URL=$(TEST_DB) go test ./cmd/server/ -v -run TestRecovery -count=1 -timeout 60s

api-test-hierarchy: ## Run hierarchy and ticket history tests only
	cd $(API_DIR) && TEST_DATABASE_URL=$(TEST_DB) go test ./cmd/server/ -v -run TestHierarchy -count=1 -timeout 60s

# ---- Go quality ----

api-lint: ## Run go vet
	cd $(API_DIR) && go vet ./...

api-tidy: ## Tidy go modules
	cd $(API_DIR) && go mod tidy

# ---- UI ----

ui-install: ## Install UI npm dependencies
	cd $(UI_DIR) && npm install

ui-dev: ## Run Vite dev server (proxies /api to localhost:3001)
	cd $(UI_DIR) && npm run dev

ui-build: ## Build production UI bundle
	cd $(UI_DIR) && npm run build
	@echo "Built: ui/dist/"

ui-preview: ui-build ## Build and preview production bundle locally
	cd $(UI_DIR) && npm run preview

ui-typecheck: ## Run TypeScript type check on UI
	cd $(UI_DIR) && npx tsc --noEmit

# ---- database ----

db-up: ## Start postgres container only
	$(COMPOSE) up -d postgres
	@echo "Waiting for postgres..."
	@until docker exec amp-postgres pg_isready -U amp -q 2>/dev/null; do sleep 1; done
	@echo "Postgres ready."

db-reset: ## Drop and recreate the test database (leaves main db intact)
	docker exec amp-postgres psql -U amp -c "DROP DATABASE IF EXISTS amp_test;" 2>/dev/null || true
	docker exec amp-postgres psql -U amp -c "CREATE DATABASE amp_test;" 2>/dev/null || true
	@echo "Test database reset."

db-shell: ## Open a psql shell on the amp database
	docker exec -it amp-postgres psql -U amp -d amp

db-shell-test: ## Open a psql shell on the test database
	docker exec -it amp-postgres psql -U amp -d amp_test

# ---- knowledge base ----

kb-setup: ## Pull nomic-embed-text model into Ollama (run once after first compose up)
	@echo "Pulling nomic-embed-text embedding model into Ollama..."
	@docker exec amp-ollama ollama pull nomic-embed-text
	@echo "Model ready. Typesense will auto-embed on first write."

kb-status: ## Show Typesense health and collection list
	@echo "=== Typesense health ==="
	@curl -s $(TYPESENSE_URL)/health | python3 -m json.tool 2>/dev/null || echo "Typesense not reachable at $(TYPESENSE_URL)"
	@echo ""
	@echo "=== Collections ==="
	@curl -s -H "X-TYPESENSE-API-KEY: $(TYPESENSE_KEY)" $(TYPESENSE_URL)/collections | python3 -m json.tool 2>/dev/null || true

kb-reindex: ## Re-embed all KB docs for a project (PROJECT_ID=1 make kb-reindex)
	@if [ -z "$(PROJECT_ID)" ]; then echo "Usage: PROJECT_ID=1 make kb-reindex"; exit 1; fi
	curl -s -X POST http://localhost:3001/api/kb/reindex?project_id=$(PROJECT_ID) | python3 -m json.tool

# ---- builds ----

build: api-build ui-build ## Build both API binary and UI bundle

# ---- cleanup ----

clean: dev-stop ## Stop dev processes and remove built artifacts
	rm -rf $(API_DIR)/bin $(UI_DIR)/dist $(PID_DIR)
	@echo "Cleaned."

# ---- global config sync ----

sync-global: ## Merge v2 skills, prompts, and opencode.json into ~/.config/opencode (upsert)
	./scripts/sync-global-config.sh

# ---- combined checks (run before commit) ----

check: api-lint ui-typecheck api-test ## Run lint + typecheck + tests
	@echo ""
	@echo "All checks passed."
