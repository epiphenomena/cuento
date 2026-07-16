# cuento

cuento is self-hosted, double-entry **fund accounting** for a small,
multi-subsidiary nonprofit. It is one Go binary and one SQLite file: a
server-rendered HTML application (html/template + a pinned copy of htmx and a few
hand-written ES modules) with a bilingual (English / Spanish) interface. It
tracks money exactly as integer minor units, versions every change into an
append-only audit trail, conserves donor-restricted funds through every account
they touch, and produces the reports a Form 990 preparer needs.

Full documentation site: **[GitHub Pages site](site/index.md)** (see
"Documentation site" below for how it is served).

---

## Features

- **Double-entry fund accounting.** Every transaction balances to zero in its
  currency, and to zero *within each fund*, so restricted money is conserved
  through cash, buildings, and loan payoffs alike.
- **Donor-restricted funds as a dimension.** Funds (grants, restricted gifts)
  carry funder / purpose / restriction metadata, scope to one or more
  subsidiaries and optionally a program subtree, and drive the with/without
  donor-restrictions presentation in reports (derived, not journaled).
- **Multi-subsidiary + consolidation.** Subsidiaries form a tree, each with a
  base currency. Reports scope to any subsidiary consolidated with its
  descendants; intercompany accounts are collapsed after a net-to-zero check
  (a nonzero net renders as a visible warning row, never dropped).
- **Programs and functional classes as orthogonal dimensions.** Revenue and
  expense splits carry a program (990 Part III) and, for expenses, a functional
  class (990 Part IX: program / management / fundraising), so the chart of
  accounts stays a tree of natural categories.
- **Exact money, multi-currency.** Amounts are stored as `int64` minor units.
  Cross-currency flows pass through an FX Clearing account; report-time
  conversion uses on-or-before exchange rates with half-even rounding.
- **A report catalog** covering trial balance, balance sheet (statement of
  financial position), income statement (statement of activities), functional
  expenses (990 Part IX), fund balances and activity, activities by restriction,
  the program statement, the Form 990 package (Parts III / VIII / IX / X), the
  account ledger, a capital-campaign statement, the reconciliation statement,
  and budget reports. Every report figure is drillable to its contributing
  splits.
- **Bank-CSV import** with horizontal column mapping, reusable profiles,
  deduplication, and a staged review queue that posts through the same ledger
  invariants as manual entry. No bank credentials are ever stored.
- **Reconciliation** per account and currency, spanning all funds, with a
  split-lock on finalized statements.
- **Budgeting** with named, reusable schedules (discrete dated occurrences, not
  pro-rata), per-fund forecasts, actuals-vs-budget, and cashflow projection.
- **Expense reports**: a submit-then-review workflow decoupled from
  book-editing; a reviewer converts a submission into a balanced, audited
  transaction.
- **Bilingual UI (en / es)** from embedded catalogs with enforced key parity.
- **Append-only audit** of every change, attributable to the acting user.

---

## Architecture

cuento is deliberately small and boring. The whole application is one binary
serving server-rendered HTML from an embedded filesystem, backed by a single
SQLite database file.

- **One Go binary, one SQLite file.** The pure-Go `modernc.org/sqlite` driver
  keeps the release build CGO-free and statically linkable. `internal/db.Open`
  is the only place database pragmas are set.
- **Migrations are the source of truth.** Schema is defined by embedded,
  forward-only [goose](https://github.com/pressly/goose) migrations; the runner
  backs up the database file before applying. There are no down migrations.
- **Reads via sqlc.** All SQL lives in `.sql` files compiled by
  [sqlc](https://sqlc.dev); there is no string-concatenated SQL. (The named
  ledger integrity checks are the one reviewed exception, as static `const`
  strings.)
- **Server-rendered frontend.** HTML is rendered through `html/template`
  (contextual auto-escaping). The frontend is a pinned, vendored copy of htmx
  plus small hand-written ES modules — no framework, no bundler, no CDN, no
  inline scripts — served under a strict Content-Security-Policy with no
  `'unsafe-inline'`.
- **A single write funnel.** Every database write goes through
  `internal/store`'s `write(ctx, kind, note, fn)` helper, which records one
  `changes` row per call and appends full-snapshot version rows in the same
  transaction. Handlers, reports, and the CLI never open transactions directly.
- **A reports registry.** Each report is a small piece of data plus a pure
  `Run` function over a read-only toolkit; the web layer auto-mounts a route,
  a CSV export, and a drill-down per report, and the permission matrix and
  `/reports` index pick it up with no additional wiring.
- **An i18n catalog.** `internal/i18n` embeds `en.toml` / `es.toml`; a test
  enforces identical key sets across the two. All user-visible strings render
  through the catalog.

Internal package layout:

```
cmd/cuento/          entrypoint: serve, migrate, user, check, ratesync, expense-report
cmd/ledgerimport/    one-shot historical CSV-ledger import (local only, never deployed)
internal/db/         Open() (pragmas), embedded goose migrations, sqlc output
internal/store/      the only writer: actor context, write funnel, entity ops, balance queries
internal/money/      Amount, currency math, date/number format enums
internal/i18n/       embedded catalogs (en.toml, es.toml), T(), template func
internal/ledger/     integrity checks (error + warning severities), Check()
internal/reports/    registry, toolkit, one file per report with template + golden
internal/web/        routes.go (registry), middleware, handlers, templates/, static/ (embedded)
internal/testutil/   NewDB(t), Fixture(t), AssertVersioned, golden helpers
docs/                internal working docs (DECISIONS.md, security.md, deploy.md, cli.md, ...)
site/                published documentation site (Jekyll)
deploy/              systemd units, litestream.yml
```

For depth, see the [Architecture page](site/architecture.md) on the
documentation site.

---

## Security

cuento models two realistic adversaries for a self-hosted nonprofit ledger:
authenticated misuse (a logged-in user doing something they should not) and
commodity automated web attacks. It does not model a nation-state attacker, a
malicious VM host, or an attacker with filesystem access to the server.

Highlights:

- **argon2id** password hashing; the hash is never stored in a version row.
- **Server-side sessions** (scs, stored in SQLite) with `HttpOnly`,
  `SameSite=Lax` cookies, `Secure` outside `-dev` mode.
- **Cross-origin protection** on all mutating routes (Go's stdlib
  `http.CrossOriginProtection`).
- **Login rate-limiting** and **uniform auth errors** (no user enumeration; the
  unknown-user path spends the same hashing time).
- **A permission-gated route registry**: every route is declared once with an
  explicit permission, and a permission-matrix test is generated from the
  registry, so an unguarded route is a test failure.
- **A strict CSP** (`script-src 'self'`, no inline script or style) and a full
  security-header set asserted by tests across every route.

For the full threat model, see the [Security page](site/security.md) and
`docs/security.md`.

---

## Data integrity

- **Money is exact**: `int64` minor units, never floats in stored or
  intermediate ledger math.
- **A single write funnel** is the only path that mutates the database.
- **Everything is versioned**: each mutation appends a full-snapshot row to the
  table's `*_versions` twin, tied to one `changes` row. Version and change rows
  are never updated or deleted by anything.
- **Migrations are truth**: forward-only, backup-first, never edited once
  applied.
- **The ledger invariants** (zero-sum per transaction and per fund, single
  currency and subsidiary per transaction, splits only on active leaf accounts,
  program/functional-class presence and scope rules) are enforced in the store
  on write *and* re-verified by `cuento check`.
- **Audit is sacred**: transactions are soft-deleted (voided), never
  hard-deleted; no code path rewrites history.

For the full picture, see the [Data integrity page](site/data-integrity.md).

---

## Build, run, and test

cuento uses a `Makefile`; the common targets are:

| Command | What it does |
|---|---|
| `make run` | Run the dev server (`cuento serve -dev`, plain HTTP, relaxed cookie flags). |
| `make test` | Go tests (`go test ./...`) plus the `node --test` JS unit tests. Hermetic: no browser, no network. |
| `make check` | Build, then run the `cuento check` integrity suite against a fresh migrated database (must be clean). |
| `make e2e` | Opt-in Playwright functional tests that drive a real `cuento serve -dev`. Needs a browser; not part of `make test`. |
| `make lint` | `go vet`, golangci-lint, and a gofumpt formatting check. |
| `make release` | CGO-free `linux/amd64` static binary, `-trimpath`, version stamped via ldflags. |
| `make build` | Build `bin/cuento` for the host. |
| `make gen` | Regenerate sqlc query code. |
| `make golden` | Regenerate report goldens (review the diff; never blind-commit). |

Playwright is a test-only Node dependency; it is never imported by the shipped
binary or the frontend runtime.

Deployment (a single GCE e2-micro VM with Litestream backups to GCS and
in-process autocert TLS) is documented in `docs/deploy.md`; the CLI reference is
`docs/cli.md`.

---

## Documentation site

The `docs/` folder holds internal working documents (decisions, deploy runbook,
CLI reference, security threat model). The **published** documentation site is a
self-contained Jekyll site in [`site/`](site/), themed with
[just-the-docs](https://just-the-docs.com/) (sidebar navigation + search):

- [Overview](site/index.md)
- [Architecture](site/architecture.md)
- [Security](site/security.md)
- [Data integrity](site/data-integrity.md)
- [Features](site/features.md)

**Enabling GitHub Pages.** GitHub's branch-based Pages deployment can only serve
the repository root or `/docs`, and both are occupied (the root is the app, and
`docs/` holds internal working docs that must not be published). Serve `site/`
with a GitHub Actions workflow instead: add a workflow that runs
`actions/jekyll-build-pages` with `source: ./site`, uploads the artifact, and
deploys it via `actions/deploy-pages`, then set Pages "Build and deployment" to
"GitHub Actions" in the repository settings. See `site/index.md` for the same
note.

---

## Working on cuento

`AGENTS.md` is the working agreement and the numbered hard rules; `PLAN.md` is
the ordered build plan (its phase list is the feature set); `docs/DECISIONS.md`
records why the design is the way it is. Read those three before changing code.
