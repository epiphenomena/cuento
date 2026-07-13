# cuento — see AGENTS.md for the working method and PLAN.md for the build order.
# Targets exist from p00.1; several are stubs until their phase lands.

GO      ?= go
BINDIR  ?= bin
BINARY  ?= $(BINDIR)/cuento
PKG     := ./...

# Tool binaries are expected on PATH (installed via `go install`); see docs/DECISIONS.md D15.
SQLC        ?= sqlc
GOLANGCILINT?= golangci-lint
GOFUMPT     ?= gofumpt

.PHONY: all gen lint test check golive-check e2e fixture golden run release build clean tools

all: lint test check

## gen — regenerate sqlc query code (no-op until phase 1 adds sqlc.yaml).
gen:
	@if [ -f sqlc.yaml ]; then $(SQLC) generate; else echo "gen: no sqlc.yaml yet (pre-p01.3), skipping"; fi

## lint — govet + golangci-lint + gofumpt formatting check.
lint:
	$(GO) vet $(PKG)
	@if command -v $(GOLANGCILINT) >/dev/null 2>&1; then $(GOLANGCILINT) run; else echo "lint: golangci-lint not installed"; exit 1; fi
	@if command -v $(GOFUMPT) >/dev/null 2>&1; then \
		out=$$($(GOFUMPT) -l .); \
		if [ -n "$$out" ]; then echo "gofumpt: files need formatting:"; echo "$$out"; exit 1; fi; \
	else echo "lint: gofumpt not installed"; exit 1; fi

## test — Go tests plus JS unit tests (node --test) when present. The JS units are
## hand-written ES modules for the boring frontend (AGENTS rule 12). node --test is
## passed the *.test.js files explicitly (a glob, not the directory): recent Node
## treats a bare directory arg as a module to run, so the files are globbed so the
## suite is discovered on every Node version.
test:
	$(GO) test $(PKG)
	@if ls internal/web/static/*.test.js >/dev/null 2>&1; then \
		node --test 'internal/web/static/*.test.js' 'internal/web/static/**/*.test.js'; \
		else echo "test: no JS tests yet, skipping node --test"; fi

## check — build, then run the ledger integrity suite (`cuento check`) against a
## FRESH temp migrated db, which MUST be clean (an empty migrated db has only the
## seeded roots and no splits). Hermetic: the temp db is created and removed here,
## and it NEVER touches fixtures/sample.db (p09.3, gitignored) — routine `make
## check` must not depend on, or fail because of, a local best-guess sample.db
## (which by design carries expected Z19 warnings pending the p09.4 human review).
## The strict go-live gate is a separate manual target, `make golive-check`.
## migrate + check share cwd and -db so db.Open resolves them to one file.
check: build
	@tmpdb=$$(mktemp -u -t cuento-check-XXXXXX.db); \
	trap 'rm -f "$$tmpdb" "$$tmpdb"-* "$$tmpdb".*' EXIT; \
	echo "check: fresh migrated db -> cuento check (must be clean)"; \
	$(BINARY) migrate -db "$$tmpdb" >/dev/null && $(BINARY) check -db "$$tmpdb"

## golive-check — the D26 strict go-live gate (manual, local): run `cuento check
## --strict` on a built db (default fixtures/sample.db). Fails on ANY warning
## (e.g. unmapped-990 Z19) — this is intentional, it is the human-review gate
## before cutover, NOT part of routine `make check`. See docs/golive.md.
golive-check: build
	$(BINARY) check -db $(or $(DB),fixtures/sample.db) --strict

## e2e — opt-in Playwright functional tests (pE.1). Builds bin/cuento, installs
## the pinned test-only Node deps (@playwright/test, bundled chromium already
## cached), and runs the suite in e2e/ against the REAL `cuento serve -dev`. NOT
## part of `make test` (which stays hermetic — no browser, no network per AGENTS):
## e2e needs a browser and is run explicitly. Playwright is a dev/test dependency
## only, never a Go dep and never shipped (AGENTS rule 12, DECISIONS "Functional
## testing"). `npm ci` requires e2e/package-lock.json (committed).
e2e: build
	cd e2e && npm ci && npx playwright test

## fixture — local only: run ledgerimport build to produce fixtures/sample.db
## (p09.3). Reads the gitignored real export + reviewed mapping that live under
## fixtures/source/ (AGENTS rule 11: never committed, never run in CI). The
## mapping is CSV + JSON (no YAML, D15). This target is NEVER part of `make test`
## or CI; it is the p09.4 human-run go-live rehearsal step (D26). sample.db is
## gitignored. Override the paths to point at your local mapping files.
LEDGERIMPORT   ?= $(BINDIR)/ledgerimport
FIXTURE_SOURCE ?= fixtures/source/jrnl.csv
FIXTURE_MAP    ?= fixtures/source/mapping-accounts.csv
FIXTURE_CONFIG ?= fixtures/source/mapping.json
FIXTURE_RATES  ?= fixtures/source/rates.csv
FIXTURE_DB     ?= fixtures/sample.db
fixture:
	@mkdir -p $(BINDIR)
	$(GO) build -o $(LEDGERIMPORT) ./cmd/ledgerimport
	rm -f $(FIXTURE_DB) $(FIXTURE_DB)-* $(FIXTURE_DB).*
	$(LEDGERIMPORT) build -source $(FIXTURE_SOURCE) -map $(FIXTURE_MAP) \
		-config $(FIXTURE_CONFIG) $(if $(wildcard $(FIXTURE_RATES)),-rates $(FIXTURE_RATES),) \
		-o $(FIXTURE_DB) --anonymize

## golden — regenerate report goldens (internal/reports/testdata/*.{txt,csv}) via the
## -update test flag; deterministic (params/currency/locale pinned in the tests). The
## resulting diff MUST be reviewed, never blind-committed (phase 15).
golden:
	$(GO) test ./internal/reports/ -run Golden -update

## run — dev server in -dev mode (phase 0 hello server onward).
run: build
	$(BINARY) serve -dev

## release — CGO-free linux/amd64 static binary, trimpath, version ldflags (phase 18).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
release:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/cuento

build:
	@mkdir -p $(BINDIR)
	$(GO) build -o $(BINARY) ./cmd/cuento

clean:
	rm -rf $(BINDIR)
