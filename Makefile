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

# Which development instance the targets below act on: `test` (disposable, the
# default) or `demo` (long-lived, refreshed only by `make demo-update`). They are
# separate compose projects with separate volumes and ports, so nothing done to
# one reaches the other. See docs/dev-notes/instances.md.
#
# The default is deliberately the disposable one. Every destructive target here
# is one typo away from another, and the version of this that defaulted to
# whatever stack happened to be running is how a demo worth showing somebody gets
# dropped by a `make db-reset` meant for a test.
INSTANCE ?= test
ENV_FILE  = .env.$(INSTANCE)
PROJECT   = linkctrl-$(INSTANCE)

# Both the flags and the exported variables. The flags are for the calls in this
# file; the variables are for the scripts it invokes, whose own `docker compose`
# calls would otherwise land on the default project.
COMPOSE = docker compose -p $(PROJECT) --env-file $(ENV_FILE)
export COMPOSE_PROJECT_NAME = $(PROJECT)
export COMPOSE_ENV_FILES    = $(ENV_FILE)

# One value out of the instance's env file. Inline comments are stripped the way
# compose strips them — whitespace, then `#` — so a `#` inside a password is kept
# rather than truncating the secret at a character that is legal in one.
envval = $(shell sed -n 's/^$(1)=//p' $(ENV_FILE) 2>/dev/null \
	| head -1 | tr -d '\r' | sed -e 's/[[:space:]][[:space:]]*#.*$$//' -e 's/[[:space:]]*$$//')

# Read from the env file rather than written here, because that file is where
# compose reads it too and therefore what the database was initialised with. A
# literal is wrong for every instance that generated its own, and the failure it
# produces — "password authentication failed" — reads as a broken database rather
# than a stale variable. An exported POSTGRES_PASSWORD still wins, for CI.
#
# A password containing characters that are not URL-safe must be
# percent-encoded, since this is assembled into a DSN. scripts/instance.sh
# generates alphanumeric ones for exactly that reason.
POSTGRES_USER     ?= linkctrl
POSTGRES_DB       ?= linkctrl
POSTGRES_PASSWORD ?= $(call envval,POSTGRES_PASSWORD)
POSTGRES_PORT     ?= $(call envval,POSTGRES_PORT)
REDIS_PORT        ?= $(call envval,REDIS_PORT)

DEV_DATABASE_URL ?= postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/$(POSTGRES_DB)?sslmode=disable
DEV_REDIS_URL    ?= redis://localhost:$(REDIS_PORT)/0

# Guard for the targets that connect. Without it an empty password produces the
# same authentication error as a wrong one, several steps away from the cause.
.PHONY: require-db-password
require-db-password:
	@test -f "$(ENV_FILE)" || { \
		echo "$(ENV_FILE) does not exist."; \
		echo "Create the $(INSTANCE) instance with: make env INSTANCE=$(INSTANCE)"; \
		exit 1; \
	}
	@test -n "$(POSTGRES_PASSWORD)" || { \
		echo "POSTGRES_PASSWORD is empty in $(ENV_FILE)."; \
		echo "Set it there, or export it in the environment."; \
		exit 1; \
	}

# Every target that writes to a database routes through this. On the test
# instance it returns immediately; on demo it refuses without CONFIRM=demo.
.PHONY: guard-%
guard-%:
	@scripts/instance.sh guard "$(INSTANCE)" "$*"

# Postgres and Redis, for the commands that run on this host and talk to them
# directly. Not the app: `make run` needs the port the app container holds, and
# the integration suite builds its own server with httptest rather than calling
# a container.
#
# The test instance is stopped whenever nothing has used it for half an hour
# (scripts/idle-stop.sh), so anything needing a database has to be able to start
# one. Only when it is not already up: `up --wait` against a healthy stack costs
# a second and six lines of output on every target that depends on it.
.PHONY: require-stack
require-stack:
	@$(COMPOSE) ps --status running --services 2>/dev/null | grep -qx postgres \
		|| $(COMPOSE) up -d --wait postgres redis

# The whole stack, for the targets that drive the app over HTTP.
.PHONY: require-app
require-app:
	@$(COMPOSE) ps --status running --services 2>/dev/null | grep -qx app \
		|| $(COMPOSE) up -d --wait

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_.-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

## ---- build ----------------------------------------------------------------

.PHONY: build
build: ## Build the server and the operator CLI
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/ ./cmd/...

# Against the instance's database and Redis, on the instance's port, with the
# instance's configuration — not `.env`, whose DSN and Redis URL name compose
# network hosts that do not resolve here. Stop that instance's app container
# first; both cannot hold the same port.
.PHONY: run
run: require-db-password require-stack ## Run the server from source against the instance's data
	@$(DEV_ENV) LINKCTRL_REDIS_URL="$(DEV_REDIS_URL)" \
		LINKCTRL_HTTP_ADDR=":$(call envval,LINKCTRL_HTTP_PORT)" \
		go run -ldflags "$(LDFLAGS)" ./cmd/linkctrl

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN) dist cover.out coverage.html

## ---- quality --------------------------------------------------------------

.PHONY: test
test: ## Unit tests with the race detector
	go test -race -covermode=atomic -coverprofile=cover.out ./...

# LINKCTRL_REDIS_URL matters as much as the DSN: without it the Redis tier tests
# fall back to localhost:6379, find nothing there and *skip*. The suite stays
# green while three tests that never ran claim to cover the cache.
#
# cmd/lctl carries one integration test of its own: the demo coverage list
# (M33.5), which has to seed a database to know whether the demo shows anything.
# It cannot live in test/integration, because that package cannot import package
# main.
.PHONY: test-integration
test-integration: require-db-password guard-test-integration require-stack ## Integration tests (needs Postgres and Redis)
	@TEST_DATABASE_URL="$(DEV_DATABASE_URL)" LINKCTRL_REDIS_URL="$(DEV_REDIS_URL)" \
		go test -race -tags=integration ./test/integration/... ./cmd/lctl/...

.PHONY: cover
cover: test ## Open the coverage report
	go tool cover -html=cover.out -o coverage.html

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: check-links
check-links: ## Verify every relative link and anchor in tracked markdown
	@scripts/check-links.sh

# A gate, after a phase of being a tool someone remembered to run by hand. This
# repository's own decision log records how that ends: the link gate was listed
# for a whole phase, unenforced, and did not work when finally built.
.PHONY: shellcheck
shellcheck: ## Lint every shell script
	shellcheck scripts/*.sh

# Not a gate. The operating contract is documentation and documentation is read
# into a context window on every task, so its size is a cost that recurs and
# compounds; this measures it before it is large enough to hurt. Regenerating on
# an unchanged tree produces no diff, so every diff in the report is real growth.
.PHONY: doc-cost
doc-cost: ## What the always-read docs cost — predicted vs realized
	@scripts/doc-cost.sh > docs/build-notes/doc-cost.md
	@echo "wrote docs/build-notes/doc-cost.md"

.PHONY: vuln
vuln: ## Check for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: tidy
tidy: ## Tidy and verify modules
	go mod tidy
	go mod verify

.PHONY: check
check: tidy lint shellcheck test ## Everything CI runs, short of integration tests

## ---- codegen --------------------------------------------------------------

.PHONY: generate
generate: sqlc openapi ## Regenerate all generated code

.PHONY: sqlc
sqlc: ## Generate the database layer from SQL
	sqlc generate

.PHONY: sqlc-vet
sqlc-vet: require-db-password require-stack ## Prepare every query against a live database
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

# APP_ENV must be in the environment, not only in a file: config.Load consults it
# before reading one, so without it the file is ignored and every target below
# fails on the three required secrets rather than on anything to do with the
# database. DATABASE_URL is then overridden because the value compose passes the
# container names the compose network host, which does not resolve from here.
#
# The instance file is sourced first, and this is not decoration. lctl runs on
# the host, where config.Load reads `.env` — the operator quickstart file, not
# the instance's. Overriding only the DSN was enough to write to the right
# database while every other value came from the wrong instance: `lctl demo`
# printed the demo instance's URL for data it had just written to the test one,
# and `lctl apikey` would have minted keys under a pepper that instance's server
# does not have, so they would never validate. godotenv does not overwrite a
# variable that is already set, so exporting these first makes them win.
#
# Recipes carrying this are prefixed with @, so the assembled DSN — password
# included — does not end up in terminal scrollback or a CI log.
DEV_ENV = set -a; . "./$(ENV_FILE)"; set +a; \
	LINKCTRL_APP_ENV=development LINKCTRL_DATABASE_URL="$(DEV_DATABASE_URL)"

.PHONY: migrate-up
migrate-up: require-db-password require-stack ## Apply all migrations
	@$(DEV_ENV) go run ./cmd/lctl migrate up

.PHONY: migrate-down
migrate-down: require-db-password guard-migrate-down require-stack ## Roll back one migration
	@$(DEV_ENV) go run ./cmd/lctl migrate down

.PHONY: migrate-status
migrate-status: require-db-password require-stack ## Show applied and pending migrations
	@$(DEV_ENV) go run ./cmd/lctl migrate status

.PHONY: db-reset
db-reset: require-db-password guard-db-reset require-stack ## Drop and recreate the dev database, then migrate
	$(COMPOSE) exec -T postgres psql -U "$(POSTGRES_USER)" -d postgres \
		-c "DROP DATABASE IF EXISTS $(POSTGRES_DB) WITH (FORCE);" \
		-c "CREATE DATABASE $(POSTGRES_DB);"
	$(MAKE) migrate-up

.PHONY: seed
seed: require-db-password guard-seed require-stack ## Seed development data
	@$(DEV_ENV) go run ./cmd/lctl seed --links 1000 --clicks 20000

.PHONY: demo
demo: require-db-password guard-demo require-stack ## Fill the dev instance with demo data worth looking at
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

.PHONY: env
env: ## Create the instance's env file if it does not exist
	@scripts/instance.sh init "$(INSTANCE)"

.PHONY: instances
instances: ## Both development instances and whether they are running
	@scripts/instance.sh list

.PHONY: up
up: ## Start the full stack
	@test -f "$(ENV_FILE)" || scripts/instance.sh init "$(INSTANCE)"
	$(COMPOSE) up -d --wait

.PHONY: down
down: guard-down ## Stop the stack and remove volumes
	$(COMPOSE) down -v

.PHONY: stop
stop: ## Stop the stack, keeping its volumes
	$(COMPOSE) stop

# What the systemd timer runs every five minutes. Here so the decision can be
# inspected without reading the journal — it reports what it sees and why.
.PHONY: idle-stop
idle-stop: ## Stop the test instance if nothing has used it (IDLE_MINUTES=30)
	@scripts/idle-stop.sh test $(or $(IDLE_MINUTES),30)

.PHONY: logs
logs: ## Follow application logs
	$(COMPOSE) logs -f app

# The test instance, from nothing: volumes gone, image rebuilt from the working
# tree, schema migrated. Guarded like any other destructive target, so
# `make rebuild INSTANCE=demo` stops rather than doing what it says.
.PHONY: rebuild
rebuild: guard-rebuild ## Recreate the instance from scratch and migrate
	$(COMPOSE) down -v
	@test -f "$(ENV_FILE)" || scripts/instance.sh init "$(INSTANCE)"
	$(COMPOSE) up -d --build --force-recreate --wait
	$(MAKE) migrate-up INSTANCE=$(INSTANCE)

# The milestone refresh, and the only thing that should write to the demo.
#
# Recursive assignment, not simple: .env.demo may not exist when this file is
# read, and does by the time the recipe below prints the URL.
DEMO_URL = http://localhost$(addprefix :,$(shell sed -n 's/^LINKCTRL_HTTP_PORT=//p' .env.demo 2>/dev/null | head -1 | tr -d '\r'))

# The clean-tree check is the enforcement: the demo is meant to show the last
# validated milestone, and building it from a tree with uncommitted work puts
# something nobody has validated in front of the person judging the milestone.
# Pass FORCE=1 when the point is to look at work in progress.
.PHONY: demo-update
demo-update: ## Rebuild the demo from the current commit and refresh its data
	@test -f .env.demo || scripts/instance.sh init demo
	@if [ -z "$(FORCE)" ] && [ -n "$$(git status --porcelain)" ]; then \
		echo "The working tree is dirty, so this build is not the validated milestone."; \
		echo "Commit first, or pass FORCE=1 to demo the work in progress."; \
		git status --short; \
		exit 1; \
	fi
	docker compose -p linkctrl-demo --env-file .env.demo up -d --build --force-recreate --wait
	@echo
	@$(MAKE) --no-print-directory demo INSTANCE=demo CONFIRM=demo || { \
		echo; \
		echo "The stack is up and migrated, but no demo data was written."; \
		echo "A fresh instance has no user to own it. Claim it at $(DEMO_URL),"; \
		echo "then run make demo-update again."; \
		exit 1; \
	}
	@echo
	@echo "demo updated to $(VERSION) at $(DEMO_URL)"

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
seed-slo: require-db-password guard-seed-slo require-stack ## Seed the dataset the SLO is defined against (100k links, 5M clicks)
	@$(DEV_ENV) go run ./cmd/lctl seed --reset --links 100000 --clicks 5000000

# Guarded even though a load test only reads: it writes several hundred thousand
# click events on the way, and the demo's analytics are meant to look like a
# workspace rather than like a benchmark.
.PHONY: load
load: guard-load require-app ## Measure the cached redirect SLO against a running stack
	./scripts/load-test.sh cached 2000 2m

.PHONY: load-uncached
load-uncached: guard-load-uncached require-app ## Same, spread across the whole dataset so the cache cannot answer
	./scripts/load-test.sh uncached 500 1m
