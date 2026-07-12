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

.PHONY: all gen lint test check e2e fixture golden run release build clean tools

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

## test — Go tests plus JS unit tests (node --test) when present.
test:
	$(GO) test $(PKG)
	@if ls internal/web/static/*.test.js internal/web/static/**/*.test.js >/dev/null 2>&1; then \
		node --test internal/web/static; else echo "test: no JS tests yet, skipping node --test"; fi

## check — build, then run the ledger integrity suite (`cuento check`) against a
## FRESH temp migrated db, which MUST be clean (an empty migrated db has only the
## seeded roots and no splits). Hermetic: the temp db is created and removed here,
## and nothing depends on fixtures/sample.db (p09.3, gitignored). If a local
## fixtures/sample.db happens to exist we additionally check it (--strict), but the
## default must never require it. migrate + check share the same cwd and -db value
## so db.Open resolves them to one physical file (the p06.4 path-escape quirk).
check: build
	@tmpdb=$$(mktemp -u -t cuento-check-XXXXXX.db); \
	trap 'rm -f "$$tmpdb" "$$tmpdb"-* "$$tmpdb".*' EXIT; \
	echo "check: fresh migrated db -> cuento check (must be clean)"; \
	$(BINARY) migrate -db "$$tmpdb" >/dev/null && $(BINARY) check -db "$$tmpdb"; \
	if [ -f fixtures/sample.db ]; then \
		echo "check: fixtures/sample.db present -> cuento check --strict"; \
		$(BINARY) check -db fixtures/sample.db --strict; \
	fi

## e2e — opt-in Playwright functional tests (pE.1). Builds bin/cuento, installs
## the pinned test-only Node deps (@playwright/test, bundled chromium already
## cached), and runs the suite in e2e/ against the REAL `cuento serve -dev`. NOT
## part of `make test` (which stays hermetic — no browser, no network per AGENTS):
## e2e needs a browser and is run explicitly. Playwright is a dev/test dependency
## only, never a Go dep and never shipped (AGENTS rule 12, DECISIONS "Functional
## testing"). `npm ci` requires e2e/package-lock.json (committed).
e2e: build
	cd e2e && npm ci && npx playwright test

## fixture — local only: run ledgerimport to produce fixtures/sample.db (phase 9).
fixture:
	@echo "fixture: ledgerimport wiring lands in phase 9"

## golden — regenerate report goldens; diffs must be reviewed, never blind-committed (phase 15).
golden:
	@echo "golden: report goldens land in phase 15"

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
