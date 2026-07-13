# Deferred / pending / incomplete register (p22.4)

This is the single, authoritative list of everything in `cuento` that is **not
done**, and why. It is compiled from the Phase 21 backlog, the tracked
follow-ups, and a full-codebase grep for deferral markers, each verified against
the current code. It is a *register*, not a work order.

**p22.5 update:** the genuinely low-effort integrity items are now landed —
§2.1 (reopen-while-later-OPEN guard), §2.2 (account-merge reconciled-source
block-guard; full recon repoint stays backlog), and the `reconciliations`
Z3/Z5 integrity-coverage gap (§5 item 1) are RESOLVED; §2.4 (account-ledger
currency-in-range) was investigated and reclassified as a non-issue (the drop
cannot occur). Everything else remains documented backlog. Section/marker refs
below are descriptive (not line-pinned), since the code has since shifted.

Structure-only per DATA RULE 11 — no `fixtures/source/` values appear here.

Status legend used below: **OPEN** (still deferred), **RESOLVED** (a prior
deferral note whose blocker has landed and the work is done — listed so the
marker is not mistaken for open work).

---

## 1. Intentional v1 non-goals (Phase 21 backlog)

These are explicit non-goals for v1 (PLAN.md "Phase 21 — Backlog"). Each is a
deliberate scope boundary, not an oversight.

| Item | Rationale for deferral |
| --- | --- |
| **Per-subsidiary permissions** | v1 permissions are org-global (D10); no RBAC-by-subsidiary machinery. Real backlog (touches the whole enforcement path). |
| **Per-subsidiary program scoping (Q5)** | Programs are org-global — every program usable in every subsidiary. Per-sub scoping only if miscoding actually occurs. |
| **Holiday calendar for budget-schedule adjustment** | Budget schedules adjust for weekends only (`weekend_adjust`); no holiday table. Deferred because holidays are jurisdiction-specific and low-value for v1. Real backlog. |
| **Intercompany elimination entries beyond the D19 collapse** | Consolidation eliminates via the D19 collapse only; full elimination entries are out of scope. |
| **Receipt attachments** | No file/blob storage for supporting documents in v1. |
| **Global audit browser & "books as edited at time T" reports** | The bitemporal data already supports both (D4/D5); only the read UI is deferred. |
| **Recurring/scheduled *ledger* transactions** | Distinct from budget recurrence (Phase 19). Auto-posting real ledger txns on a schedule is out of scope. |
| **Board-designated (quasi-restricted) funds** | Only the three donor-restriction kinds (purpose/time/perpetual) in v1. |
| **Additional UI languages beyond en/es** | The i18n catalogs make new languages a file-drop; no new language shipped. |
| **API tokens** | No programmatic/API auth surface in v1 (session cookies only). |
| **Dashboard / home page** | The landing page is a minimal index (`home.tmpl`), not an analytics dashboard. |
| **Multi-org** | Single-org by construction; no tenancy layer. |

---

## 2. Known limitations / documented edges

Small, deliberately-scoped behaviors. Each was recorded as a design choice at
its step; each is verified against the current code below.

### 2.1 Reconciliation reopen while a later OPEN recon exists — **RESOLVED (p22.5)**
`internal/store/reconciliations.go` — `Reopen`.

`Reopen` now ALSO refuses to reopen a finalized recon when ANOTHER OPEN recon
stands on the same `(account, currency)` (a `CountOpenReconciliations` check
reusing the existing `ErrOpenReconciliationExists` sentinel; the recon being
reopened is still 'finalized' at the check so it is not self-counted). This
closes the two-OPEN-recons state that `StartReconciliation`'s one-open guard
would refuse to create and that no `cuento check` Z-rule catches (the D13
single-open invariant is store-guard-only). The web `reconReopen` handler maps
the sentinel to a 409. Covered by `TestReopenBlockedWhenOtherOpenExists` and the
updated `TestReopenBlockedWhenLaterFinalizedExists` (whose p16.5 success
assertion — reopening the earlier recon after reopening the later — was the
two-opens bug and now asserts `ErrOpenReconciliationExists`). See DECISIONS p22.5.

### 2.2 Account merge does not repoint reconciliations — **GUARDED (p22.5); full repoint is backlog**
`internal/store/merge.go` (doc + `MergeAccount` guard); `internal/web/merge.go`
(422 mapping + preview text); `internal/store/merge_test.go`
(`TestMergeBlockedSourceReconciled`).

**Update (p22.5):** the integrity hole is CLOSED with a block-guard.
`MergeAccount` now REFUSES the merge (`ErrMergeSourceReconciled`, via
`CountReconciledSplitsForAccount`) when the source account has ANY split with a
non-NULL `reconciliation_id` — checked inside the write funnel before any
mutation, so a rejected merge writes nothing and `ledger.Check` stays clean. The
web merge handler maps it to a clean 422 field error (not a 500 / not a raw
SQLite abort), and the preview shows the count of reconciled splits blocking the
merge instead of the old misleading "0 reconciliations move". **Full recon
repointing (the correct merge-the-chains behavior) remains BACKLOG** — see the
"real design problem" note below and the §5 "Real backlog" list.

The ORIGINAL analysis (why the guard is needed), preserved:

`MergeAccount` repoints every split from src to dst
(`RepointSplitAccount` + op='update' version) but leaves each split's
`reconciliation_id` pointing at a reconciliation **on the old src account**. The
TODO was written when the `reconciliations` table did not exist; that blocker
landed in p16.1 (migration 00014), so this is now a *stale-open* deferral, not a
truly-blocked one. Precise consequence (verified against Z8 and the 00014
trigger):

- **Splits cleared against a FINALIZED recon:** `trg_split_locked_when_finalized`
  (00014 L97) fires on `NEW.account_id IS NOT OLD.account_id`, so the merge's
  account_id UPDATE **ABORTs**. No integrity hole, but the merge fails with a
  raw low-level SQLite abort rather than a clean typed rejection.
- **Splits cleared against an OPEN recon:** the trigger does **not** fire (it
  guards only finalized recons), so the UPDATE succeeds and the split ends with
  `account_id = dst` while still linked to a recon whose `account_id = src` →
  **Z8 fires** (`sqlZ8`, `checks.go` L360: `s.account_id <> r.account_id`). This
  is a genuine latent integrity hole surfaced by `cuento check`.

Full correct behavior (repoint / merge recon chains across the two accounts) is
a real design problem (recons are per `(account, currency)`; dst may already
hold overlapping recons; the finalized opening chain) → backlog. A cheap
*block-guard* slice is possible (see §5).

### 2.3 Bank import: EU decimal-comma amounts unsupported — **OPEN**
`internal/bankimport/parse.go` — `parseMoney`/`stripCurrency` (L229, L243).

EU **dates** are supported (`DateEU`, DD/MM/YYYY, L61). EU **amounts** with a
decimal comma are not: `stripCurrency` unconditionally strips every comma as a
thousands separator (L246, `strings.ReplaceAll(s, ",", "")`) and passes the
result to `money.Parse(..., money.NumberPlain)`, which expects a dot decimal. So
`"1.234,56"` becomes `"1.23456"` — wrong. Bank plain exports in this deployment
use dot-decimal, so this is deferred, not fixed. A locale-aware amount parser is
the real fix. Real backlog (a genuine parser branch + config, with its own tests).

### 2.4 Account-ledger report: currency-in-range-only section dropped — **RESOLVED / non-issue (p22.5: the drop cannot occur)**
`internal/reports/account_ledger.go` (`unionCurrencies(openByCcy, closeByCcy)`).

Investigated in p22.5 and found to be a NON-ISSUE — the feared drop cannot
happen. The section set is `union(opening, closing)`, and both maps come from
`SubtreeBalancesAsOf`, which has **no `HAVING SUM<>0`**: it emits a zero-balance
row for every `(account, currency)` with ANY split on-or-before the as-of date.
A currency with in-range activity therefore **always** appears at the CLOSING
endpoint (as-of `To`) with balance 0 — the in-range currency set is provably a
**subset** of the closing set, so `union(opening, closing)` already contains it
and its section renders. The p22.4 note assumed a net-zero currency is *absent*
from the closing map; it is actually *present as a zero row*. Any "union in the
range currencies" code would be a no-op (cannot bite on revert → violates the
TDD rule), so **no code change was made**. A labeled regression guard
(`TestAccountLedgerMidRangeOnlyCurrency`) pins the correct current behavior and
would catch a future `SubtreeBalancesAsOf` change that dropped zero rows.
`make golden` unchanged. See DECISIONS p22.5.

### 2.5 Expense-report reviewer: restricted-fund counter-side prefill — **OPEN (by design)**
`internal/web/expenses_review.go` — `prefillExpenseRows` (~L229); recorded in
DECISIONS p20.3.

Review & post prefills the p12 editor from the report lines and appends a
trailing empty cash/bank counter row. The counter row is left **unrestricted**.
If a report line carries a restricted fund, the reviewer must set the
counter-side's fund by hand; otherwise the store's per-fund zero-sum gate
(`ErrFundUnbalanced` → 422) rejects the post. This is **by design** — the store
validation is the gate, not a defect. Documented here for completeness; auto-
mirroring the fund onto the counter row is a possible future nicety, not backlog.

### 2.6 Budget line-form: subsidiary-change re-fetch loses in-progress entry — **OPEN (by design)**
`internal/web/templates/budget_line_form.tmpl` L10, L30; recorded in DECISIONS p19.3.

Changing the subsidiary on the budget-line editor re-fetches the form via hx-get
so the server re-filters the fund/account selectors to that subsidiary. The
re-fetch rebuilds the form from only the `subsidiary` param, so — unlike the txn
editor — it does **not** echo the user's in-progress account/fund/program/amount/
schedule. In practice the subsidiary is chosen first (it scopes the rest), so
re-selecting it mid-entry is rare; the store re-validates on submit, so there is
no correctness impact. Applying the txn editor's `hx-include`+echo pattern is a
small follow-up. Low-effort but UI-only and not correctness-bearing → see §5.

---

## 3. Open code markers (genuine TODO / FIXME still open)

Full grep of the codebase for `TODO`/`FIXME`/`XXX`/`HACK`/`deferred`/`stub`/
`not yet`/`unsupported`, reconciled. Only genuinely-open items are listed.

| Marker | Location | Status / note |
| --- | --- | --- |
| `TODO(p16.1): repoint reconciliations from src to dst` | `internal/store/merge.go` | **RESOLVED (p22.5): guarded.** TODO removed; the merge now refuses a reconciled source (`ErrMergeSourceReconciled`) rather than repointing. Full repoint stays BACKLOG (§2.2, §6). |
| merge doc comment (recon repoint) | `internal/store/merge.go` | **RESOLVED (p22.5):** rewritten to describe the block-guard; full repoint noted as backlog. |
| pending recon-repoint assertion | `internal/store/merge_test.go` | **RESOLVED (p22.5):** replaced by `TestMergeBlockedSourceReconciled` (the guard is now the tested behavior). |
| "0 reconciliations move (deferred to p16)" (preview text) | `internal/web/merge.go` | **RESOLVED (p22.5):** preview now shows the reconciled-split count that BLOCKS the merge, not "0 recons move". |

### Reconciled markers found but **NOT** open (resolved or non-issues)

- **p06.3 `_placeholder` report group** — **RESOLVED in p15.1.** `internal/reports/registry.go`
  now registers the real reports (L26, "p15.1 smoke report"); the `_placeholder`
  stub is gone. The remaining "placeholder" hits in `internal/reports/*.go` are
  the account-tree *placeholder parent* domain concept (subtotal rows), unrelated.
- **txngrid keyboard-grid wiring** — **RESOLVED in p12.6.** Wired in
  `internal/web/static/txneditor.js:247` ("keyboard grid (Appendix C, p12.6)").
- **`TODO(p05.2)` / `TODO(p07.3)` / `TODO(p08)` split-usage guards** — **RESOLVED in p08.2/p08.5.**
  DECISIONS p08.2 records "grep `TODO(p08)` in `internal/` now CLEAN"; the deferred
  removal guards were completed. The `-- deferred p08 guards` comment in
  `internal/db/queries/transactions.sql:293` labels the *implemented* queries.
- **Z8 / Z9 `sqlDeferred` placeholders** — **RESOLVED in p16.1.** `checks.go` L350/L370
  now carry the real Z8/Z9 SQL; the `sqlDeferred` stubs are gone.
- **`internal/store/merge_test.go:177/178` "XXX"** — non-marker: `XXX` is an
  unseeded currency code used to prove currency rejection, not a TODO tag.
- **`stub` in `render.go`, `shell.go`, `reports.go`, `rates/yahoo.go`** — design
  stubs (a no-op template `t` func, a never-called grant closure, a test transport
  seam), not deferred work.
- **`shell.go:254` "p10.2 stub … once the users/currencies pages landed"** — the
  referenced pages landed; the nav index is populated. Not open work.

---

## 4. Pre-cutover human gates

These are not code defects — they are manual steps a human MUST perform before a
real go-live. Do not treat them as automatable.

### 4.1 p09.4 go-live import mapping review — **OPEN (human gate)**
`docs/golive.md`, `make golive-check` (Makefile L48–60).

The historical CSV-ledger import was a **best-guess autonomous rehearsal**. The
account/fund/program mappings it produced are provisional and **must be reviewed
by a human** before a real cutover. `make golive-check` is the D26 strict gate
(runs `cuento check --strict` on the live db and is separate from routine
`make check`); it does not itself validate that the mappings are *semantically
right* — that is the human's judgment. Backlog until an actual cutover is scheduled.

### 4.2 Stale local `fixtures/sample.db` — **OPEN (regenerate before golive-check)**
`fixtures/sample.db` is gitignored (AGENTS layout) and, if present locally,
predates the p16 reconciliations migration (00014). It must be **regenerated via
`make fixture`** (re-run `ledgerimport`) before running `make golive-check`, or
the gate runs against a schema-stale db. Routine `make check` builds its own
fresh migrated temp db and does **not** depend on `sample.db`, so this only bites
the manual golive path.

### 4.3 Z19 unmapped-990 warnings on the best-guess import — **OPEN (by design, warning)**
`internal/ledger/checks.go` — Z19 (`sqlZ19`, ~L561).

Z19 is a **warning** (not an error): an active revenue/expense leaf with activity
but no effective Form-990 code. On the best-guess import, uncoded leaves surface
as Z19 warnings by design — they do not block `cuento check` (exit 0 unless
`--strict`). Under `make golive-check` (which is `--strict`) they DO fail the
gate, which is the intended forcing-function for the human to finish 990 coding
before cutover. This is a deliberate known-warning, not a bug.

---

## 5. Low-hanging-fruit candidates for p22.5

Honest triage. "Low-hanging" = small, localized, low-risk, self-contained test.
Ranked by recommended order (effort estimate in parentheses). The recommendation
was p22.5's input.

**p22.5 OUTCOME:** items 1–3 IMPLEMENTED (see §2.1, §2.2, and the resolved
markers in §3); item 4 INVESTIGATED and reclassified as a non-issue (§2.4 — the
drop cannot occur), no code change, a regression guard added. Items 5–6 remain
optional/by-design. The originally-drafted recommendations are kept below for the
record.

### Recommended for p22.5 (genuinely low-effort / low-risk)

1. **`reconciliations_versions` into Z3 and Z5** — *(S, ~30 min)* — **DONE (p22.5).**
   **VERIFIED GAP.** `reconciliations_versions` exists (00014) and is written by
   the store (`InsertReconciliationVersion`), but is **absent** from both integrity
   checks: `checks.go` Z3 enumerates subsidiaries/programs/accounts/account_names/
   account_subsidiaries/funds/fund_subsidiaries/budget_schedules/
   budget_schedule_dates/budgets/budget_lines/payees/transactions/splits/users/
   expense_reports/expense_report_lines — **no reconciliations**; Z5's UNION
   (L298–315) lists all those twins — **no `reconciliations_versions`**. The
   comparable p19.1 (budgets) and p20.1 (expense_reports) twins WERE wired in;
   reconciliations was missed. Fix: add one Z3 join block mirroring the
   `expense_reports` block (single-id twin, `op='update'` on reopen, no delete) and
   one `UNION ALL SELECT 'reconciliations_versions', id, change_id FROM
   reconciliations_versions` to Z5, plus a negative test. Pure integrity-coverage
   fix, no behavior change.

2. **Reopen-while-later-OPEN guard** — *(S, ~30–45 min)* — **DONE (p22.5), reusing `ErrOpenReconciliationExists`.**
   Add an "another OPEN recon exists on this `(account, currency)`" check to
   `Reopen` (§2.1), mirroring `StartReconciliation`'s `CountOpenReconciliations`
   (a new sentinel, e.g. reuse/extend `ErrOpenReconciliationExists`), plus a
   negative test. Note there is **no `cuento check` backstop** for two open recons,
   so this is a store-guard-only fix — worth doing precisely because nothing else
   catches it.

3. **Merge with recon'd splits — block-guard slice only** — *(S–M, ~1 hr)* — **DONE (p22.5); full repoint stays backlog (§5 "Real backlog").**
   NOT full repointing. Add a clean pre-write guard to `MergeAccount`: refuse the
   merge (typed sentinel, e.g. `ErrMergeReconciledSplits`) when src has any split
   with a non-NULL `reconciliation_id`. This converts today's two bad outcomes
   (§2.2: a raw SQLite trigger ABORT for finalized-recon splits, and a silent Z8
   integrity hole for open-recon splits) into one clean, tested rejection. Also
   fix the misleading web preview text ("0 reconciliations move"). Recommended
   only if the human agrees blocking is acceptable v1 behavior; the full repoint
   stays backlog.

4. **Account-ledger currency-in-range section** — *(S–M, ~1 hr incl. golden)* — **RECLASSIFIED (p22.5): non-issue, no code (§2.4).**
   The drafted fix (union the in-range `DrillSplits` currencies into the section
   set) is provably a NO-OP: `SubtreeBalancesAsOf` emits a zero-balance row for
   every in-range currency, so the closing set already contains it — the feared
   drop cannot occur. Shipped as a regression guard only, no code change, golden
   unchanged.

### Optional (cosmetic / by-design; defer unless the human wants them)

5. **Budget line-form in-progress echo** *(M, UI-only)* — §2.6. Apply the txn
   editor's `hx-include`+echo pattern. No correctness impact; pure UX nicety.
6. **Expense-review restricted-fund counter prefill** *(S, by design)* — §2.5.
   Auto-mirror the line's fund onto the counter row. Currently working-as-designed;
   only a convenience.

### Real backlog — do NOT attempt in p22.5

- **Full merge recon-repointing** (§2.2) — design-heavy (per-`(account,currency)`
  recon semantics, dst-overlap, opening chain). The §5.3 block-guard is the v1 answer.
- **EU decimal-comma amount parser** (§2.3) — a real locale parser branch + config + tests.
- **Everything in §1** (Phase 21 backlog) — intentional v1 non-goals. Per-subsidiary
  permissions and a holiday calendar in particular are large, not low-hanging.
- **p09.4 mapping review / stale sample.db** (§4) — human gates, not code.

---

## 6. Known non-blocking tooling finding

**govulncheck `x/crypto/openpgp` (GO-2026-5932)** — informational, non-blocking,
**unfixable without dropping a dependency**. `golang.org/x/crypto` is a required
module (pulled by `alexedwards/argon2id`, D15) but cuento never imports its
`openpgp` subpackage. govulncheck reports the module-level advisory
(GO-2026-5932, "unmaintained", Fixed-in: N/A) yet exits **0** with "code affected
by 0 vulnerabilities" because the vulnerable code is uncalled. It has ridden
along on every step's `make lint test check` since p06.1 and does **not** block
the DoD. Recorded in DECISIONS p06.1. No action available short of removing the
argon2id dep; not planned.
