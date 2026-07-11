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

.PHONY: all gen lint test check fixture golden run release build clean tools

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

## check — build, then run `cuento check` against a fixture db (wired in p08.3).
check: build
	@echo "check: cuento check wiring lands in p08.3"

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
