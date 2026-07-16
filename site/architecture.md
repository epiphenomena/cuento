---
title: Architecture
layout: default
nav_order: 2
---

# Architecture

cuento is one Go binary serving server-rendered HTML from an embedded
filesystem, backed by a single SQLite database file. Everything below follows
from that choice.

## The stack

- **One binary, one file.** The application ships as a single Go executable. It
  uses the pure-Go `modernc.org/sqlite` driver, so the release build is CGO-free
  and statically linkable, and cross-compiles trivially.
  `internal/db.Open` is the only place database pragmas are configured.
- **SQLite as the database.** All state lives in one file. A single-writer
  SQLite file suits a small nonprofit's transaction volume and is what makes the
  one-VM deployment (with off-host streaming backups) honest, rather than
  scale-to-zero on an ephemeral filesystem.
- **Server-rendered HTML + htmx.** Pages are rendered with `html/template`.
  Interactivity is a pinned, vendored copy of htmx plus small hand-written ES
  modules. There is no SPA framework, no bundler, and no CDN.

## Request lifecycle

A browser request enters a middleware chain and, if authorized, reaches a
handler that reads through the store and renders a template:

1. **Security headers** are applied as the outermost middleware, so every
   response — 200, 302, 403, 404, JSON health check, or a static asset — carries
   them.
2. **Cross-origin protection** rejects non-safe cross-site browser requests.
3. **Session** resolution loads the session and re-reads the full user identity
   each request, so a permission or lockout change takes effect without a
   re-login.
4. **Authorization** checks the route's declared permission against the resolved
   user via a single pure decision function.
5. **Language resolution** picks the locale (user setting, then cookie, then
   English) so every rendered string comes from the right catalog.
6. The **handler** runs: it reads through `internal/store` (or, for reports, the
   read-only report toolkit) and renders an `html/template`. Mutations go through
   the write funnel; nothing else opens a transaction.

htmx requests target a fragment of the page and swap it in place; the handlers
return the same partials the full-page render composes, so there is one source of
truth per region.

## Layer boundaries

The internal packages have deliberately narrow responsibilities:

| Package | Responsibility |
|---|---|
| `internal/db` | `Open()` (the only place pragmas are set), the embedded goose migrations, and the sqlc-generated query code. |
| `internal/store` | The only writer. Holds the actor context, the write funnel, entity operations, and the balance/register queries. |
| `internal/money` | `Amount` (int64 minor units), currency math, and the date/number format enums — the only date/number formatting and parsing entry points. |
| `internal/i18n` | The embedded `en.toml` / `es.toml` catalogs, `T()`, and the template function. |
| `internal/ledger` | The named integrity checks (error and warning severities) and `Check()`. |
| `internal/reports` | The report registry, the read-only toolkit, and one file per report with its template and golden. |
| `internal/web` | The route registry (`routes.go`), middleware, handlers, and the embedded templates and static assets. |

Dependencies point inward: `web` and `reports` read through `store`; `store`
owns the database; `money` and `i18n` are leaf utilities used everywhere.

## Migrations and sqlc

- **Migrations are the source of truth for the schema.** They are embedded
  [goose](https://github.com/pressly/goose) migrations, forward-only and
  numbered; the runner backs up the database file before applying pending
  migrations, and there are no down migrations. An applied migration is never
  edited. `cuento serve` auto-migrates on start; `cuento migrate` runs it
  standalone.
- **Reads go through sqlc.** All SQL lives in `.sql` files compiled by
  [sqlc](https://sqlc.dev) into typed Go; there is no string-concatenated SQL.
  The one reviewed exception is the ledger's named integrity checks, which are
  static `const` SQL strings reviewed as a set.

## The write funnel

Every database mutation goes through `internal/store`'s
`write(ctx, kind, note, fn)` helper. It opens one transaction, records exactly
one `changes` row naming the acting user (carried on the context), and runs the
supplied function — which performs the entity change and appends the matching
full-snapshot `*_versions` rows in the same transaction. Handlers, reports, and
the CLI never open a transaction or write directly. This is what makes the audit
trail complete by construction: there is no write path that skips versioning.

## The reports registry

A report is a small piece of data (an id, a title key, a group, and a parameter
spec) plus a pure `Run(ctx, toolkit, params)` that returns a typed `Table`. The
report never opens a transaction; it computes through a read-only toolkit over
the store's balance and activity queries. The web layer auto-mounts a route, a
CSV export, and a drill-down endpoint per report — each gated by the report's
permission group — and the `/reports` index and the permission matrix pick the
report up with no additional wiring. Adding a report is therefore a code-only
addition: register it, add its i18n keys to both catalogs, and add a golden.

## The frontend and CSP model

The frontend is boring on purpose. It is `html/template` output, a pinned
vendored copy of htmx, and small hand-written ES modules — no framework, no
bundler, no CDN, and no inline event handlers. It is served under a strict
Content-Security-Policy whose `script-src` and `style-src` are `'self'` with no
`'unsafe-inline'`, so an injected `<script>`, an inline `onclick`, or a
`style="..."` attribute is refused by the browser. Static assets are served
content-addressed (a hash in the filename) with immutable cache headers. Dates
are always entered through text inputs with a hand-written calendar popover, never
a native `input[type=date]`, so formatting follows each user's settings.

## Testing

- **Go table tests** by default, with property tests where the plan names them.
  Handler tests hit the real mounted router against a real migrated temporary
  database — the store is never mocked at the handler layer.
- **Report goldens** live next to each report and are regenerated only with a
  reviewed diff.
- **Every store mutation test asserts versioning** through a shared
  `AssertVersioned` helper, and every ledger invariant has at least one negative
  test proving it rejects.
- **JavaScript units** run under `node --test` for the hand-written ES modules.
- **End-to-end tests use Playwright** (a test-only Node suite) that launches a
  real `cuento serve -dev` and drives a browser. Playwright is a dev/test
  dependency only; it is never imported by the shipped binary or the frontend
  runtime, and the hermetic `make test` does not run it.
