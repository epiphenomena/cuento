# cuento

cuento is self-hosted, double-entry **fund accounting** for a small,
multi-subsidiary nonprofit. It is one Go binary and one SQLite file: a
server-rendered HTML application (html/template + a pinned copy of htmx and a few
hand-written ES modules) with a bilingual (English / Spanish) interface. It
tracks money exactly as integer minor units, versions every change into an
append-only audit trail, conserves donor-restricted funds through every account
they touch, and produces the reports a Form 990 preparer needs.

**cuento is a customized, single-organization tool — not a general-purpose
accounting package.** Its data model and design decisions are deliberately
tailored to one nonprofit's real workflows (name-keyed receivables, this
organization's fund / program / subsidiary structure, cash-flow-first
budgeting) rather than trying to fit every organization. It makes no attempt to
be configurable into someone else's chart of accounts, tax regime, or process.
Building a bespoke line-of-business tool this complete — one that fits the org
exactly instead of forcing the org to fit the software — is what AI-assisted
coding now makes practical: the marginal cost of a purpose-built system has
fallen far enough that "buy a generic package and adapt your work to it" is no
longer the only sensible option.

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
  FX follows GAAP (ASC 830): the remeasurement gain/loss on a foreign-currency
  monetary balance is recognized in the change in net assets (ASC 830-20), while
  a foreign entity's translation for consolidation stays in equity as a
  Cumulative Translation Adjustment (ASC 830-30).
- **A report catalog** covering trial balance, balance sheet (statement of
  financial position), income statement (statement of activities), functional
  expenses (990 Part IX), fund balances and activity, activities by restriction,
  FX conversion details, the program statement, the Form 990 package (Parts
  III / VIII / IX / X), the account ledger, the reconciliation statement, and
  budget reports. Every
  report figure is drillable to its contributing
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

## Foreign currency (ASC 830)

Each subsidiary's **functional currency** is its `base_currency`. This is a
management determination (ASC 830-10-45), not something cuento derives: an
organization sets each subsidiary's base currency to its true functional
currency. A balance held in a currency that equals its holding subsidiary's
functional currency carries no FX exposure; a balance in a different currency is
a foreign-currency item.

cuento distinguishes two GAAP mechanisms that land in different places.
**Remeasurement recognized in income (ASC 830-20):** a foreign-currency
*monetary* balance (cash, receivables, payables) held in a subsidiary whose
functional currency differs is remeasured to the functional currency at the
closing rate on the report date, while the transactions that built it were
measured at their transaction-date rates. The difference is a gain or loss
recognized in the change in net assets, surfaced as an "FX remeasurement
gain/(loss)" line on the converted Statement of Activities (income statement).
**Translation to a CTA in equity (ASC 830-30):** translating a foreign entity's
functional-currency books to the reporting currency for consolidation produces a
Cumulative Translation Adjustment within Net Assets — not income — which cuento
already carries as the intercompany consolidation residual on the balance sheet.

The discriminator between the two is the account's `intercompany` flag.
Non-intercompany foreign monetary balances take the income path;
intercompany balances stay on the translation (CTA) path, because their
equal-and-opposite FX-Clearing leg is equity-class and recognizing their
remeasurement in income would double-count against the CTA. Monetary
classification is a documented whitelist: accounts flagged `current_cash` (cash)
or `open_item` (receivables and payables).

The FX gain/loss is a **report-time computation, not a posted journal entry.**
cuento stores amounts natively and converts at report time, so the functional
(for example USD) value of a foreign balance only exists at report time. A
Lempira-bank expense in a USD-functional subsidiary is a single-currency HNL
transaction — DR expense HNL / CR HNL bank HNL — and does *not* run through FX
Clearing (that account is only for value moved between two currencies). The
remeasurement gain or loss on the residual HNL bank balance is computed at report
time from its closing-rate value versus its transaction-date basis.

The **FX Conversion Details** report (`fx_detail`) is the auditor's
reconciliation artifact. As of a date and scoped to a subsidiary, it lists each
foreign-currency monetary item with its native balance, closing rate and rate
date, transaction-date (historical) basis, remeasured-at-closing value, and FX
gain/(loss), grouped by subsidiary, with a per-functional-currency total equal to
the amount recognized in income. Because the remeasurement now lands in income,
the Statement of Activities' change in net assets articulates with the balance
sheet's net-asset change; before this was recognized, the gap between the two was
exactly the unrecognized remeasurement.

In the shipped `cuento demo` database, "Banco Lempira" is an HNL bank in the
USD-functional US subsidiary, funded by a 250,000.00 HNL contribution and drawn
down by a 100,000.00 HNL Food Purchases expense, leaving 150,000.00 HNL. As the
Lempira weakens against the dollar (schedule 24.00 to 25.70 HNL/USD), the
residual is worth fewer dollars at the report date than the transaction-date
value of the flows that built it: a remeasurement loss of $461.74, shown on the
FX Conversion Details report and the Statement of Activities.

**Boundaries and assumptions.**

- Functional currency = `base_currency` is a management setting, not derived.
- Monetary classification is a whitelist (`current_cash` plus `open_item`); a
  foreign-currency debt carrying neither flag (for example a plain foreign bank
  loan) is not picked up, and such accounts should carry `open_item`.
- Non-monetary foreign balances (fixed assets, inventory, prepaid) correctly
  produce no remeasurement, but currently convert at the closing rate rather than
  the historical rate — immaterial in practice, noted as a future refinement.
- The highly-inflationary-economy exception (ASC 830-10-45-11) is not automated.
- Functional-to-reporting translation of recognized income uses the period-end
  closing rate (an average rate would be a refinement); it is an identity when
  the functional currency equals the reporting currency.

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
self-contained Jekyll site in [`site/`](site/) — a hand-written product showcase
(feature cards, a comparison, and the deeper docs pages) with its own layouts and
a single stylesheet, no remote theme, so it builds on plain Jekyll offline:

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
