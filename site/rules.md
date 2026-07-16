---
title: Rules
layout: default
nav_order: 6
---

# Governing rules

cuento is held to a small set of non-negotiable engineering rules. They are
enforced by tests and code review, and violating any of them is treated as a
defect even when the feature otherwise works. The rules below are the project's
working agreement; the deep-dive pages explain the mechanisms that satisfy them.

## 1. Dependencies are allow-listed

Only a vetted set of dependencies is permitted. Any addition requires a recorded
decision and explicit human sign-off, and the standard library is preferred over
convenience packages. This keeps the supply-chain surface small and auditable.

## 2. A single write funnel

Every database write goes through one helper in the store package; handlers,
reports, and the CLI never open transactions or execute writes directly. Reads
outside the store go only through generated queries. One choke point is where
versioning, actor attribution, and the ledger invariants are enforced. See
[Data integrity](data-integrity.md).

## 3. Money is exact

Stored amounts are always 64-bit integer minor units. Floating point may appear
only in exchange-rate values and report-time currency conversion — never in
stored amounts or intermediate ledger math. The books never accumulate rounding
drift.

## 4. Migrations are the truth

The schema is defined by embedded, forward-only, numbered migrations. An applied
migration is never edited, there are no down migrations, and the runner backs up
the database file before applying anything. The schema history is reproducible
and cannot be reversed by accident. See [Architecture](architecture.md).

## 5. Everything is versioned

Every mutation of a business table appends a full-snapshot row to its versions
table inside the same transaction, tied to one change row. Version and change
rows are never updated or deleted by anything, ever, and a password hash never
appears in a version row. The result is a complete, append-only audit trail.

## 6. SQL goes through the query compiler

All queries live in files compiled by sqlc; there is no string-concatenated SQL.
The named ledger-integrity checks may be reviewed constant SQL strings. Data
access stays type-safe and injection-resistant.

## 7. Ledger invariants are enforced twice

Zero-sum per transaction and per fund; one currency and one subsidiary per
transaction; splits only on active leaf accounts within the transaction's
subsidiary; account subsidiary sets covering their children; a program present
exactly on revenue and expense splits and within the fund's program scope; a
functional class present exactly on expense splits. Each is enforced in the store
on write and independently re-verified by the `cuento check` command; schema
triggers cover the row-local subset. Correctness is guaranteed at write time and
auditable after the fact. See [Data integrity](data-integrity.md).

## 8. Routes exist only through the registry

Every route is declared in one registry with an explicit permission, and a
permission-matrix test picks up new routes automatically. A route that bypasses
the registry is treated as a security defect. See [Security](security.md).

## 9. No user-visible string outside the translation catalogs

Templates render text through the translation function, and Go code returns error
keys for display. The English and Spanish catalogs must hold identical key sets,
enforced by a test. Account names come from stored data, and proper nouns are
data rather than catalog entries.

## 10. All date and number formatting goes through the money package

Rendering and parsing honor each user's display settings. No template path
formats a date or number directly, and native date inputs are not used, so every
figure is presented consistently and in the user's locale.

## 11. Real data never enters the repository

The confidential source export is git-ignored, and its values never appear in
code, tests, documentation, commit messages, or chat. Tests use a synthetic
fixture exclusively, and the one-shot historical importer runs locally only.

## 12. The frontend is boring

html/template, a vendored pinned copy of htmx, and small hand-written ES modules
— no framework, no bundler, no CDN, no inline scripts, and a strict
Content-Security-Policy. JavaScript is unit-tested, and end-to-end behavior is
covered by a browser test suite. See [Architecture](architecture.md).

## 13. A defined security posture

argon2id password hashing; server-side sessions stored in SQLite; HttpOnly,
SameSite cookies (Secure outside local development); cross-origin protection on
every mutating route; rate-limited login; uniform authentication errors that do
not reveal whether an account exists; and security headers asserted by tests
across every route. See [Security](security.md).

## 14. The audit trail is sacred

No code path — including maintenance tooling — deletes or rewrites change or
version rows. Transactions are soft-deleted only. The history can always be
trusted.
