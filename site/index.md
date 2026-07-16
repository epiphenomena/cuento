---
title: Overview
layout: default
nav_order: 1
---

# cuento

cuento is self-hosted, double-entry **fund accounting** for a small,
multi-subsidiary nonprofit. It is one Go binary and one SQLite file: a
server-rendered HTML application (html/template plus a pinned copy of htmx and a
few hand-written ES modules) with a bilingual English / Spanish interface.

Its guiding constraints:

- **Money is tracked exactly**, as integer minor units — never as floating point
  in stored amounts or ledger math.
- **Every change is versioned** into an append-only audit trail, attributable to
  the acting user.
- **Donor-restricted funds are conserved** through every account they touch,
  because every transaction balances to zero *within each fund*, not only
  overall.
- **The reports a Form 990 preparer needs** are produced directly, including the
  functional-expense matrix and program-level statements.

## What it is (and is not)

cuento is intentionally small and boring. It runs as a single process on a
single VM behind TLS, with the entire database in one SQLite file. There is no
framework, no bundler, no CDN, no external database, and no bank connection —
bank data enters only as a CSV file the user uploads.

It targets two adversaries: authenticated misuse (a logged-in user doing
something they should not) and commodity automated web attacks. It does not try
to defend against a nation-state attacker, a malicious VM host, or an attacker
with filesystem access to the server; those are handled operationally
(disk encryption, VM hygiene, off-host backups), not in the application.

## Documentation map

| Page | Contents |
|---|---|
| [Architecture](architecture.md) | The binary/SQLite/htmx stack, request lifecycle, package boundaries, migrations + sqlc, the write funnel, the reports registry, the frontend/CSP model, and testing. |
| [Security](security.md) | Passwords, sessions, cross-origin protection, rate-limiting, the permission-gated route registry, the strict CSP, and the security headers — with the tests that enforce each. |
| [Data integrity](data-integrity.md) | Exact money, the single write funnel, append-only versioning, migrations-as-truth, sqlc-only SQL, and the ledger invariants enforced twice. |
| [Features](features.md) | The full feature set: fund accounting, subsidiaries and consolidation, programs and functional classes, the report catalog, bank import, reconciliation, budgeting, and expense reports. |
| [Rules](rules.md) | The fourteen non-negotiable engineering rules the project is held to, each with the reason behind it and a link to the mechanism that satisfies it. |

## Enabling GitHub Pages for this site

GitHub's branch-based Pages deployment can serve only the repository root or the
`/docs` folder, and both are occupied in this repository (the root is the
application, and `docs/` holds internal working documents that must not be
published). Serve this `site/` folder with a GitHub Actions workflow instead:

1. Add a workflow that builds this directory with `actions/jekyll-build-pages`
   using `source: ./site`, uploads the result with `actions/upload-pages-artifact`,
   and deploys it with `actions/deploy-pages`.
2. In the repository settings, under **Pages** (Build and deployment), set the
   source to **GitHub Actions**.

The site uses the [just-the-docs](https://just-the-docs.com/) remote theme, so no
theme gem needs to be vendored; the `github-pages` gem in `site/Gemfile` provides
a compatible build for local previews (`bundle exec jekyll serve`).
