---
title: Data integrity
layout: default
nav_order: 4
---

# Data integrity

cuento is a ledger, so correctness of the numbers and permanence of the record
are the product. The mechanisms below are enforced structurally, not by
convention.

## Money is exact

Stored amounts are `int64` in the currency's minor units — always. The currency's
exponent comes from a currencies table, so exponents from zero (yen-style) to four
are supported at no cost. Floating point appears only in exchange-rate values and
in report-time currency conversion; it never appears in stored amounts or in
intermediate ledger math. Report-time conversion looks up the on-or-before rate
(direct pair, then reciprocal) and rounds half-even at final aggregates, so
storage stays integer-exact.

Signed amounts are net-debit (debits positive, credits negative). Whether a figure
displays as signed or as debit/credit columns is a per-user display choice, not a
storage difference.

## A single write funnel

Every database mutation goes through one helper in `internal/store`. It opens one
transaction, records exactly one `changes` row naming the acting user (carried on
the request or CLI context), and runs the change plus its version appends inside
that transaction. Handlers, reports, and the CLI never open a transaction or write
directly; reads outside the store go only through sqlc-generated queries. Because
there is exactly one write path, there is no way to mutate the database while
skipping the audit trail or the invariant checks.

## Everything is versioned

Every mutation of a versioned business table appends a full-snapshot row to that
table's `*_versions` twin, in the same transaction, tied to one `changes` row. The
current tables hold the denormalized latest state; the version tables hold the
full history. State as of a time T is the row with the greatest `valid_from` at or
before T per entity, excluding rows whose operation is a delete.

Two rules make this an audit trail rather than a log:

- **Version and change rows are never updated or deleted** — by anything, ever,
  including maintenance tooling.
- **The users version table never contains the password hash**, so history can be
  read freely without exposing credentials.

## Migrations are truth

The schema is defined by embedded, forward-only, numbered goose migrations. The
runner backs up the database file before applying pending migrations. There are no
down migrations, and an applied migration is never edited — a change to the schema
is always a new migration. Backups beat theoretical rollbacks for a
single-file database.

## SQL only through sqlc

All queries live in `.sql` files compiled by sqlc into typed Go; there is no
string-concatenated SQL. The one reviewed exception is the ledger's named
integrity checks, which are static `const` SQL strings reviewed together as a set.

## The ledger invariants, enforced twice

The core invariants are enforced in the store on write **and** re-verified by
`cuento check` (with the row-local subset also covered by schema triggers), so a
bug or a crafted request cannot leave the books invalid undetected:

- Every non-voided transaction **sums to zero** in its currency, and to zero
  **within each fund** — so restricted money is conserved through any account,
  cash or buildings or loan payoffs alike.
- A transaction is **single-currency** and **single-subsidiary**.
- Splits fall only on **active leaf accounts** that are mapped to the
  transaction's subsidiary.
- Each account's subsidiary set is a **superset of the union of its children's**
  sets.
- A split's fund, when set, is scoped to a subsidiary set containing the
  transaction's subsidiary.
- A **program** is present exactly on revenue and expense splits (and, when the
  fund is program-scoped, inside that scope); a **functional class** is present
  exactly on expense splits.
- The subsidiary and program trees are acyclic with exactly one root each.

`cuento check` also reports **warnings** that are surfaced but not blocked at
write time: intercompany accounts that do not net to zero at full consolidation, a
restricted fund with a negative cumulative balance, and an active revenue/expense
leaf with activity but no effective Form 990 code.

## Audit is sacred

Transactions are soft-deleted (voided), never hard-deleted, and no code path
rewrites the change or version history. A finalized reconciliation locks the
amount, account, transaction, date, and fund of its cleared splits; editing them
requires an audited reopen first. The integrity suite includes a check that each
current row equals its latest version snapshot, so drift between the live state
and its audit trail is detectable.
