---
title: Security
layout: default
nav_order: 3
---

# Security

cuento is a self-hosted ledger on a single VM behind TLS. Its security posture is
scoped to two realistic adversaries and, importantly, is enforced by tests rather
than left aspirational.

## Threat model

The design targets exactly two adversaries:

1. **Authenticated misuse** — a logged-in user (bookkeeper, viewer, or a
   compromised account) doing something they should not: editing books they may
   only read, reaching an admin action, or quietly altering history.
2. **Commodity web attacks** — the automated background radiation of the public
   internet: credential stuffing, CSRF, injected-markup XSS, clickjacking,
   session theft, and brute-force login guessing.

It does **not** model a nation-state adversary, a malicious VM host, or an
attacker with filesystem access to the server. Those are outside a one-VM
self-hosted tool's control and are addressed operationally — disk encryption, VM
hygiene, and off-host backups — not in the application.

## Authenticated misuse: permissions plus audit

- **Every route is authorized through one registry.** `internal/web/routes.go`
  declares each route once with an explicit permission, and the mount function is
  the only place a route attaches to the router. The permission-matrix test is
  generated *from* the registry, so a route added without a permission is a test
  failure, not a silent hole. Permission classes are `Public`, `AnyUser`,
  `TxnRead`, `TxnWrite`, `ReportGroup(name)`, and `Admin`; an admin implies
  everything. Enforcement is a single pure decision function, asserted over every
  permission-by-persona pair and again over real HTTP by the permission matrix.
- **The audit trail is append-only.** Every mutation of a versioned business
  table appends a full-snapshot row to its `*_versions` twin, in the same
  transaction, tied to one `changes` row naming the acting user. No code path —
  including maintenance tooling — updates or deletes a `changes` or `*_versions`
  row. Transactions are soft-deleted (voided), never hard-deleted. So an
  authenticated user's every change is attributable and permanent: misuse is
  visible, which for an internal-trust tool is the operative control.
- **All writes go through one funnel**, and the ledger invariants are enforced on
  write and re-verified by `cuento check`, so a bug or a crafted request cannot
  leave the books invalid undetected.

## Commodity web attacks

- **Passwords: argon2id.** Passwords are hashed with argon2id (memory-hard,
  salted, tuned). Verification is centralized, and the version snapshot of the
  users table never carries the password hash.
- **Sessions: server-side, in SQLite.** Sessions (via scs) store only the user
  id; the middleware re-reads the full identity each request, so a permission or
  lockout change takes effect without a re-login. Cookies are `HttpOnly`,
  `SameSite=Lax`, path-scoped, and `Secure` in production (off only under `-dev`,
  which speaks plain HTTP). Sessions have an absolute lifetime cap and an idle
  timeout, and expired rows are swept by the session store's background cleanup.
- **CSRF / cross-origin: stdlib cross-origin protection.** The whole router is
  wrapped in Go's `http.CrossOriginProtection`, which rejects non-safe (mutating)
  cross-site browser requests by inspecting `Sec-Fetch-Site` and Origin-vs-Host.
  Nothing is hand-rolled; safe methods and same-origin or non-browser requests
  pass.
- **XSS / injection: html/template plus a strict CSP.** All HTML is rendered
  through `html/template` with contextual auto-escaping, and the
  Content-Security-Policy pins `default-src`, `script-src`, and `style-src` (and
  the rest) to `'self'` with `object-src 'none'`, `base-uri 'none'`,
  `frame-ancestors 'none'`, and `form-action 'self'` — with no `'unsafe-inline'`
  anywhere. An injected `<script>`, an inline `onclick`, an inline `<style>`, or
  a `style="..."` attribute is refused by the browser. A `-dev` end-to-end test
  walks the main pages plus an htmx swap asserting zero CSP violations, so no
  inline script or style can slip in unnoticed.
- **Clickjacking:** framing is forbidden two ways — `X-Frame-Options: DENY` and
  `frame-ancestors 'none'`.
- **The rest of the header set, on every response.** `X-Content-Type-Options:
  nosniff`, `Referrer-Policy: same-origin`, `Cross-Origin-Opener-Policy:
  same-origin`, and — in production only — `Strict-Transport-Security` (withheld
  under `-dev`, whose server speaks plain HTTP). Because the header middleware is
  outermost, every response carries these headers regardless of status. A
  route-sweep test iterates the live route registry and asserts the header values
  on every route, so a missing or altered header on any current or future route
  fails the tests.
- **Login brute-force: rate limiting plus uniform errors.** The login POST is
  throttled per (IP, username) with a small burst and a slow sustained refill, so
  one attacker cannot lock out a victim site-wide. Auth errors are uniform, and
  the unknown-user path spends the same argon2id time via a fixed decoy hash, so
  neither the message nor the timing enumerates usernames.
- **TLS.** In production, in-process autocert terminates TLS on port 443 with an
  80-to-443 redirect; `-dev` runs plain HTTP locally.

## Not stored: bank credentials

cuento never holds bank login credentials, API tokens, or a live bank connection.
Bank data enters only as a CSV file the user uploads (upload, column mapping,
staged review, then post). There is no aggregator and no screen scraping, which
eliminates the single highest-value secret a small nonprofit ledger could hold.
Imported rows go through the same write funnel, invariants, and audit trail as any
manual entry.

## Dependency posture

The dependency surface is deliberately tiny and allowlisted: a pure-Go SQLite
driver, goose (migrations), scs (sessions), an argon2id wrapper, `golang.org/x`
packages for autocert TLS and rate-limiting, and a small bilingual-catalog stack.
`go-cmp` is used in tests only. Any addition requires an explicit decision entry
and human acknowledgment. Playwright, the end-to-end harness, is a test-only Node
dependency, never imported by the shipped binary or the frontend runtime.

## Verification

Each control is backed by a test or gate rather than a claim:

| Control | Test / gate |
|---|---|
| Every route authorized | Permission-matrix, decision-policy, and route-registry-complete tests |
| Security headers on every route | Route-sweep header test; HSTS dev-vs-prod test |
| No inline script or style (CSP clean) | A `-dev` end-to-end CSP-clean walk |
| en/es catalog parity | The i18n catalog-parity test |
| Ledger invariants hold | Store enforcement plus `cuento check` |
| Audit is append-only | Store versioning tests plus a current-equals-latest check |
| No known vulnerable dependencies | `govulncheck ./...` |
