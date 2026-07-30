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
#
# The password is read from .env rather than written here, because .env is where
# compose reads it too and therefore what the database was initialised with. A
# literal here is wrong for every developer who generated their own, and the
# failure it produces — "password authentication failed" — reads as a broken
# database rather than a stale variable. An exported POSTGRES_PASSWORD wins, so
# CI can supply it without a file.
#
# A password containing characters that are not URL-safe must be
# percent-encoded, since this is assembled into a DSN.
POSTGRES_USER     ?= linkctrl
POSTGRES_DB       ?= linkctrl
POSTGRES_PASSWORD ?= $(shell sed -n 's/^POSTGRES_PASSWORD=//p' .env 2>/dev/null | tr -d '\r')

DEV_DATABASE_URL ?= postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:55432/$(POSTGRES_DB)?sslmode=disable
DEV_REDIS_URL    ?= redis://localhost:56379/0

# Guard for the targets that connect. Without it an empty password produces the
# same authentication error as a wrong one, several steps away from the cause.
.PHONY: require-db-password
require-db-password:
	@test -n "$(POSTGRES_PASSWORD)" || { \
		echo "POSTGRES_PASSWORD is empty."; \
		echo "Set it in .env (cp .env.example .env) or export it in the environment."; \
		exit 1; \
	}

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
test-integration: require-db-password ## Integration tests (needs Postgres and Redis)
	@TEST_DATABASE_URL="$(DEV_DATABASE_URL)" go test -race -tags=integration ./test/integration/...

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
sqlc-vet: require-db-password ## Prepare every query against a live database
	@LINKCTRL_DATABASE_URL="$(DEV_DATABASE_URL)" sqlc vet -f sqlc-vet.yaml

.PHONY: openapi
openapi: ## Generate server and client code from the OpenAPI spec
	oapi-codegen -config api/oapi-server.yaml api/openapi.yaml
	oapi-codegen -config api/oapi-client.yaml api/openapi.yaml

## ---- database -------------------------------------------------------------

# APP_ENV must be in the environment, not only in .env: config.Load consults it
# before reading the file, so without it .env is ignored and every target below
# fails on the three required secrets rather than on anything to do with the
# database. DATABASE_URL is then overridden because the value in .env names the
# compose network host, which does not resolve from the developer's machine.
#
# Recipes carrying this are prefixed with @, so the assembled DSN — password
# included — does not end up in terminal scrollback or a CI log.
DEV_ENV := LINKCTRL_APP_ENV=development LINKCTRL_DATABASE_URL="$(DEV_DATABASE_URL)"

.PHONY: migrate-up
migrate-up: require-db-password ## Apply all migrations
	@$(DEV_ENV) go run ./cmd/lctl migrate up

.PHONY: migrate-down
migrate-down: require-db-password ## Roll back one migration
	@$(DEV_ENV) go run ./cmd/lctl migrate down

.PHONY: migrate-status
migrate-status: require-db-password ## Show applied and pending migrations
	@$(DEV_ENV) go run ./cmd/lctl migrate status

.PHONY: db-reset
db-reset: require-db-password ## Drop and recreate the dev database, then migrate
	docker compose exec -T postgres psql -U "$(POSTGRES_USER)" -d postgres \
		-c "DROP DATABASE IF EXISTS $(POSTGRES_DB) WITH (FORCE);" \
		-c "CREATE DATABASE $(POSTGRES_DB);"
	$(MAKE) migrate-up

.PHONY: seed
seed: require-db-password ## Seed development data
	@$(DEV_ENV) go run ./cmd/lctl seed --links 1000 --clicks 20000

## ---- frontend -------------------------------------------------------------

TAILWIND_VERSION := v4.1.14

# htmx is vendored at internal/ui/static/js/htmx.min.js so a fresh clone builds
# offline. This pin is what makes the committed blob verifiable: `make htmx`
# checks it against the upstream release checksum and re-fetches on mismatch.
HTMX_VERSION := v2.0.9
HTMX_SHA256  := 57d9191515339922bd1356d7b2d80b1ee3b29f1b3a2c65a078bb8b2e8fd9ae5f

.PHONY: tailwind
tailwind: ## Download the pinned Tailwind standalone CLI
	@scripts/get-tailwind.sh "$(TAILWIND_VERSION)" "$(BIN)"

.PHONY: htmx
htmx: ## Verify (or restore) the vendored htmx against its pinned checksum
	@scripts/get-htmx.sh "$(HTMX_VERSION)" "$(HTMX_SHA256)" internal/ui/static/js/htmx.min.js

.PHONY: css
css: tailwind ## Build the stylesheet
	$(BIN)/tailwindcss -i internal/ui/static/css/input.css -o internal/ui/static/css/app.css --minify

.PHONY: css-watch
css-watch: tailwind ## Rebuild the stylesheet on change
	$(BIN)/tailwindcss -i internal/ui/static/css/input.css -o internal/ui/static/css/app.css --watch

.PHONY: assets
assets: htmx css ## Everything `go build` embeds

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
