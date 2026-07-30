# LinkCtrl development tasks.
#
# A Taskfile.yml with the same targets is provided for contributors on Windows
# without make. Keep the two in sync.

SHELL := /bin/sh
.DEFAULT_GOAL := help

BIN        := bin
MODULE     := github.com/DevOfPie/LinkCtrl
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
DATE       ?= $(shell git log -1 --format=%cI 2>/dev/null || echo unknown)

LDFLAGS := -s -w \
	-X '$(MODULE)/internal/build.version=$(VERSION)' \
	-X '$(MODULE)/internal/build.commit=$(COMMIT)' \
	-X '$(MODULE)/internal/build.date=$(DATE)'

# Postgres/Redis for local development come from the compose override, which
# publishes on non-default ports so they do not collide with anything the
# developer already has installed.
DEV_DATABASE_URL ?= postgres://linkctrl:linkctrl@localhost:55432/linkctrl?sslmode=disable
DEV_REDIS_URL    ?= redis://localhost:56379/0

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_.-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

## ---- build ----------------------------------------------------------------

.PHONY: build
build: ## Build the server and the operator CLI
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/ ./cmd/...

.PHONY: run
run: ## Run the server against the dev database
	go run -ldflags "$(LDFLAGS)" ./cmd/linkctrl

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN) dist cover.out coverage.html

## ---- quality --------------------------------------------------------------

.PHONY: test
test: ## Unit tests with the race detector
	go test -race -covermode=atomic -coverprofile=cover.out ./...

.PHONY: test-integration
test-integration: ## Integration tests (needs Postgres and Redis)
	go test -race -tags=integration ./test/integration/...

.PHONY: cover
cover: test ## Open the coverage report
	go tool cover -html=cover.out -o coverage.html

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: vuln
vuln: ## Check for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: tidy
tidy: ## Tidy and verify modules
	go mod tidy
	go mod verify

.PHONY: check
check: tidy lint test ## Everything CI runs, short of integration tests

## ---- codegen --------------------------------------------------------------

.PHONY: generate
generate: sqlc openapi ## Regenerate all generated code

.PHONY: sqlc
sqlc: ## Generate the database layer from SQL
	sqlc generate

.PHONY: sqlc-vet
sqlc-vet: ## Prepare every query against a live database
	sqlc vet

.PHONY: openapi
openapi: ## Generate server and client code from the OpenAPI spec
	oapi-codegen -config api/oapi-server.yaml api/openapi.yaml
	oapi-codegen -config api/oapi-client.yaml api/openapi.yaml

## ---- database -------------------------------------------------------------

.PHONY: migrate-up
migrate-up: ## Apply all migrations
	LINKCTRL_DATABASE_URL="$(DEV_DATABASE_URL)" go run ./cmd/lctl migrate up

.PHONY: migrate-down
migrate-down: ## Roll back one migration
	LINKCTRL_DATABASE_URL="$(DEV_DATABASE_URL)" go run ./cmd/lctl migrate down

.PHONY: db-reset
db-reset: ## Drop and recreate the dev database, then migrate
	docker compose exec -T postgres psql -U linkctrl -d postgres \
		-c "DROP DATABASE IF EXISTS linkctrl WITH (FORCE);" -c "CREATE DATABASE linkctrl;"
	$(MAKE) migrate-up

.PHONY: seed
seed: ## Seed development data
	LINKCTRL_DATABASE_URL="$(DEV_DATABASE_URL)" go run ./cmd/lctl seed --links 1000 --clicks 20000

## ---- frontend -------------------------------------------------------------

TAILWIND_VERSION := v4.1.14

.PHONY: tailwind
tailwind: ## Download the pinned Tailwind standalone CLI
	@scripts/get-tailwind.sh "$(TAILWIND_VERSION)" "$(BIN)"

.PHONY: css
css: tailwind ## Build the stylesheet
	$(BIN)/tailwindcss -i internal/ui/static/css/input.css -o internal/ui/static/css/app.css --minify

.PHONY: css-watch
css-watch: tailwind ## Rebuild the stylesheet on change
	$(BIN)/tailwindcss -i internal/ui/static/css/input.css -o internal/ui/static/css/app.css --watch

## ---- containers -----------------------------------------------------------

.PHONY: up
up: ## Start the full stack
	docker compose up -d --wait

.PHONY: down
down: ## Stop the stack and remove volumes
	docker compose down -v

.PHONY: logs
logs: ## Follow application logs
	docker compose logs -f app

.PHONY: docker-build
docker-build: ## Build the production image
	docker build -t linkctrl:dev --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) .

## ---- load -----------------------------------------------------------------

.PHONY: load
load: ## Run the redirect load test against a running stack
	docker run --rm -i --network host -v "$(PWD)/test/load:/scripts" \
		grafana/k6 run -e BASE=http://localhost:8080 /scripts/redirect.js
