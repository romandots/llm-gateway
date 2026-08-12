# Entry point for every routine operation. `make` alone prints this list.
#
# Targets that change state (apply, key-revoke, key-rotate, restore) ask for
# confirmation unless YES=1 is passed, so that `make` in unfamiliar hands does
# not break production by accident.

SHELL := /bin/bash
.DEFAULT_GOAL := help

GO             ?= go
GOBIN          ?= $(shell $(GO) env GOPATH)/bin
BIN            := bin/gwctl
GWCTL          ?= ./$(BIN)
COMPOSE        ?= docker compose -f deploy/docker-compose.yml --env-file deploy/.env
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS        := -s -w -X main.version=$(VERSION)
BACKUP_DIR     ?= backups
POSTGRES_USER  ?= litellm
POSTGRES_DB    ?= litellm

# Confirmation switch shared by the destructive targets.
YES ?=
ifeq ($(YES),1)
CONFIRM := --yes
else
CONFIRM :=
endif

# Optional arguments of the management targets.
CONSUMER ?=
GRACE    ?= 24h
BY       ?= consumer
SINCE    ?= 7d
SERVICE  ?=
FILE     ?=
OUTPUT   ?= table

.PHONY: help
help: ## Show this help
	@echo "LLM gateway — make targets"
	@echo
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Variables: CONSUMER=<name> GRACE=24h BY=consumer|alias|model SINCE=7d SERVICE=<name> FILE=<path> YES=1"

# ----------------------------------------------------------------- build

.PHONY: build
build: ## Build gwctl into bin/gwctl
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/gwctl

.PHONY: install
install: ## Install gwctl into $(GOBIN)
	$(GO) install -trimpath -ldflags "$(LDFLAGS)" ./cmd/gwctl

.PHONY: fmt
fmt: ## Format the code (gofmt + goimports when available)
	gofmt -w $(shell find . -name '*.go' -not -path './vendor/*')
	@command -v goimports >/dev/null 2>&1 && goimports -w $(shell find . -name '*.go' -not -path './vendor/*') || \
		echo "goimports not installed, skipped (go install golang.org/x/tools/cmd/goimports@latest)"

.PHONY: lint
lint: ## Run golangci-lint
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint is not installed: https://golangci-lint.run/welcome/install/"; exit 1; }
	golangci-lint run

.PHONY: test
test: ## Run the test suite with race detection and coverage
	$(GO) test ./... -race -cover -coverpkg=./internal/... -coverprofile=coverage.out
	@$(GO) tool cover -func=coverage.out | tail -1

.PHONY: cover
cover: test ## Open the coverage report in a browser
	$(GO) tool cover -html=coverage.out

# ----------------------------------------------------------------- stack

.PHONY: up
up: deploy/.env deploy/litellm/config.yaml ## Start the stack (litellm, postgres, redis, caddy)
	$(COMPOSE) up -d
	@echo "stack is starting; check it with: make health"

.PHONY: down
down: ## Stop the stack
	$(COMPOSE) down

.PHONY: restart
restart: down up ## Restart the stack

.PHONY: logs
logs: ## Follow logs (SERVICE=litellm for one service)
	$(COMPOSE) logs -f $(SERVICE)

.PHONY: ps
ps: ## Show container status
	$(COMPOSE) ps

deploy/.env:
	@echo "deploy/.env is missing. Copy deploy/.env.example and fill it in:"
	@echo "    cp deploy/.env.example deploy/.env"
	@exit 1

# The proxy cannot start without its generated configuration, and the
# configuration cannot be rendered by a proxy that is not running.
deploy/litellm/config.yaml: config/proxy.yaml config/models.yaml build
	$(GWCTL) apply --render-only --yes

# ------------------------------------------------------------ management

.PHONY: validate
validate: build ## Check the configuration locally (schema, references, secrets in git)
	$(GWCTL) validate --output $(OUTPUT)

.PHONY: diff
diff: build ## Show the difference between the configuration and the proxy
	$(GWCTL) diff --output $(OUTPUT)

.PHONY: apply
apply: build ## Bring the proxy in line with the configuration
	$(GWCTL) apply $(CONFIRM)

.PHONY: apply-dry
apply-dry: build ## Print the plan without changing anything
	$(GWCTL) apply --dry-run

.PHONY: key-issue
key-issue: build require-consumer ## Issue a key: make key-issue CONSUMER=my-bot
	$(GWCTL) key issue $(CONSUMER)

.PHONY: key-list
key-list: build ## List the keys the gateway manages
	$(GWCTL) key list --output $(OUTPUT)

.PHONY: key-revoke
key-revoke: build require-consumer ## Revoke a key: make key-revoke CONSUMER=my-bot [GRACE=24h]
	$(GWCTL) key revoke $(CONSUMER) --grace $(GRACE) $(CONFIRM)

.PHONY: key-rotate
key-rotate: build require-consumer ## Rotate a key: make key-rotate CONSUMER=my-bot [GRACE=24h]
	$(GWCTL) key rotate $(CONSUMER) --grace $(GRACE) $(CONFIRM)

.PHONY: spend
spend: build ## Spend report: make spend [BY=consumer|alias|model] [SINCE=7d]
	$(GWCTL) spend --by $(BY) --since $(SINCE) --output $(OUTPUT)

.PHONY: models
models: build ## List aliases and the vendor models serving them
	$(GWCTL) models --output $(OUTPUT)

.PHONY: health
health: build ## Check the proxy, its dependencies and every provider
	$(GWCTL) health --output $(OUTPUT)

.PHONY: require-consumer
require-consumer:
	@test -n "$(CONSUMER)" || { echo "CONSUMER is required, e.g. make $(MAKECMDGOALS) CONSUMER=my-bot"; exit 1; }

# ----------------------------------------------------------- maintenance

.PHONY: backup
backup: ## Dump Postgres into backups/YYYY-MM-DD-HHMM.sql.gz
	@mkdir -p $(BACKUP_DIR)
	@out=$(BACKUP_DIR)/$$(date -u +%Y-%m-%d-%H%M).sql.gz; \
	$(COMPOSE) exec -T postgres pg_dump -U $(POSTGRES_USER) -d $(POSTGRES_DB) | gzip > $$out && \
	echo "wrote $$out"

.PHONY: restore
restore: ## Restore Postgres from a dump: make restore FILE=backups/....sql.gz
	@test -n "$(FILE)" || { echo "FILE is required, e.g. make restore FILE=backups/2026-08-12-1200.sql.gz"; exit 1; }
	@test -f "$(FILE)" || { echo "no such file: $(FILE)"; exit 1; }
	@if [ "$(YES)" != "1" ]; then \
		read -p "Restore $(FILE) over the current database? Every key and spend record is replaced. [y/N]: " answer; \
		case "$$answer" in y|Y|yes|YES) ;; *) echo "aborted"; exit 1;; esac; \
	fi
	gunzip -c $(FILE) | $(COMPOSE) exec -T postgres psql -U $(POSTGRES_USER) -d $(POSTGRES_DB)
	@echo "restored; restart the proxy: make restart"

.PHONY: smoke
smoke: ## Run the contract smoke test against a running stack
	./test/smoke/smoke.sh

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin coverage.out
