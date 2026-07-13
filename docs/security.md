# Security — threat model and posture

*cuento* is self-hosted double-entry fund accounting for a small multi-subsidiary
nonprofit: one Go binary, one SQLite file, server-rendered HTML + htmx, run on a
single VM behind TLS. This document states what it defends against, how, and — as
important — what it deliberately does **not** try to do. It describes the system as
built through Phase 18 (the p18.4 hardening sweep). It uses no real infrastructure
details, hostnames, or credentials (AGENTS DATA RULE 11).

## Who and what we protect against

The realistic adversaries for a self-hosted nonprofit ledger are two, and the
design targets exactly them:

1. **Authenticated misuse** — a logged-in user (bookkeeper, viewer, or a
   compromised account) doing something they should not: editing books they may
   only read, reaching an admin action, quietly altering or erasing history.
2. **Commodity web attacks** — the untargeted, automated background radiation of
   the public internet: credential stuffing, CSRF, XSS via injected markup,
   clickjacking, session theft, brute-force login guessing.

We do **not** model a nation-state adversary, a malicious VM host, or an attacker
with filesystem access to the server — those are outside a one-VM self-hosted
tool's control and are addressed operationally (disk encryption, VM hygiene,
Litestream backups), not in the application.

## Mitigation 1 — authenticated misuse: permissions + audit

**Every route is authorized through one registry.** `internal/web/routes.go`
declares every route once with an explicit `Perm`; `Mount` is the *only* place a
route attaches to the mux; and the permission-matrix test is generated *from* the
registry, so a route added without a permission is a test failure, not a silent
hole (AGENTS rule 8). A route mounted outside the registry is a security bug by
definition. Permission classes: `Public`, `AnyUser`, `TxnRead`, `TxnWrite`,
`ReportGroup(name)`, `Admin`. `is_admin` implies everything (D10). Enforcement is a
single pure function, `decide()`, asserted across every `(Perm × persona)` pair by
`TestDecidePolicy` and, over real HTTP, by `TestPermissionMatrix`.

**The audit trail is append-only and cannot be rewritten.** Every mutation of a
versioned business table appends a full-snapshot row to its `*_versions` twin,
inside the same transaction, tied to one `changes` row naming the acting user
(AGENTS rule 5, D4). No code path — including "cleanup" tooling — ever updates or
deletes a `changes` or `*_versions` row (rule 14). Transactions are soft-deleted
(voided), never hard-deleted. `users_versions` never stores `password_hash`. So an
authenticated user's every change is attributable and permanent: misuse is
*visible*, which for an internal-trust tool is the operative control.

**All writes go through one funnel.** `internal/store`'s `write(ctx, kind, note,
fn)` is the sole writer (rule 2); handlers, reports, and the CLI never open
transactions directly. The ledger invariants (zero-sum per transaction *and* per
fund, single currency/subsidiary per transaction, active-leaf splits, fund/program
scoping) are enforced on write *and* re-verified by `cuento check` (rule 7) — so a
bug or a crafted request cannot leave the books in an invalid state undetected.

## Mitigation 2 — commodity web attacks

- **Passwords: argon2id.** `internal/auth` hashes with `argon2id.DefaultParams`
  (memory-hard, salted, tuned). Password verification is centralized;
  `users_versions` never carries the hash.
- **Sessions: scs in SQLite.** Server-side sessions (`internal/web/session.go`)
  store only the user id; the middleware re-reads the full identity each request,
  so a permission or lockout change takes effect without re-login. Cookie posture:
  `HttpOnly`, `SameSite=Lax`, `Path=/`, name `cuento_session`, and `Secure` in
  production (off only under `-dev`, which speaks plain HTTP). **Lifetime 12 h**
  (an absolute cap covering a working day) with a **2 h idle timeout** (an
  abandoned tab de-authenticates); reviewed p18.4, kept — sane for the bookkeeper
  workflow, not indefinite. Expired rows are swept by scs's background cleanup on
  our goose-managed `sessions` table.
- **CSRF / cross-origin: stdlib `http.CrossOriginProtection`.** The whole mux is
  wrapped in Go 1.25's cross-origin protection, which rejects non-safe (mutating)
  cross-site browser requests via `Sec-Fetch-Site` / Origin-vs-Host. Nothing is
  hand-rolled; safe methods and same-origin/non-browser requests pass (D9).
- **XSS / injection: `html/template` + a strict CSP, no inline anything.** All
  HTML is rendered through `html/template` (contextual auto-escaping). The
  Content-Security-Policy (`internal/web/middleware.go`) is
  `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self';
  connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none';
  frame-ancestors 'none'; form-action 'self'` — **no `'unsafe-inline'`, ever**. An
  injected `<script>`, an inline `onclick=`, an inline `<style>`, or a `style="…"`
  attribute is refused by the browser. The frontend is boring by rule (rule 12):
  vendored pinned htmx and small hand-written same-origin ES modules, no framework,
  no bundler, no CDN, no inline handlers. A `-dev` click-through e2e
  (`e2e/tests/csp-clean.spec.js`) walks every main page plus the styleguide and an
  htmx swap asserting **zero** CSP violations, so no inline script/style can slip
  in across phases 10–18 unnoticed.
- **Clickjacking: `X-Frame-Options: DENY` + `frame-ancestors 'none'`.** Framing is
  forbidden two ways (the CSP directive supersedes the header on modern browsers;
  the header covers the rest).
- **The rest of the header set, on every response.** `X-Content-Type-Options:
  nosniff`, `Referrer-Policy: same-origin`, `Cross-Origin-Opener-Policy:
  same-origin`, and — **in production only** — `Strict-Transport-Security:
  max-age=31536000; includeSubDomains` (HSTS is withheld under `-dev`, whose server
  speaks plain HTTP, so a developer's browser is never pinned to https for a host
  that only serves http). Because `secureHeaders` is the *outermost* middleware,
  every response carries these headers regardless of status — 200, 302, 403, 404,
  the `/healthz` JSON, `/static` assets, the ops-backup octet-stream alike. A
  route-sweep test (`internal/web/headers_test.go: TestSecurityHeadersEveryRoute`)
  iterates the live route registry and asserts the header *values* on every route,
  so a missing or altered header on any current or future route fails CI
  (rule 13).
- **Login brute-force: rate limiting + uniform errors.** `POST /login` is
  throttled per `(IP, username)` — burst 5, ~2/min sustained refill
  (`internal/web/ratelimit.go`) — so one attacker cannot lock out a victim
  site-wide, and sustained guessing is choked. Auth errors are uniform and the
  unknown-user path spends the same argon2id time via a fixed decoy hash, so
  neither the message nor the timing enumerates usernames (rule 13).
- **TLS.** In production, in-process autocert terminates TLS on :443 with an :80
  redirect (p18.2); the cert cache lives in the data dir. `-dev` runs plain HTTP
  locally.

## Explicitly NOT stored: bank credentials

cuento **never holds bank login credentials, API tokens, or any live bank
connection.** Bank data enters *only* as a **CSV file the user uploads** (Phase 17:
upload → column mapping → staged review → post). There is no aggregator, no screen
scraping, no stored username/password/OAuth token for any financial institution.
This is a deliberate scope decision, and it eliminates the single highest-value
secret a small nonprofit ledger could hold: there is nothing here for an attacker
to steal that would grant access to the organization's actual bank accounts. The
import path validates and dedupes uploaded rows; the resulting ledger entries go
through the same write funnel, invariants, and audit trail as any manual entry.

## Dependency posture

The dependency surface is deliberately tiny and allowlisted (D15, AGENTS rule 1):
`modernc.org/sqlite` (pure-Go driver, CGO-free), `pressly/goose/v3` (migrations),
`alexedwards/scs/v2` (+`sqlite3store`, sessions), `alexedwards/argon2id`,
`golang.org/x/crypto` (autocert), `golang.org/x/time` (rate), `nicksnyder/go-i18n/v2`
+ `BurntSushi/toml` + `golang.org/x/text` (the bilingual catalog stack, D14).
`google/go-cmp` is allowlisted for tests. Any addition needs a DECISIONS entry and
human acknowledgment. Playwright (the e2e harness) is a test-only Node dependency,
never imported by the shipped Go binary or the frontend runtime (rule 12).

**`govulncheck ./...` is clean** (no vulnerability reachable from cuento's code;
run as part of the p18.4 sweep and re-runnable any time).

## Verification (this is enforced, not aspirational)

| Control | Test / gate |
|---|---|
| Every route authorized | `TestPermissionMatrix`, `TestDecidePolicy`, `TestRouteRegistryComplete` |
| Security headers on every route | `TestSecurityHeadersEveryRoute`, `TestHSTSDevVsProd` |
| No inline script/style (CSP clean) | `e2e/tests/csp-clean.spec.js` |
| en/es catalog parity | `internal/i18n: TestCatalogParity` |
| Ledger invariants hold | store enforcement + `cuento check` (Z1–Z19) |
| Audit append-only | store versioning tests + `cuento check` current==latest |
| No known vulnerable deps | `govulncheck ./...` |
| Go-live strict gate (manual) | `make golive-check` on the local `sample.db` (human review, D26) |
