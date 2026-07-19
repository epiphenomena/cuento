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
    label: Read the docs
    url: /architecture.html
  secondary:
    label: Repository
    url: repo
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

## What it is (and is not)

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
