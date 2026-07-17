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

.PHONY: all gen lint test check golive-check e2e fixture dev-db scaffold-db import-sub golden run release build clean tools

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

## dev-db — local only: rebuild the throwaway dev database bin/dev.db from the same
## gitignored real export + reviewed mapping as `fixture`, but WITHOUT --anonymize
## (real names, for local go-live review — bin/ is gitignored so nothing leaks, rule
## 11) and re-add the dev login. Use this after the importer or mapping changes so the
## running app isn't served a stale db. Stop the :3390 server FIRST (it holds the file
## open); restart it after. DEV_PASS overrides the seeded password.
DEV_DB   ?= $(BINDIR)/dev.db
DEV_USER ?= admin
DEV_PASS ?= devpass123
dev-db:
	@mkdir -p $(BINDIR)
	$(GO) build -o $(LEDGERIMPORT) ./cmd/ledgerimport
	$(GO) build -o $(BINARY) ./cmd/cuento
	rm -f $(DEV_DB) $(DEV_DB)-* $(DEV_DB).*
	$(LEDGERIMPORT) build -source $(FIXTURE_SOURCE) -map $(FIXTURE_MAP) \
		-config $(FIXTURE_CONFIG) $(if $(wildcard $(FIXTURE_RATES)),-rates $(FIXTURE_RATES),) \
		-o $(DEV_DB)
	printf '%s\n' '$(DEV_PASS)' | $(BINARY) user add $(DEV_USER) --admin -db $(DEV_DB)
	$(BINARY) devseed budget -db $(DEV_DB)
	@echo "dev-db: rebuilt $(DEV_DB) (login $(DEV_USER)/$(DEV_PASS)) — (re)start the :3390 server now"

## scaffold-db / import-sub — local only: the SPLIT go-live import (D26). scaffold-db
## builds a fresh reference db (subs, programs, funds, whole chart, rates; NO
## transactions); import-sub then ADDITIVELY imports ONE subsidiary's transactions
## into it, with a backup/restore safety net (the importer stays row-by-row, so a
## failed import is recovered by restoring the .bak, not rolled back). Import the US
## side, prove it live, then run import-sub again with IMPORT_SUB=UPH later. Stop the
## server first (it holds the db file open). Re-importing a subsidiary is refused —
## re-import means a fresh scaffold-db + import-sub from scratch.
IMPORT_DB  ?= $(BINDIR)/live.db
IMPORT_SUB ?= UPLAM
scaffold-db:
	@mkdir -p $(BINDIR)
	$(GO) build -o $(LEDGERIMPORT) ./cmd/ledgerimport
	$(GO) build -o $(BINARY) ./cmd/cuento
	rm -f $(IMPORT_DB) $(IMPORT_DB)-* $(IMPORT_DB).*
	$(LEDGERIMPORT) scaffold -map $(FIXTURE_MAP) -config $(FIXTURE_CONFIG) \
		$(if $(wildcard $(FIXTURE_RATES)),-rates $(FIXTURE_RATES),) -o $(IMPORT_DB)
	$(BINARY) check -db $(IMPORT_DB) --strict
	@echo "scaffold-db: reference db ready at $(IMPORT_DB) — now: make import-sub IMPORT_SUB=UPLAM"

import-sub:
	@mkdir -p $(BINDIR)
	$(GO) build -o $(LEDGERIMPORT) ./cmd/ledgerimport
	$(GO) build -o $(BINARY) ./cmd/cuento
	@bak="$(IMPORT_DB).bak.$$(date +%Y%m%d-%H%M%S)"; cp $(IMPORT_DB) "$$bak"; \
		echo "import-sub: backed up $(IMPORT_DB) -> $$bak"; \
		$(LEDGERIMPORT) import-subsidiary -source $(FIXTURE_SOURCE) -map $(FIXTURE_MAP) \
			-config $(FIXTURE_CONFIG) -subsidiary $(IMPORT_SUB) -o $(IMPORT_DB) && \
		$(BINARY) check -db $(IMPORT_DB) --strict && \
		echo "import-sub $(IMPORT_SUB): GREEN (backup kept at $$bak)" || \
		{ echo "import-sub $(IMPORT_SUB): FAILED — restore with: cp $$bak $(IMPORT_DB)"; exit 1; }

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
