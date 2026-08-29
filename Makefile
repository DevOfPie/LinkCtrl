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
test: addon-fixtures ## Unit tests with the race detector
	go test -race -covermode=atomic -coverprofile=cover.out -timeout 30m ./...

# LINKCTRL_REDIS_URL matters as much as the DSN: without it the Redis tier tests
# fall back to localhost:6379, find nothing there and *skip*. The suite stays
# green while three tests that never ran claim to cover the cache.
#
# cmd/lctl carries one integration test of its own: the demo coverage list
# (M33.5), which has to seed a database to know whether the demo shows anything.
# It cannot live in test/integration, because that package cannot import package
# main.
.PHONY: test-integration
test-integration: require-db-password guard-test-integration require-stack oidc-fixture idp-up ## Integration tests (needs Postgres, Redis and the test IdP)
	@TEST_DATABASE_URL="$(DEV_DATABASE_URL)" LINKCTRL_REDIS_URL="$(DEV_REDIS_URL)" \
		go test -race -tags=integration -timeout 30m ./test/integration/... ./cmd/lctl/... ./cmd/linkctrl/...

.PHONY: cover
cover: test ## Open the coverage report
	go tool cover -html=cover.out -o coverage.html

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: check-links
check-links: ## Verify tracked markdown: links and anchors resolve, rows match their headers, row-table links land on their row
	@scripts/check-links.sh

# Deliberately not in `check` below. Every other gate there answers a question
# about this working tree — including `css`, which reaches the network only to
# fetch the pinned Tailwind CLI it does not already have, and `verify-assets`,
# which under VERIFY_ONLY refuses rather than downloads. This one asks GitHub
# about a push that already happened, so it belongs where the answer can change
# a decision —
# release-check, and the phase loop's land sequence — rather than in front of
# every local test run. Exit 2 is "could not ask" and is not a red build. See
# F255: nine days of red CI that no gate anywhere was looking at.
.PHONY: check-ci
check-ci: ## Is the branch's latest CI run green? (asks GitHub; needs gh)
	@scripts/check-ci.sh

# A gate, after a phase of being a tool someone remembered to run by hand. This
# repository's own decision log records how that ends: the link gate was listed
# for a whole phase, unenforced, and did not work when finally built.
#
# The runner ships shellcheck, so this costs an apt-get nothing in CI. Unpinned,
# unlike golangci-lint — and the reason once given for that, *shellcheck's output
# is stable across minor versions*, is **false and cost this repository nine days
# of red CI**. It renamed a diagnostic: an uninvoked function is SC2329 in 0.11
# and its body is unreachable, SC2317, in the release the runner ships. A
# suppression written against one version passed here and failed there from
# 2026-08-09 until M58's close, and nobody looked, because no gate in
# docs/build-notes/workflow.md asks whether CI is green. What survives of the old
# argument is the second half: the surface is scripts only, so the fix is a
# two-line edit rather than a red build across the repository — which is why this
# stays unpinned rather than becoming a third pinned tool. Both codes are named
# at every suppression site. See F255.
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

.PHONY: vet
vet: ## Run go vet
	go vet ./...

# `tidy` repairs; this one reports. A gate that fixes what it finds passes on a
# tree nobody has looked at, which is the same distinction verify-assets draws
# against `make htmx swagger-ui`.
.PHONY: check-tidy
check-tidy: ## Fail if go.mod or go.sum are not tidy
	go mod tidy
	git diff --exit-code -- go.mod go.sum

# Must match the version stamped into internal/store/dbgen, which sqlc writes
# into every file it emits. A mismatch fails on the comment alone, with the rest
# of the diff empty — which is what it did the first time this check ever ran,
# having been pinned a minor version behind the committed output since it was
# written. Bumping it here is the whole change; nothing else names the version.
SQLC_VERSION := v1.31.1

# `go install` puts sqlc in $(go env GOPATH)/bin. That is on PATH on a GitHub
# runner; on a workstation it may not be, which is the one way this target fails
# for a reason that has nothing to do with the diff it is checking.
.PHONY: check-generate
check-generate: ## Fail if committed generated code does not match its source
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
	sqlc generate
	git diff --exit-code -- internal/store/dbgen
	@$(MAKE) --no-print-directory abi-sdk
	git diff --exit-code -- sdk docs/addon-abi.md

.PHONY: check-version-stamp
check-version-stamp: build ## Fail if a built binary does not report its version stamp
	@scripts/check-version-stamp.sh $(BIN)/linkctrl $(BIN)/lctl

# verify-assets and css lead, because without them this target measures the
# machine rather than the code (F350, M69.5). `app.css` is gitignored and built
# by `make css`, so a test that reads a built asset runs here against whatever
# happens to be on disk and in CI against a stylesheet built moments earlier.
# That is how F349 shipped: worker, reviewer and orchestrator each ran this
# target and each got green on a four-day-old stylesheet while CI was red on the
# same commit. The CI build job's own order is `verify-assets css build ...`, and
# this is that order.
#
# verify-assets rather than the `assets` target, which the comment above that
# target already explains: `htmx` and `swagger-ui` repair a stale copy, which is
# right for a developer and wrong for a gate, because a gate that fixes what it
# finds reports success on a tampered blob.
.PHONY: check
check: verify-assets css tidy lint shellcheck check-links test ## Everything CI runs, short of integration tests

# Deliberately NOT a prerequisite of `check` above, of any ci- target, or of
# release-check — and this comment sits here because directly above is where
# somebody would add it.
#
# M26.5 positions the header's two popover panels against the viewport, because
# a top-layer element ignores the header's containing block, and says the result
# is "verified in a browser at each engine, not assumed from the markup". This is
# that verification, kept rather than thrown away with the session that first
# ran it (W14). It reads the class strings out of the templates and the built
# stylesheet, so it needs no instance running and never touches :8081 or :8080.
#
# Node is required — here and in tools/agent-browser's targets below, and by
# nothing the product ships. D25 is what permits it: shipped code stays
# stdlib-only, tooling that only verifies it may use Node. tools/render-verify
# is not imported by anything, not built into anything, and not in the image.
#
# The npm install is done for you; the three browser engines are not. That is
# several hundred megabytes, and a target that quietly spends it is a target
# nobody runs twice — so the harness names the command instead.
RENDER_VERIFY := tools/render-verify

.PHONY: verify-render
verify-render: ## Re-verify M26.5's popover geometry in Blink, Gecko and WebKit (needs Node)
	@command -v node >/dev/null 2>&1 || { \
		echo "node is not on PATH, and only this file's browser-tooling targets need it."; \
		echo "See $(RENDER_VERIFY)/README.md, and Plan.md D25 for why it is allowed to."; \
		exit 1; \
	}
	@test -d $(RENDER_VERIFY)/node_modules || npm install --prefix $(RENDER_VERIFY)
	@node $(RENDER_VERIFY)/verify.mjs $(RENDER_ARGS)

# M46.5 keeps two more things under the same D25 licence: the browser an agent
# drives — @playwright/cli, pinned in tools/agent-browser the way render-verify
# pins its own stack — and the kept spec, which asserts what no template scan
# can: a clean console on a real page served by the running test instance.
# The CLI is wrapped because it defaults to branded Chrome, which is not on
# this machine and not among --browser's values; cli-config.json names the
# bundled chromium instead, and the session it opens persists, so later
# playwright-cli commands take no flag. Same refusal as verify-render: the npm
# install is done for you, browser engines are never downloaded silently — a
# missing engine fails naming the install command.
AGENT_BROWSER := tools/agent-browser
PLAYWRIGHT_CLI := $(AGENT_BROWSER)/node_modules/.bin/playwright-cli

.PHONY: browse
browse: ## Open the pinned browser CLI on the test instance; ARGS="..." runs any playwright-cli command instead
	@command -v node >/dev/null 2>&1 || { \
		echo "node is not on PATH. See $(AGENT_BROWSER)/README.md, and Plan.md D25 for why it is allowed to be needed."; \
		exit 1; \
	}
	@test -d $(AGENT_BROWSER)/node_modules || npm install --prefix $(AGENT_BROWSER)
	@if [ -n "$(ARGS)" ]; then \
		$(PLAYWRIGHT_CLI) $(ARGS); \
	else \
		$(PLAYWRIGHT_CLI) open --config=$(AGENT_BROWSER)/cli-config.json http://127.0.0.1:8081/login; \
	fi

# **`make up` does not rebuild the app image.** A browser check driving a change
# you have not rebuilt for runs against the previous image and passes for the
# wrong reason, which is the same trap workflow.md names before a phase PR and is
# easier to fall into here, mid-milestone, where nobody is thinking about images.
# Rebuild first:
#   docker compose -p linkctrl-test --env-file .env.test up -d --build --wait app
# The `--env-file` is not optional — without it compose resolves a different
# project's variables, the app service does not come up, and this target then
# reports the instance is not answering rather than anything about the image.
# `make rebuild` instead if the schema moved too. This target deliberately does not do it
# for you: a rebuild is a minute and this is meant to be runnable in a loop.
.PHONY: verify-ui
verify-ui: ## Run the kept browser spec against the running test instance (needs Node, make up, and a rebuilt image)
	@command -v node >/dev/null 2>&1 || { \
		echo "node is not on PATH. See $(AGENT_BROWSER)/README.md, and Plan.md D25 for why it is allowed to be needed."; \
		exit 1; \
	}
	@test -d $(AGENT_BROWSER)/node_modules || npm install --prefix $(AGENT_BROWSER)
	@curl -sf -o /dev/null http://127.0.0.1:8081/login || { \
		echo "the test instance is not answering on :8081 — make up"; \
		exit 1; \
	}
	@cd $(AGENT_BROWSER) && ./node_modules/.bin/playwright test --reporter=json | node report-failures.mjs

# M50.6's reopening grows the logo box until a code stops decoding, and says the
# size is "measured by simulated scanning ... the check is kept, not run once —
# it gates the fraction". This is the gate. Two halves: internal/qr renders the
# corpus off the shipping path, and tools/qr-scan decodes it at several
# pixels-per-module, because Go has no QR decoder and D25 puts one in tooling
# rather than in the require block — which is also what keeps M49's
# no-new-dependency assertion true.
#
# Not a prerequisite of `check` or of any ci- target, for verify-render's reason:
# it needs Node, and the shipped build does not.
QR_SCAN := tools/qr-scan

.PHONY: verify-scan
verify-scan: ## Decode every logo'd code the product can draw, at simulated distance (needs Node)
	@command -v node >/dev/null 2>&1 || { \
		echo "node is not on PATH. See $(QR_SCAN)/README.md, and Plan.md D25 for why it is allowed to be needed."; \
		exit 1; \
	}
	@test -d $(QR_SCAN)/node_modules || npm install --prefix $(QR_SCAN)
	@dir=$$(mktemp -d -t linkctrl-qr-scan-XXXXXX); \
	trap 'rm -rf "$$dir"' EXIT; \
	QR_SCAN_CORPUS_DIR="$$dir" go test ./internal/qr -run TestWriteScanCorpus -count=1 >/dev/null && \
	node $(QR_SCAN)/scan.mjs --corpus "$$dir" $(SCAN_ARGS)

## ---- ci -------------------------------------------------------------------

# One target per CI job, and every step of every job is a target rather than a
# `run:` block in YAML. This is not tidiness: the agent that maintains this
# repository holds a token without the `Workflows` permission and cannot write
# `.github/workflows/` at all, so a check added here reaches CI on the next push
# while a check added as a workflow step reaches CI only after the owner applies
# a proposal by hand. ci/proposed/README.md is the contract and says what still
# has to go through that route — triggers, permissions, service images, actions.
#
# These are not `make check`, and the difference is deliberate: CI runs uncached,
# against services the runner provides, and verifies the vendored assets instead
# of repairing them. `make check` is the local gate and is unchanged.
#
# The cost of the aggregates, stated: GitHub shows one step per `run:`, so
# folding a job into one target costs the per-check timing and the green tick
# beside each name in the run summary. Make prints the recipe it is running, so a
# failure still names itself in the log — that is the trade, and it is what buys
# a CI check that can be added without a workflow edit.

# `-timeout 30m` is not slack for a slow machine, it is the line between slow and
# stuck. Go's default is 10 minutes **per package**, and `internal/addon` takes
# 429s of it on this VM under `-race` because every test builds and instantiates
# wasm fixtures. A runner is slower than this VM, so the default started killing
# that package mid-suite — first on a docs-only commit, which is how it announced
# that the margin had gone rather than that anything was wrong. 30m is what a
# genuinely hung test costs before the job gives up, and no passing package is
# within a factor of four of it.
.PHONY: ci-test
ci-test: addon-fixtures ## Unit tests with the race detector, uncached — what CI runs
	go test -race -count=1 -timeout 30m ./...

.PHONY: ci-build
ci-build: verify-assets css build check-version-stamp vet ci-test openapi check-tidy check-generate check-links ## The CI build job, end to end
	@echo "ci-build: every check passed"

# golangci-lint is not here. It runs from a commit-pinned action in the workflow,
# and pinning is the point of that action — reproducing it with a `go install`
# would be a second version of the same pin, free to drift from the first.
.PHONY: ci-lint
ci-lint: shellcheck htmx swagger-ui css ## The CI lint job's steps, short of golangci-lint
	@echo "ci-lint: shell clean, assets built; golangci-lint runs from the workflow's pinned action"

# Against whatever Postgres and Redis the caller provides, not the compose
# instance: on a runner these are service containers, and require-stack would try
# to start a stack that is already there under different ports.
#
# Both variables are required rather than defaulted, and LINKCTRL_REDIS_URL is
# the one that matters most: without it the Redis tier tests fall back to
# localhost:6379, find nothing, and *skip*. The suite stays green while three
# tests that never ran claim to cover the cache — so an unset variable fails here
# instead of passing quietly.
.PHONY: ci-integration
ci-integration: addon-fixtures oidc-fixture idp-up ## Integration tests against caller-provided services — what CI runs
	@test -n "$(TEST_DATABASE_URL)" || { \
		echo "TEST_DATABASE_URL is not set — refusing to run against an unknown database."; \
		exit 1; \
	}
	@test -n "$(LINKCTRL_REDIS_URL)" || { \
		echo "LINKCTRL_REDIS_URL is not set — the Redis tier tests would skip and the run would be green for it."; \
		exit 1; \
	}
	@scripts/ci-db-timezone.sh "$(TEST_DATABASE_URL)"
	@# The same three packages `test-integration` runs, and that is the point
	@# (F144). This ran `./test/integration/` alone, so two integration tests
	@# never ran on a runner at all: the demo-coverage list in `cmd/lctl`, which
	@# fails when a milestone adds a visible feature and seeds nothing, and the
	@# scheduler's own tests in `cmd/linkctrl`, added at F73 for a job whose
	@# whole claim is that it runs without leadership. Neither can live in
	@# `test/integration`, because that package cannot import package main —
	@# which is exactly why they were easy to leave out of CI and hard to notice.
	go test -tags=integration -race -count=1 -timeout 30m ./test/integration/... ./cmd/lctl/... ./cmd/linkctrl/...

IMAGE         ?= linkctrl:ci
IMAGE_VERSION ?= ci

.PHONY: ci-image-smoke
ci-image-smoke: single-instance ## Check a built image reports its version and serves everything on Postgres alone
	@scripts/ci-image-smoke.sh "$(IMAGE)" "$(IMAGE_VERSION)"

# The single-instance guarantee, as a gate rather than an intention (M57).
#
# It rides `ci-image-smoke` because that is the one CI job holding a Docker
# daemon and no service containers, which is exactly what a one-container
# conformance run needs — and because adding a *step* to the workflow needs the
# owner while adding a check to a target reaches CI on the next push. The
# comment at the head of this section is where that bargain is argued; this is
# the second thing to take it.
#
# Prerequisite rather than a second recipe line, so a run that never reaches the
# version check still runs this one: the version stamp is the cheaper failure and
# the conformance is the one somebody's milestone will break.
#
# `addon-fixtures` is built here and the failure is tolerated, which is deliberate
# and is what closed F262. This target is reached by `ci-image-smoke`, and that is
# the one CI job with a Docker daemon and no actions/setup-go step, so making the
# whole one-container conformance run depend on the runner image happening to ship
# a Go toolchain traded a real gate against an unpinned property of somebody
# else's VM. Without a fixture the script skips its add-on limb and says so; with
# one — every developer machine, and every runner that does ship Go — all three
# limbs run exactly as before. `make addon-fixtures` on its own still fails loudly,
# which is where somebody debugging a fixture should be looking.
.PHONY: single-instance
single-instance: ## One container, no Redis, no load balancer — the whole surface
	-@$(MAKE) --no-print-directory addon-fixtures
	@scripts/single-instance-check.sh "$(IMAGE)" "$(ADDON_FIXTURE_DIR)/minimal.wasm"

.PHONY: workflow-proposals
workflow-proposals: ## Which ci/proposed/ workflow proposals the owner has not applied yet
	@scripts/workflow-proposals.sh

## ---- codegen --------------------------------------------------------------

.PHONY: generate
generate: sqlc openapi abi-sdk ## Regenerate all generated code

# The add-on ABI has one author, internal/addon/abi, and three faces: the SDK an
# add-on imports, the function table in docs/addon-abi.md, and the host module the
# runtime registers. Only the first two are files; the third is derived at runtime
# from the same slice, which is what makes host and guest unable to disagree about
# a signature.
#
# Committed like sqlc's dbgen and the world-map paths, and held to the same
# property: re-running it on an unchanged tree produces no diff, which is what
# `check-generate` asserts. Committed rather than generated at build time because
# the SDK is what another repository imports — an importable package that only
# exists after somebody runs a make target is not one.
.PHONY: abi-sdk
abi-sdk: ## Regenerate the add-on SDK and the ABI's documented function table
	go run ./internal/addon/abi/gen

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

# The WASM modules the add-on host is tested against, and the one the
# single-instance gate installs into a container.
#
# Built rather than committed. m60.md refuses a checked-in binary by name, and the
# reason is the one every vendored asset in the frontend section is checksum-pinned
# for: a blob in the tree is a build input nobody reviews. These are ~1.8 MB each
# because a GOOS=wasip1 module built by the standard toolchain carries the whole Go
# runtime; that is a fixture cost, not a product one, and it is why the fixture
# strategy — not the product — is what would revisit TinyGo if it ever mattered.
#
# -buildmode=c-shared is load-bearing, not a flag: it makes the entry point
# _initialize instead of _start, so the module is a reactor that stays
# instantiated rather than a command that runs main and exits. addon.StartFunction
# is the other half of the pair and the two must agree.
#
# A prerequisite of `test` and `ci-test` rather than something to remember, so the
# build is paid for once, outside the test binary, with make's timestamps deciding.
# It is not the only way the modules get built: internal/addon's fixture() builds
# what it needs when it is missing, because two callers run `go test` without ever
# reaching a make target and neither is this repository's to edit — the release
# workflow, and the CI `image` job (F262). This target is the fast path, not the
# contract.
#
# The set is globbed rather than named. It was named here and named again in two
# other places, and a third module would have been built by one of them and by
# nothing else; a source directory is the one enumeration that cannot disagree
# with itself.
#
# The prerequisites are every .go file in the module's directory **and every .go
# file in the SDK**, which is F266's fix. The rule used to depend on `main.go`
# alone while its recipe compiled the whole package directory, so a fixture that
# grew a second file was built from a file the rule did not watch. That was
# theoretical while each fixture was one file; M61 made it real from the other
# side by rebuilding the fixtures on top of the generated SDK, which is a shared
# input neither fixture's directory knows anything about. The failure it produces
# is a green run against yesterday's bytes, in the one package whose subject is
# verifying that bytes are the bytes a manifest describes.
#
# .SECONDEXPANSION: is what lets a pattern rule take a $(wildcard) over its own
# stem — `$$*` is expanded when the rule is *used* rather than when it is read.
# The Taskfile's mirror expresses the same set with sources:/generates:, and
# internal/addon's own fixture() compares the same mtimes, because two of the three
# callers cannot be reached from a make target at all (F262).
ADDON_FIXTURE_SRC := internal/addon/testdata/modules
ADDON_FIXTURE_DIR := internal/addon/testdata/build
ADDON_FIXTURES    := $(patsubst $(ADDON_FIXTURE_SRC)/%/main.go,$(ADDON_FIXTURE_DIR)/%.wasm,$(wildcard $(ADDON_FIXTURE_SRC)/*/main.go))
ADDON_SDK_SRC     := $(wildcard sdk/*.go)

.PHONY: addon-fixtures
addon-fixtures: $(ADDON_FIXTURES) ## Build the WASM test modules the add-on host is tested against

.SECONDEXPANSION:
$(ADDON_FIXTURE_DIR)/%.wasm: $$(wildcard $(ADDON_FIXTURE_SRC)/$$*/*.go) $(ADDON_SDK_SRC)
	@mkdir -p $(ADDON_FIXTURE_DIR)
	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o $@ ./$(ADDON_FIXTURE_SRC)/$*

# The OIDC add-on, and it is not one of the fixtures above: those are this
# repository's own test modules, and this is `DevOfPie/LinkCtrl-OIDC` — a
# different repository, whose released bundle is downloaded, checked against the
# digest that release's SHA256SUMS carries, and admitted only once the source at
# the same tag rebuilds to the module inside it. M69's acceptance test installs
# it, so those checks are the difference between testing the add-on that was
# published and testing whatever built today.
#
# Not a file rule. What decides whether it has to run is the digest of what is
# already there against the pin in the script, which a timestamp cannot express,
# so the script answers that question itself and exits immediately when the
# fixture is current.
.PHONY: oidc-fixture
oidc-fixture: ## Build the released OIDC add-on the integration suite installs
	@scripts/oidc-fixture.sh

# The identity provider the acceptance test signs in through. `up` generates the
# certificate dex serves if it is not there, starts the container and waits for a
# discovery document — not for a started container, which is a different claim.
.PHONY: idp-up
idp-up: ## Start the integration suite's identity provider (dex) and wait for it
	@scripts/idp.sh up

.PHONY: idp-down
idp-down: ## Stop the integration suite's identity provider
	@scripts/idp.sh down

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
#
# **`LINKCTRL_ADDONS_DIR` is emptied for the same reason the DSN is overridden.**
# The instance file names a path *inside the container* — `/addons`, which is
# where the image puts the sample add-on — and `config.Load` refuses a directory
# it cannot stat, so sourcing the file unedited makes every one of these targets
# exit on a configuration error rather than run. lctl has no add-on host and wants
# none: it migrates, seeds and mints keys, and not one of those reads a module.
DEV_ENV = set -a; . "./$(ENV_FILE)"; set +a; \
	LINKCTRL_APP_ENV=development LINKCTRL_ADDONS_DIR= \
	LINKCTRL_DATABASE_URL="$(DEV_DATABASE_URL)"

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

# The world map (M37, D63). world-atlas is pre-built TopoJSON of Natural Earth:
# the data is public domain and asks for no attribution, the packaging is ISC.
#
# Unlike the three above, this file is never served. It is a build-time input to
# `make mapgen`, which converts it to Go source once; the binary embeds the
# generated paths and nothing parses TopoJSON at request time.
WORLDMAP_VERSION := 2.0.2
WORLDMAP_SHA256  := 2516c915867c7baf18ddec727aec46c315541a07cfb3d79a6559b05d5e94eee8
WORLDMAP_FILE    := assets/world-atlas/countries-110m.json

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
.PHONY: worldmap
worldmap: ## Verify (or restore) the vendored world map against its pinned checksum
	@scripts/get-worldmap.sh "$(WORLDMAP_VERSION)" "$(WORLDMAP_SHA256)" "$(WORLDMAP_FILE)"

# Generated output, committed like sqlc's dbgen. Re-running it on an unchanged
# tree must produce no diff, which is the same property `make sqlc` is held to.
.PHONY: mapgen
mapgen: worldmap ## Regenerate the world-map SVG paths from the vendored TopoJSON
	@go run ./internal/ui/geo/mapgen "$(WORLDMAP_FILE)" internal/ui/geo/countries_gen.go

.PHONY: verify-assets
verify-assets: ## Fail if a vendored asset does not match its pinned checksum
	@VERIFY_ONLY=1 scripts/get-htmx.sh "$(HTMX_VERSION)" "$(HTMX_SHA256)" internal/ui/static/js/htmx.min.js
	@VERIFY_ONLY=1 scripts/get-swagger.sh "$(SWAGGER_UI_VERSION)" "$(SWAGGER_CSS_SHA256)" "$(SWAGGER_JS_SHA256)" internal/ui/static/vendor
	@VERIFY_ONLY=1 scripts/get-worldmap.sh "$(WORLDMAP_VERSION)" "$(WORLDMAP_SHA256)" "$(WORLDMAP_FILE)"

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

.PHONY: load-breaking-point
load-breaking-point: require-stack ## Grow the dataset until an SLO check fails
	@scripts/slo-breaking-point.sh $(START_LINKS) $(MULTIPLIER) $(MAX_STEPS)

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
#
# **The app is recreated twice, and the second one is the load-bearing one.**
# The first brings up the new image so the reseed runs against this milestone's
# schema. The reseed then deletes and rewrites the `domains` rows — new ids for
# `go.linkctrl.example` — while the app has held its verified-hostname set in
# memory since boot, from before those rows existed. Nothing else puts the new
# set in front of it: `lctl` runs on the host with no Redis, so it publishes no
# invalidation; `internal/redirect/hosts.go` holds the whole set precisely so a
# miss costs no query, so there is no lazy reload to fall back on; and the demo
# runs `DOMAIN_VERIFY_INTERVAL=0`, which stops the periodic reload with the
# verification pass. Without the second recreate a fresh demo serves nothing on
# its custom hostname and a repeat serves a cached entry pointing at a deleted
# domain id.
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
	docker compose -p linkctrl-demo --env-file .env.demo up -d --force-recreate --wait app
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
release-check: require-db-password ## Everything that must hold before tagging a release
	@# The DSNs are exported here rather than left to the caller's environment.
	@# release-check.sh decides to run the integration tests by asking docker
	@# compose whether Postgres is up, and then ran `go test` with no
	@# TEST_DATABASE_URL — so on a machine with the stack running, the last gate
	@# before a tag failed for want of a connection string it could have built
	@# itself. Same two variables `test-integration` above passes, from the same
	@# variables, so the two cannot disagree about which instance they mean.
	@TEST_DATABASE_URL="$(DEV_DATABASE_URL)" LINKCTRL_REDIS_URL="$(DEV_REDIS_URL)" \
		scripts/release-check.sh

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
