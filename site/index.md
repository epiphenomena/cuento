---
layout: home
title: Overview
hero:
  eyebrow: Self-hosted double-entry fund accounting
  headline: Books that fit one nonprofit exactly.
  lede: >-
    cuento is fund accounting for a small, multi-subsidiary nonprofit — one Go
    binary and one SQLite file, serving bilingual server-rendered HTML. Money is
    tracked exactly, every change is versioned, and the reports a Form 990
    preparer needs come out directly.
  primary:
    label: Explore the code
    url: repo
  secondary:
    label: Read the docs
    url: /features.html
  tags:
    - One binary
    - One SQLite file
    - Bilingual en / es
    - MIT licensed

features_heading: What it does
features_lede: >-
  A complete fund-accounting ledger — not a demo. Every capability below maps to
  built, tested functionality, documented in depth on the
  <a href="/features.html">features page</a>.
features:
  - title: Double-entry fund accounting
    glyph: scale
    body: >-
      Every transaction balances to zero in its currency and to zero within each
      fund, so donor-restricted money is conserved through every account it
      touches — cash, buildings, or loan principal.
  - title: Donor-restricted funds
    glyph: lock
    body: >-
      Funds carry funder, purpose, and restriction metadata and drive the
      with/without-donor-restrictions presentation in reports — derived from fund
      tagging, not journaled transfer entries.
  - title: Multi-subsidiary + consolidation
    glyph: tree
    body: >-
      Subsidiaries form a tree with per-entity base currencies. Reports scope to
      any subsidiary consolidated with its descendants; intercompany accounts
      collapse after a net-to-zero check.
  - title: Multi-currency, GAAP FX
    glyph: coins
    body: >-
      Amounts are exact integer minor units. Report-time FX follows ASC 830:
      monetary remeasurement recognized in income (830-20), foreign-entity
      translation held in equity as a CTA (830-30).
  - title: Programs + functional classes
    glyph: grid
    body: >-
      Revenue and expense splits carry a program (990 Part III) and, for
      expenses, a functional class — program, management, or fundraising (990
      Part IX) — as orthogonal reporting dimensions.
  - title: Report catalog with drill-down
    glyph: chart
    body: >-
      Trial balance, balance sheet, income statement, functional expenses, fund
      activity, FX detail, and the Form 990 package. Every figure drills to its
      contributing splits and exports to CSV.
  - title: Bank-CSV import + reconciliation
    glyph: import
    body: >-
      Bank data enters only as an uploaded CSV, mapped, deduplicated, and staged
      for review through the same ledger invariants as manual entry. No bank
      credentials are ever stored.
  - title: Budgeting + expense reports
    glyph: calendar
    body: >-
      Named, reusable budget schedules with per-fund forecasts and cashflow
      projection, plus a submit-then-review expense workflow decoupled from
      book-editing.
  - title: Append-only audit trail
    glyph: history
    body: >-
      Every change appends a full-snapshot version row, attributable to the
      acting user. Transactions are voided, never hard-deleted; no code path
      rewrites history.

compare_heading: Why a purpose-built ledger
compare_lede: >-
  cuento is a bespoke, single-organization tool — not a general package. For a
  small nonprofit whose books have to be exactly right, owning an open ledger
  beats renting a generic one.
compare_cols:
  - cuento
  - General accounting SaaS
  - Enterprise fund accounting
  - Spreadsheets
compare_rows:
  - axis: Data ownership
    cuento: You host it; one SQLite file you own outright
    saas: Hosted; your books live in their cloud
    enterprise: Hosted; your books live in their cloud
    sheets: You own the files
  - axis: Fund-level conservation
    cuento: Balances to zero within each fund, not just the top line
    saas: Class/tag tracking, top-line restriction reporting
    enterprise: True fund dimension
    sheets: Manual and error-prone
  - axis: Double-entry + append-only audit
    cuento: Enforced on write and re-checked; history never rewritten
    saas: Double-entry; edit history varies
    enterprise: Double-entry with strong audit
    sheets: No enforcement
  - axis: Multi-currency GAAP (ASC 830)
    cuento: Remeasurement to income + CTA in equity, at report time
    saas: Limited or add-on
    enterprise: Comprehensive
    sheets: Hand-rolled
  - axis: Cost & lock-in
    cuento: Open source, MIT; no subscription, no vendor lock-in
    saas: Subscription; closed data; migration friction
    enterprise: Subscription; higher cost; closed data
    sheets: Free; no lock-in
  - axis: Fit
    cuento: Built to fit THIS org exactly
    saas: Configure your work to fit it
    enterprise: Configure your work to fit it
    sheets: Fits anything, guarantees nothing
compare_note: >-
  Claims about other tools are kept high-level and defensible; the point is not
  that they are bad, but that a purpose-built, self-hosted, open ledger is the
  right trade for one organization that needs its books to be exactly right.

tech_heading: Under the hood
tech_lede: >-
  Deliberately small and boring: no framework, no bundler, no CDN, no external
  database, no bank connection.
tech:
  - One Go binary, one SQLite file — pure-Go driver, CGO-free static build
  - Server-rendered HTML with a pinned, vendored copy of htmx
  - Strict Content-Security-Policy — no inline script or style
  - Forward-only migrations as the source of truth, reads via sqlc
  - A single write funnel — every mutation records a change and a version row
  - Ledger invariants enforced on write and re-verified by <code>cuento check</code>
tech_links:
  - label: Architecture
    url: /architecture.html
  - label: Data integrity
    url: /data-integrity.html

story_heading: Built to fit one nonprofit exactly

constraints_heading: Guiding constraints
constraints_lede: >-
  Four commitments shape every design decision. They are enforced structurally
  and by tests — not left to convention.
constraints:
  - title: Money is exact
    body: >-
      Amounts are integer minor units, never floating point in stored values or
      ledger math. Exponents from zero to four are supported at no cost.
  - title: Every change is versioned
    body: >-
      Each write lands in an append-only audit trail, attributable to the acting
      user. History is added to, never quietly overwritten.
  - title: Restricted funds are conserved
    body: >-
      Every transaction balances to zero within each fund, not only overall — so
      donor-restricted money is conserved through every account it touches.
  - title: Form 990 reports come out directly
    body: >-
      The functional-expense matrix and program-level statements a preparer
      needs are produced by the application, not reconstructed by hand.
---

**cuento is a customized, single-organization tool, not a general-purpose
accounting package.** Its data model and its design decisions are deliberately
tailored to one nonprofit's real workflows — name-keyed receivables, this
organization's fund / program / subsidiary structure, cash-flow-first budgeting
— rather than trying to fit every organization. It is not configurable into
someone else's chart of accounts, tax regime, or bookkeeping process, and does
not try to be. Building a bespoke line-of-business tool this complete — one that
fits the organization exactly instead of forcing the organization to fit the
software — is what AI-assisted coding now makes practical: the marginal cost of
a purpose-built system has fallen far enough that adapting the work to a generic
package is no longer the only sensible option.

cuento is intentionally small and boring. It runs as a single process on a
single VM behind TLS, with the entire database in one SQLite file. There is no
framework, no bundler, no CDN, no external database, and no bank connection —
bank data enters only as a CSV file the user uploads.

It targets two adversaries: authenticated misuse (a logged-in user doing
something they should not) and commodity automated web attacks. It does not try
to defend against a nation-state attacker, a malicious VM host, or an attacker
with filesystem access to the server; those are handled operationally
(disk encryption, VM hygiene, off-host backups), not in the application.
