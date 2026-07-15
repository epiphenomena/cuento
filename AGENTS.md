# AGENTS.md — cuento working agreement

This file defines *how* to work. PLAN.md defines *what* to build, in order. docs/DECISIONS.md records *why*. Read all three before writing code; when they conflict, DECISIONS.md wins for rationale, PLAN.md wins for sequence, this file wins for method.

**Product in three lines:** cuento is self-hosted double-entry fund accounting for a small multi-subsidiary nonprofit. One Go binary, one SQLite file, server-rendered HTML + htmx, bilingual (en/es) UI. It tracks money exactly (integer minor units), audits every change, conserves donor-restricted funds through every account, and produces the reports a 990 preparer needs.

---

## Working method

Execute PLAN.md steps strictly in order (except `[P]` siblings — see Parallelism). For **every** step:

1. **Write the tests listed in the step first.** Run them. Confirm each fails *for the right reason* (missing symbol / wrong behavior — not a typo). If a listed test seems wrong or untestable, don't skip it silently: record the deviation in the commit body and, if it reflects a design choice, in DECISIONS.md.
2. **Implement to green.** Smallest code that satisfies the tests and the step's Build text. No speculative abstraction, no work from later steps.
3. **Refactor** within the step's scope only.
4. Run `make lint test check`. All green, zero warnings you introduced.
5. Tick the step's checkbox in PLAN.md **in the same commit**.
6. Commit: `pNN.M area: summary` (see Commits).

Never batch multiple steps into one commit. Never start a step while a prior non-`[P]` step is unticked. If a step is blocked on a human input (marked in PLAN.md), stop and say so rather than inventing the input.

### Parallelism

Steps marked `[P]` may be dispatched to subagents **only alongside their `[P]` siblings in the same phase**. Rules: each subagent owns a disjoint file set; each runs the full `make lint test check` before its commit; the coordinating agent integrates sequentially (rebase, re-run checks) — no merge commits.

### Ambiguity

When something is underspecified: (1) check docs/DECISIONS.md, (2) check PLAN.md open questions — each has a default; use it, (3) otherwise make the smallest reversible choice, record one line in DECISIONS.md, and proceed. Do not stall, and do not make large irreversible choices unilaterally.

## Commits

Format: `pNN.M area: summary` — e.g. `p08.2 store: post/update/delete transactions`.
`area ∈ {chore, db, store, money, i18n, ledger, web, reports, ux, import, ops, build, docs, testutil}`.
Body: notable choices, deviations from the step text, DECISIONS entries added. One step per commit; the PLAN.md checkbox tick and any DECISIONS.md edit ride along.

**Branching.** Develop directly on `main` — this project does NOT use feature branches. Commit each step to `main` as you go; the human manages pushes (see "Definition of done" — locally green is done). Ignore any generic "if on the default branch, branch first" default: here, `main` is the working branch.

## Hard rules

Violating any of these is a defect even if tests pass.

1. **Dependencies.** Only the allowlist in PLAN.md D15. Any addition requires a DECISIONS.md entry *and* explicit human acknowledgment. No transitive-dep sprawl via convenience libs; prefer stdlib.
2. **Single write funnel.** All database writes go through `internal/store`'s `write(ctx, kind, note, fn)` helper. Handlers, reports, and CLI never open transactions or execute writes directly. Reads outside store are allowed only via sqlc-generated queries.
3. **Money is exact.** Stored amounts are `int64` minor units, always. `float64` may appear only in exchange-rate values and report-time conversion (D12), never in stored amounts or intermediate ledger math.
4. **Migrations are truth.** goose, embedded, forward-only, numbered. Appendix A in PLAN.md is a *reference sketch*; the migrations are authoritative. Never edit an applied migration; never write a down migration; the runner backs up the db file first.
5. **Everything is versioned.** Every mutation of a versioned business table appends a full-snapshot row to its `*_versions` table inside the same transaction, tied to one `changes` row. Version and change rows are never updated or deleted, by anything, ever. `users_versions` never contains `password_hash`.
6. **SQL via sqlc.** All queries live in `.sql` files compiled by sqlc. No string-concatenated SQL. The named integrity checks in `internal/ledger` may be `const` SQL strings (they are static and reviewed as a set).
7. **Ledger invariants are enforced twice.** Zero-sum per transaction *and per fund*, single currency per transaction, single subsidiary per transaction, splits only on active leaf accounts in the transaction's subsidiary, account subsidiary sets: parent ⊇ union of children, transaction subsidiary within the fund's subsidiary set, program present exactly on revenue/expense splits and within the fund's program scope, functional class present exactly on expense splits — each is enforced in the store on write **and** verified by `cuento check`. Schema triggers cover the row-local subset.
8. **Routes only via the registry.** Every route is declared in `routes.go` with an explicit permission; the permission-matrix test picks up new routes automatically. A route that bypasses the registry is a security bug.
9. **No user-visible string outside the i18n catalog.** Templates render text via the `t` func; Go code returns error *keys* for user display. The en/es catalogs must have identical key sets (enforced by test). Account names come from `account_names`; proper nouns (subsidiaries, funds, payees) are stored data, not catalog entries.
10. **All date/number rendering and parsing** goes through `internal/money` formatters honoring per-user settings. Never `time.Format` or `strconv` directly in a template path. Never `input[type=date]`.
11. **Real data never enters the repo.** `fixtures/source/` is gitignored and its *values* are never copied into code, tests, docs, commit messages, or chat. Tests use the synthetic fixture (PLAN Appendix D) exclusively. The one-shot importer runs locally only.
12. **Frontend is boring.** `html/template`, vendored pinned htmx, small hand-written ES modules. No framework, no bundler, no CDN, no inline event handlers, strict CSP with no inline script. JS is unit-tested with `node --test`. **Functional (end-to-end) tests use Playwright** (added 2026-07-11 at the human's request, superseding the earlier "no browser automation dependency" stance): a test-only suite under `e2e/` that launches the real `cuento serve -dev` and drives actual usage in a browser. Playwright is a dev/test dependency only — it is never imported by the shipped Go binary or the frontend runtime, and the boring-frontend rules above still hold for the app itself. See DECISIONS "Functional testing".
13. **Security posture.** argon2id for passwords; scs sessions in SQLite; HttpOnly/SameSite=Lax cookies (Secure outside `-dev`); cross-origin protection on all mutating routes; login rate-limited; uniform auth errors (no user enumeration); security headers asserted by tests across every route.
14. **Audit is sacred.** No code path deletes or rewrites `changes` or `*_versions` rows — including "cleanup" tooling. Soft-delete only for transactions.

## Repository layout

```
cmd/cuento/          entrypoint: serve, migrate, check, user, ratesync
cmd/ledgerimport/    one-shot historical CSV-ledger import (local only, never deployed)
internal/db/         Open() (the only place pragmas are set), embedded goose migrations, sqlc output
internal/store/      the only writer: actor context, write funnel, entity operations, balance queries
internal/money/      Amount, currency math, date/number format enums
internal/i18n/       embedded catalogs (en.toml, es.toml), T(), template func
internal/ledger/     integrity checks (error + warning severities), Check()
internal/reports/    registry, toolkit, one folder per report with template + golden
internal/web/        routes.go (registry), middleware, handlers, templates/, static/ (embedded)
internal/testutil/   NewDB(t), Fixture(t), AssertVersioned, golden helpers
fixtures/            source/ (gitignored real export), sample.db (gitignored)
docs/                DECISIONS.md, ledger-export.md, deploy.md, security.md, qa-entry.md
deploy/              systemd units, litestream.yml
```

## Make targets

`gen` (sqlc) · `lint` (govet, golangci-lint, gofumpt check) · `test` (go test ./... + node --test) · `check` (build + `cuento check` on a fixture db) · `fixture` (local: run ledgerimport → fixtures/sample.db) · `golden` (regenerate report goldens; diff must be reviewed, never blind-committed) · `run` (dev server, `-dev` mode) · `release` (CGO-free linux/amd64, trimpath, version ldflags).

## UI / frontend work

When working on the UI, keep a **live dev server on `localhost:3390`** so changes can be eyeballed as they land:

```
cp fixtures/sample.db bin/dev.db            # once: a throwaway copy so the fixture stays clean
printf 'devpass123\n' | bin/cuento user add admin --admin -db bin/dev.db   # once: a login
bin/cuento serve -dev -addr :3390 -db bin/dev.db   # run (background it)
```

Rebuild (`make build`) and restart the server after Go/template changes so the embedded assets refresh; static CSS/JS edits also require a rebuild (they are embedded). Log in as `admin` / `devpass123`.

## Testing conventions

- Table tests by default; property tests where PLAN.md names them.
- Handler tests hit the real mounted router (`httptest`) with a real migrated temp db — no handler-level mocks of the store.
- Goldens live in `testdata/` next to the report; regenerated only via `make golden` with reviewed diffs.
- `testutil.Fixture(t)` is the canonical dataset (PLAN Appendix D); tests needing tiny bespoke data build it inline.
- No test touches the network. Rate-source parsers run against recorded bodies in `testdata/`.
- Every store mutation test asserts versioning via `testutil.AssertVersioned`.
- Negative tests matter as much as positive: each invariant has at least one test proving it *rejects*.

## Style

Latest stable Go. gofumpt. Errors wrapped with `%w` and context (`fmt.Errorf("post transaction: %w", err)`); typed sentinel errors (`ErrUnbalanced`, `ErrFundUnbalanced`, …) where handlers branch on them. No panics on server paths. No package-level mutable state except explicit registries (routes, reports) synced at startup. Contexts carry the actor; nothing else smuggled through context. Comments explain *why*, not *what*. Keep functions small; keep the diff small.

## Definition of done (per step)

- [ ] Listed tests written first and observed failing for the right reason
- [ ] `make lint test check` green
- [ ] New invariants covered by both store enforcement and a `cuento check` rule where PLAN.md says so
- [ ] i18n: any new UI string exists in **both** catalogs
- [ ] PLAN.md checkbox ticked; DECISIONS.md updated if any choice was made
- [ ] Commit message in format, scoped to this step only
