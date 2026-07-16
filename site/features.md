---
title: Features
layout: default
nav_order: 5
---

# Features

The feature set below is what the application actually does; each item maps to
built, tested functionality.

## Double-entry fund accounting

cuento is a double-entry ledger where **funds are a first-class split
dimension**. Every transaction balances to zero in its currency and to zero
*within each fund*, so donor-restricted money is conserved through every account
it touches — cash, receivables, buildings, or loan principal — rather than only at
the top line. Unrestricted activity is simply the null-fund group. Reports derive
the GAAP "released from restrictions" presentation from fund tagging rather than
journaling transfer entries.

## Donor-restricted funds

A fund documents a grant or restricted gift: funder, purpose, restriction type
(purpose / time / perpetual), and dates. A fund scopes to one or more
subsidiaries and optionally to a program subtree, and a transaction may only use a
fund whose scope includes its subsidiary. Fund pages show per-currency balances
with a warning badge on a negative balance; the fund statement gives one fund's
period activity broken into opening, received, applied (expenses versus
non-expense applications like asset purchases and loan principal), and closing.

## Multi-subsidiary and consolidation

Subsidiaries form a tree with a single root, each with a base currency. An account
maps to one or more subsidiaries, with the invariant that a parent's subsidiary
set is a superset of the union of its children's. A report scopes to any
subsidiary consolidated with all its descendants; the root is full consolidation,
and the default report currency is the scoped subsidiary's base currency.
Cross-subsidiary funding uses paired transactions through intercompany-flagged
due-to/due-from accounts; a consolidated report that covers both sides collapses
those accounts after asserting they net to zero, and renders a warning row rather
than silently dropping a nonzero net.

## Programs and functional classes

Programs are a separate tree (a dimension), seeded with a single "General" root as
the unallocated default. Every revenue and expense split carries a program;
balance-sheet splits carry none. Every expense split additionally carries a
functional class from a fixed set — program, management, or fundraising (990 Part
IX) — defaulted from the account and overridable per split for allocations like
rent or salaries. Programs and functional classes are orthogonal: Part IX columns
come from the class, Part III rows from the program. Keeping both as dimensions
lets the chart of accounts hold natural categories (salaries, supplies, occupancy)
without duplicating the tree per program or per class.

## Exact, multi-currency money

Amounts are stored as integer minor units. Transactions are single-currency;
cross-currency flows pass through a multicurrency FX Clearing account, whose
converted balance on reports is the cumulative FX gain or loss. Report-time
conversion uses the latest on-or-before rate (balance-sheet figures at the closing
rate, activity at each transaction date's rate) with half-even rounding at
aggregates. Rates are stored per pair per date and can be fetched by
`cuento ratesync` or imported from a CSV.

## The report catalog

Reports are grouped by audience (financial, funds, programs, tax,
reconciliation, budget), and each figure is drillable to the exact contributing
splits, including from a converted or consolidated cell down to its native
underlying splits. Each report also exports to CSV.

- **Trial balance** — as-of balances per scope, native currencies plus a
  converted column.
- **Balance sheet** (statement of financial position) — assets, liabilities, and
  equity, with net assets split into with- and without-donor-restrictions.
- **Income statement** (statement of activities) — period activity with
  comparative columns.
- **Functional expenses** — 990 Part IX: expense accounts under their effective
  Part IX lines, columns for program / management & general / fundraising / total.
- **Fund balances and activity** — per-fund balances with funder and restriction
  metadata, and a per-fund period statement.
- **Activities by restriction** — a statement of activities with without- and
  with-donor-restrictions columns and a derived "net assets released from
  restrictions" line.
- **Program statement** — revenue and expense by natural account per program,
  rolled up over the program tree; feeds 990 Part III.
- **Form 990 package** — one page per part (Part VIII revenue, Part IX expenses,
  Part X balance sheet, Part III program services), each with an explicit Unmapped
  bucket rather than dropping rows.
- **Account ledger** — a printable register for a date range with opening and
  closing balances and a fund column.
- **Reconciliation statement** — statement info, included splits, and the
  opening/closing chain for a finalized reconciliation.
- **Budget reports** — forecast, actuals-vs-budget, and cashflow projection,
  bucketed by period and broken out per fund.
- **Capital campaign** — a quarterly capital-campaign statement scoped to one
  restricted fund.

## Bank-CSV import

Bank data enters only as an uploaded CSV. The mapping UI presents the file's
actual columns and lets the user tag each one (date, description, amount, or a
debit/credit pair, memo, or ignore); the mapping can be saved as a reusable
profile and deactivated later without breaking the audit link to batches it
produced. Rows are deduplicated against existing splits and pending rows, staged
for review, and posted through the phase-12 editor — with the counter-side and
its fund and functional class prefilled from the payee template — so every
imported entry goes through the same invariants and audit trail as manual entry.

## Reconciliation

A reconciliation is per account and currency and spans all funds, because a bank
statement covers one balance regardless of how the money is restricted. The
workspace toggles cleared splits and enables finalize only when the difference is
zero. Finalizing locks the cleared splits (amount, account, transaction, date,
fund); editing or voiding them requires an audited reopen, and a reopen is refused
while a later finalized reconciliation exists on the same account and currency.

## Budgeting

Budget lines are keyed by subsidiary, account, fund, and program, with an
amount-per-occurrence and a named, reusable schedule that generates concrete
occurrence dates. The scheduling model is discrete dated occurrences, not
pro-rata: the full amount lands on each occurrence date, and reports bucket
occurrences by period and sum. Schedule kinds include one-time/annual, monthly (a
day-of-month or an ordinal weekday), semimonthly, biweekly, weekly, and a custom
imported date list, with a weekend-adjustment policy for day-of-month kinds.
Forecasts, actuals-vs-budget, and cashflow projection all break out by fund, so
restricted and unrestricted net assets are projected separately.

## Expense reports

A submission-then-review workflow decoupled from book-editing. A low-privilege
submitter — who may have no ledger access at all — enters expense (and revenue)
lines for one subsidiary and submits; the lines need not balance. A reviewer with
write access opens the submission in the transaction editor, balances it, and
posts a real, versioned transaction linked back to its source report, or rejects
it with a reason that routes it back to the submitter. A converted report is
immutable and shows its resulting transaction.

## Bilingual interface

The UI is bilingual (English and Spanish) from embedded catalogs, with a test
enforcing identical key sets across the two so translations stay honest. Every
user-visible string renders through the catalog; account names come from a
per-language account-names table, and proper nouns (subsidiaries, funds) are
stored data. Adding a language is adding one catalog file. Each user's locale,
date format, number format, display mode, negative style, theme, default
subsidiary, and default program are personal settings.

## The historical importer

`cmd/ledgerimport` is a one-shot, local-only tool that builds the production
database from a cleaned full-ledger CSV plus a reviewed mapping file (which assigns
subsidiaries, funds, programs, functional classes, and Form 990 codes up front). It
runs locally only, is never deployed, and never enters the repository with real
data; the resulting database is validated with `cuento check --strict` before
cutover. It supports both a whole-database build and a split scaffold-then-import
flow that adds one subsidiary's transactions at a time.
