# PROGRESS — env/tooling scratch (not the source of truth)

Resume state lives in **git log** + **PLAN.md checkboxes**. This file only tracks the local
toolchain so a resumed session doesn't re-discover it. Do not mirror step status here.

## Toolchain (installed to `$(go env GOPATH)/bin`, must be on PATH for `make lint`)
- go 1.25.7
- node v25.6.1
- gofumpt (installed)
- goose (installed)
- sqlc v1.31.1 (installed)
- golangci-lint v2.12.2 (installed, built with go1.25.7 — verified runs)

Subagents MUST prepend `$(go env GOPATH)/bin` to PATH so `make lint` finds sqlc/golangci-lint/gofumpt/goose.

## Delegation model (per AGENTS + advisor)
- One PLAN step per subagent; the subagent runs the full loop and COMMITS before returning.
- Coordinator verifies each with `git show <hash>` (don't trust the report; check tests came first).
- Non-`[P]` steps are strictly sequential. First parallel fan-out: p03.2/.3/.4 (worktree + rebase).

## Working-agreement clarifications (from the human)
- **"Done = green" means locally green** (2026-07-11). No hosted CI; the human pushes as they choose.
  For every step, "done" = `make lint test check` + the ci.yml steps (vet, golangci-lint, gofumpt -l,
  go test ./..., govulncheck ./...) all pass locally. Do NOT block on a remote/host run.
- Toolchain auto-upgrades to go1.25.12 via go.mod (`GOTOOLCHAIN=auto`); needed for a clean govulncheck.
