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

.PHONY: check-links
check-links: ## Verify every relative link and anchor in tracked markdown
	@scripts/check-links.sh

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

# No code is generated from the spec in either direction. The API was built
# service-first with handlers as thin skins, so a generated server interface
# would rewrite working code for zero behavioural change; the contract is
# enforced by tests instead — a router↔spec parity check here, and a full
# request/response replay in the integration suite.
.PHONY: openapi
openapi: ## Validate the OpenAPI document against the implementation
	go test ./internal/httpx/ -run TestOpenAPI -count=1

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

.PHONY: demo
demo: require-db-password ## Fill the dev instance with demo data worth looking at
	@$(DEV_ENV) go run ./cmd/lctl demo --reset

## ---- frontend -------------------------------------------------------------

TAILWIND_VERSION := v4.1.14

# htmx is vendored at internal/ui/static/js/htmx.min.js so a fresh clone builds
# offline. This pin is what makes the committed blob verifiable: `make htmx`
# checks it against the upstream release checksum and re-fetches on mismatch.
HTMX_VERSION := v2.0.9
HTMX_SHA256  := 57d9191515339922bd1356d7b2d80b1ee3b29f1b3a2c65a078bb8b2e8fd9ae5f

# Swagger UI, vendored the same way for /docs.
SWAGGER_UI_VERSION := v5.32.11
SWAGGER_CSS_SHA256 := ca238f7d7c2cf4480c1e77a9c3b9da915ab216e96ffd354e69076560c650c6de
SWAGGER_JS_SHA256  := fcb81e2c79e7e3b76ddb9bd7fc791552045040fde05c19d3f98f9213e7f7724d

.PHONY: tailwind
tailwind: ## Download the pinned Tailwind standalone CLI
	@scripts/get-tailwind.sh "$(TAILWIND_VERSION)" "$(BIN)"

.PHONY: htmx
htmx: ## Verify (or restore) the vendored htmx against its pinned checksum
	@scripts/get-htmx.sh "$(HTMX_VERSION)" "$(HTMX_SHA256)" internal/ui/static/js/htmx.min.js

.PHONY: swagger-ui
swagger-ui: ## Verify (or restore) the vendored Swagger UI against its pinned checksums
	@scripts/get-swagger.sh "$(SWAGGER_UI_VERSION)" "$(SWAGGER_CSS_SHA256)" "$(SWAGGER_JS_SHA256)" internal/ui/static/vendor

# What CI runs instead of `make htmx swagger-ui`. Those targets repair a stale
# copy, which is right for a developer and wrong for a gate: a gate that fixes
# what it finds reports success on a tampered blob. VERIFY_ONLY makes the
# mismatch fatal instead.
.PHONY: verify-assets
verify-assets: ## Fail if a vendored asset does not match its pinned checksum
	@VERIFY_ONLY=1 scripts/get-htmx.sh "$(HTMX_VERSION)" "$(HTMX_SHA256)" internal/ui/static/js/htmx.min.js
	@VERIFY_ONLY=1 scripts/get-swagger.sh "$(SWAGGER_UI_VERSION)" "$(SWAGGER_CSS_SHA256)" "$(SWAGGER_JS_SHA256)" internal/ui/static/vendor

.PHONY: css
css: tailwind ## Build the stylesheet
	$(BIN)/tailwindcss -i internal/ui/static/css/input.css -o internal/ui/static/css/app.css --minify

.PHONY: css-watch
css-watch: tailwind ## Rebuild the stylesheet on change
	$(BIN)/tailwindcss -i internal/ui/static/css/input.css -o internal/ui/static/css/app.css --watch

.PHONY: assets
assets: htmx swagger-ui css ## Everything `go build` embeds

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
	docker build -t linkctrl:$(VERSION) -t linkctrl:dev \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) .

## ---- release --------------------------------------------------------------

# The platforms a release publishes. linux/amd64 and linux/arm64 are the
# deployment targets; the darwin and windows builds exist so an operator can run
# lctl against a remote database from their own machine.
RELEASE_PLATFORMS := linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64
DIST := dist

# One archive format for every platform, tar.gz included for Windows. Windows 10
# and later ship tar, so the format costs a Windows user nothing — while shipping
# .zip would mean either a zip tool on every build host or an artifact whose format
# depends on where it was built, and neither is worth it.

.PHONY: dist
dist: assets ## Cross-compile release binaries with checksums into dist/
	@rm -rf $(DIST) && mkdir -p $(DIST)
	@for platform in $(RELEASE_PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; ext=""; \
		[ "$$os" = windows ] && ext=".exe"; \
		echo "  $$os/$$arch"; \
		name="linkctrl_$(VERSION)_$${os}_$${arch}"; \
		mkdir -p "$(DIST)/$$name"; \
		for cmd in linkctrl lctl; do \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
				go build -trimpath -ldflags "$(LDFLAGS)" \
				-o "$(DIST)/$$name/$$cmd$$ext" ./cmd/$$cmd || exit 1; \
		done; \
		cp LICENSE README.md CHANGELOG.md .env.example "$(DIST)/$$name/"; \
		( cd $(DIST) && tar czf "$$name.tar.gz" "$$name" ) || exit 1; \
		rm -rf "$(DIST)/$$name"; \
	done
	@cd $(DIST) && sha256sum *.tar.gz > SHA256SUMS && cat SHA256SUMS

.PHONY: release-check
release-check: ## Everything that must hold before tagging a release
	@scripts/release-check.sh

## ---- load -----------------------------------------------------------------

.PHONY: seed-slo
seed-slo: require-db-password ## Seed the dataset the SLO is defined against (100k links, 5M clicks)
	@$(DEV_ENV) go run ./cmd/lctl seed --reset --links 100000 --clicks 5000000

.PHONY: load
load: ## Measure the cached redirect SLO against a running stack
	./scripts/load-test.sh cached 2000 2m

.PHONY: load-uncached
load-uncached: ## Same, spread across the whole dataset so the cache cannot answer
	./scripts/load-test.sh uncached 500 1m
