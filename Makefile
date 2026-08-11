SHELL := bash
.DEFAULT_GOAL := help

BIN        := bin/echo
PKG        := github.com/jonathanng/echo
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
DATE       ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w \
              -X $(PKG)/internal/version.Version=$(VERSION) \
              -X $(PKG)/internal/version.Commit=$(COMMIT) \
              -X $(PKG)/internal/version.Date=$(DATE)

SQLC       := go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
GOOSE      := go run github.com/pressly/goose/v3/cmd/goose@latest

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

## ---- build ----------------------------------------------------------------

.PHONY: build
build: web ## Build the server with the web client embedded
	CGO_ENABLED=0 go build -trimpath -tags embedweb -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/echo

.PHONY: build-server
build-server: ## Build the server only (no client, no Node required)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/echo

.PHONY: web
web: web/node_modules ## Build the web client into internal/webui/dist
	cd web && npm run build

web/node_modules: web/package.json web/package-lock.json
	cd web && npm ci
	@touch $@

## ---- codegen ---------------------------------------------------------------

.PHONY: generate
generate: sqlc types ## Run all code generation

.PHONY: sqlc
sqlc: ## Generate typed Go from internal/db/queries
	$(SQLC) generate

openapi.yaml: $(shell find cmd internal -name '*.go' -not -name '*_test.go')
	go run ./cmd/echo openapi > $@

.PHONY: openapi
openapi: openapi.yaml ## Regenerate the OpenAPI document

.PHONY: types
types: openapi.yaml web/node_modules ## Generate the client's TypeScript API types
	cd web && npm run gen:api

## ---- quality ---------------------------------------------------------------

.PHONY: test
test: ## Run unit tests (no Docker required)
	go test -race ./...

.PHONY: test-integration
test-integration: ## Run integration tests (requires a running Docker daemon)
	@# testcontainers resolves the daemon itself, but its fallback order can pick
	@# a stale socket when several Docker contexts exist (a leftover Docker
	@# Desktop entry alongside colima, say). Taking the endpoint from the active
	@# context is unambiguous. The socket override tells Ryuk, which runs inside
	@# the VM, where the daemon socket lives there — /var/run/docker.sock on
	@# colima, Docker Desktop, and GitHub runners alike.
	@endpoint=$$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null); \
	if [ -n "$$endpoint" ]; then \
		export DOCKER_HOST="$$endpoint"; \
		export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock; \
	fi; \
	go test -race -tags integration -timeout 15m ./...

.PHONY: lint
lint: ## Vet both build configurations
	go vet ./...
	go vet -tags integration ./...
	gofmt -l . | tee /dev/stderr | (! read)

.PHONY: fmt
fmt: ## Format Go sources
	gofmt -w .

## ---- database --------------------------------------------------------------

.PHONY: migrate
migrate: ## Apply migrations against ECHO_DATABASE_URL
	go run ./cmd/echo migrate

.PHONY: migration
migration: ## Create a migration: make migration name=add_users
	@test -n "$(name)" || { echo "usage: make migration name=add_users"; exit 1; }
	$(GOOSE) -dir internal/db/migrations create $(name) sql

## ---- compose ---------------------------------------------------------------

.PHONY: dev-up
dev-up: ## Start Postgres and a local Dex identity provider
	docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d db dex

# Dev instance URL. Override to move off 8080: make dev-up dev-server ECHO_DEV_BASE_URL=http://localhost:8099
export ECHO_DEV_BASE_URL ?= http://localhost:8080

.PHONY: dev-down
dev-down: ## Stop the development stack
	docker compose -f docker-compose.yml -f docker-compose.dev.yml down

.PHONY: dev-server
dev-server: ## Run the server on the host against the dev stack (see docker-compose.dev.yml)
	ECHO_DATABASE_URL="postgres://echo:echo@localhost:55432/echo?sslmode=disable" \
	ECHO_ADDR="$(shell echo $(ECHO_DEV_BASE_URL) | sed -E 's#^https?://[^:/]*##')" \
	ECHO_BASE_URL="$(ECHO_DEV_BASE_URL)" \
	ECHO_OIDC_ISSUER_URL="http://127.0.0.1:5556/dex" \
	ECHO_OIDC_CLIENT_ID="echo" \
	ECHO_OIDC_CLIENT_SECRET="echo-dev-secret" \
	ECHO_OIDC_NAME="Dex" \
	ECHO_CACHE_DIR="./cache" \
	ECHO_LOG_LEVEL="debug" \
	go run ./cmd/echo serve

.PHONY: up
up: ## Start the stack in the background
	docker compose up -d --build

.PHONY: down
down: ## Stop the stack
	docker compose down

.PHONY: logs
logs: ## Follow application logs
	docker compose logs -f echo

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin internal/webui/dist openapi.yaml
