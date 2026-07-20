-- p08.4: balance queries (read-only). These are the backbone of registers, fund
-- pages, program pages, and the report toolkit (Appendix E over these, p15.2).
-- All read via sqlc (rule 6); amounts stay int64 minor units (rule 3) -- every
-- SUM is CAST(... AS INTEGER) because sqlc's sqlite analyzer types a bare SUM as
-- REAL (sql.NullFloat64), and money must never become float. At runtime SQLite
-- returns the exact integer sum; the CAST only pins the STATIC type to int64.
--
-- SCOPE (D18): a chosen subsidiary consolidated with ALL its descendants. Every
-- scoped aggregate closes the descendant set with a recursive CTE over
-- subsidiaries (base case = the scope subsidiary itself, so a leaf sub scopes to
-- just itself) and joins transactions on subsidiary_id IN (that closure). sqlc
-- v1.31.1 DOES accept a recursive CTE referenced in an outer WHERE ... IN
-- (SELECT ...) here (verified by make gen), so no Go-side merge is needed.
--
-- Only NON-DELETED transactions count (t.deleted = 0) in EVERY query. Net-debit
-- signs (D2): debits +, credits -, so a bare SUM(amount) is the signed balance.
-- Unrestricted fund (D20, NULL fund_id) is represented as fund id 0 via
-- COALESCE(fund_id, 0) so the report layer's zero-FundID convention (Appendix E)
-- reads directly.
--
-- NOTE: keep every comment and identifier in this file PURE ASCII. sqlc v1.31.1
-- miscounts byte offsets on multi-byte UTF-8, corrupting the WHOLE file's
-- generated SQL (docs/DECISIONS.md p04.2).

-- name: SubtreeBalancesAsOf :many
-- Per (account, currency): cumulative signed balance of non-deleted splits whose
-- txn date <= asof and whose subsidiary is in the scope closure. Params: scopeSub
-- (CTE base), asof.
WITH RECURSIVE scope(id) AS (
  SELECT s.id FROM subsidiaries s WHERE s.id = ?
  UNION ALL
  SELECT s.id FROM subsidiaries s JOIN scope ON s.parent_id = scope.id
)
SELECT sp.account_id, t.currency, CAST(SUM(sp.amount) AS INTEGER) AS balance
FROM splits sp
JOIN transactions t ON t.id = sp.transaction_id
WHERE t.deleted = 0
  AND t.date <= ?
  AND t.subsidiary_id IN (SELECT id FROM scope)
GROUP BY sp.account_id, t.currency
ORDER BY sp.account_id, t.currency;

-- name: SubDatedBalancesAsOf :many
-- Per (subsidiary, account, currency, DATE): signed net-debit activity of the
-- non-deleted splits on that date, for every txn date <= asof whose subsidiary is in
-- the scope closure. Unlike SubtreeBalancesAsOf (which consolidates the whole subtree
-- into one balance per account/currency), this preserves BOTH the HOLDING subsidiary
-- (so the report knows each balance's functional currency = that sub's base_currency)
-- and the transaction DATE (so the FX toolkit can value each dated flow at its own
-- transaction-date rate for the ASC 830-20 remeasurement basis, while the summed
-- balance values at the closing rate). Params: scopeSub (CTE base), asof. p31.1.
WITH RECURSIVE scope(id) AS (
  SELECT s.id FROM subsidiaries s WHERE s.id = ?
  UNION ALL
  SELECT s.id FROM subsidiaries s JOIN scope ON s.parent_id = scope.id
)
SELECT t.subsidiary_id, sp.account_id, t.currency, t.date,
       CAST(SUM(sp.amount) AS INTEGER) AS activity
FROM splits sp
JOIN transactions t ON t.id = sp.transaction_id
WHERE t.deleted = 0
  AND t.date <= ?
  AND t.subsidiary_id IN (SELECT id FROM scope)
GROUP BY t.subsidiary_id, sp.account_id, t.currency, t.date
ORDER BY t.subsidiary_id, sp.account_id, t.currency, t.date;

-- name: PeriodActivity :many
-- Per (account, currency): signed activity over from <= date <= to in scope.
-- Params: scopeSub, from, to.
WITH RECURSIVE scope(id) AS (
  SELECT s.id FROM subsidiaries s WHERE s.id = ?
  UNION ALL
  SELECT s.id FROM subsidiaries s JOIN scope ON s.parent_id = scope.id
)
SELECT sp.account_id, t.currency, CAST(SUM(sp.amount) AS INTEGER) AS activity
FROM splits sp
JOIN transactions t ON t.id = sp.transaction_id
WHERE t.deleted = 0
  AND t.date >= ?
  AND t.date <= ?
  AND t.subsidiary_id IN (SELECT id FROM scope)
GROUP BY sp.account_id, t.currency
ORDER BY sp.account_id, t.currency;

-- name: FundBalancesAsOf :many
-- Per (fund, currency): the fund's cumulative unexpended balance to asof in scope,
-- INCLUDING the unrestricted group (NULL fund_id -> fund id 0 via COALESCE).
--
-- ASSET- side sum only (WHERE a.type = 'asset'). Every transaction nets to zero
-- WITHIN each fund group (D20/Z10), and scoping is by whole transactions (one sub,
-- one date each), so a sum over ALL of a fund's splits is IDENTICALLY zero for
-- every (fund, currency) -- it would measure nothing. Restricting to asset
-- accounts yields the fund's cash/asset position = unexpended restricted resources
-- (a grant received then partly spent shows the remaining balance). This is the
-- Z18 precedent (docs/DECISIONS.md p08.3) applied to the balance read; recorded as
-- p08.4. Params: scopeSub, asof.
WITH RECURSIVE scope(id) AS (
  SELECT s.id FROM subsidiaries s WHERE s.id = ?
  UNION ALL
  SELECT s.id FROM subsidiaries s JOIN scope ON s.parent_id = scope.id
)
SELECT COALESCE(sp.fund_id, 0) AS fund_id, t.currency,
       CAST(SUM(sp.amount) AS INTEGER) AS balance
FROM splits sp
JOIN transactions t ON t.id = sp.transaction_id
JOIN accounts a ON a.id = sp.account_id
WHERE t.deleted = 0
  AND a.type = 'asset'
  AND t.date <= ?
  AND t.subsidiary_id IN (SELECT id FROM scope)
GROUP BY COALESCE(sp.fund_id, 0), t.currency
ORDER BY fund_id, t.currency;

-- name: CurrentCashFundBalancesAsOf :many
-- Per (fund, currency): the fund's cumulative CASH-AVAILABLE balance to asof in
-- scope, restricted to accounts flagged current_cash (p27.1) -- the cash-flow
-- projection's PER-FUND opening base (DECISIONS "Budget redesign", p27.3). Mirrors
-- FundBalancesAsOf exactly but filters a.current_cash = 1 instead of a.type =
-- 'asset': spendable cash is a strict subset of the fund's asset position (it
-- excludes receivables and capitalized non-cash assets -- cf. p26.94), which is
-- what "opening cash available" means. INCLUDES the unrestricted group (NULL
-- fund_id -> fund id 0 via COALESCE, D20). Params: scopeSub, asof.
WITH RECURSIVE scope(id) AS (
  SELECT s.id FROM subsidiaries s WHERE s.id = ?
  UNION ALL
  SELECT s.id FROM subsidiaries s JOIN scope ON s.parent_id = scope.id
)
SELECT COALESCE(sp.fund_id, 0) AS fund_id, t.currency,
       CAST(SUM(sp.amount) AS INTEGER) AS balance
FROM splits sp
JOIN transactions t ON t.id = sp.transaction_id
JOIN accounts a ON a.id = sp.account_id
WHERE t.deleted = 0
  AND a.current_cash = 1
  AND t.date <= ?
  AND t.subsidiary_id IN (SELECT id FROM scope)
GROUP BY COALESCE(sp.fund_id, 0), t.currency
ORDER BY fund_id, t.currency;

-- name: FunctionalActivity :many
-- Per (expense account, functional_class, currency): signed activity over the
-- period in scope. Only expense splits carry a class (D21), so the NOT NULL filter
-- restricts to exactly the expense splits. Params: scopeSub, from, to.
WITH RECURSIVE scope(id) AS (
  SELECT s.id FROM subsidiaries s WHERE s.id = ?
  UNION ALL
  SELECT s.id FROM subsidiaries s JOIN scope ON s.parent_id = scope.id
)
SELECT sp.account_id, sp.functional_class, t.currency,
       CAST(SUM(sp.amount) AS INTEGER) AS activity
FROM splits sp
JOIN transactions t ON t.id = sp.transaction_id
WHERE t.deleted = 0
  AND sp.functional_class IS NOT NULL
  AND t.date >= ?
  AND t.date <= ?
  AND t.subsidiary_id IN (SELECT id FROM scope)
GROUP BY sp.account_id, sp.functional_class, t.currency
ORDER BY sp.account_id, sp.functional_class, t.currency;

-- name: FunctionalActivityByProgram :many
-- Per (expense account, functional_class, program, currency): signed activity over
-- the period in scope. Like FunctionalActivity but ALSO groups by program_id -- the
-- p27.4 SCOPED variant: a program-scoped report grant filters the functional-expense
-- matrix to the granted program subtree BEFORE rolling classes up, so a sibling
-- subtree's expense never contributes to a functional line. Expense splits carry BOTH
-- a class (D21) and a program (D24), so the NOT NULL filters restrict to exactly the
-- expense activity, keyed additionally by program. This query is used ONLY on the
-- scoped path (ProgramScope set); the unscoped path keeps FunctionalActivity untouched
-- (so the goldens do not move). Params: scopeSub, from, to.
WITH RECURSIVE scope(id) AS (
  SELECT s.id FROM subsidiaries s WHERE s.id = ?
  UNION ALL
  SELECT s.id FROM subsidiaries s JOIN scope ON s.parent_id = scope.id
)
SELECT sp.account_id, sp.functional_class, sp.program_id, t.currency,
       CAST(SUM(sp.amount) AS INTEGER) AS activity
FROM splits sp
JOIN transactions t ON t.id = sp.transaction_id
WHERE t.deleted = 0
  AND sp.functional_class IS NOT NULL
  AND sp.program_id IS NOT NULL
  AND t.date >= ?
  AND t.date <= ?
  AND t.subsidiary_id IN (SELECT id FROM scope)
GROUP BY sp.account_id, sp.functional_class, sp.program_id, t.currency
ORDER BY sp.account_id, sp.functional_class, sp.program_id, t.currency;

-- name: ProgramActivity :many
-- Per (program, account, currency): signed activity over the period in scope. Only
-- revenue/expense splits carry a program (D24), so the NOT NULL filter restricts to
-- exactly the R/E splits. Returned per (program, account) raw -- the tree rollup is
-- the report layer's job. Params: scopeSub, from, to.
WITH RECURSIVE scope(id) AS (
  SELECT s.id FROM subsidiaries s WHERE s.id = ?
  UNION ALL
  SELECT s.id FROM subsidiaries s JOIN scope ON s.parent_id = scope.id
)
SELECT sp.program_id, sp.account_id, t.currency,
       CAST(SUM(sp.amount) AS INTEGER) AS activity
FROM splits sp
JOIN transactions t ON t.id = sp.transaction_id
WHERE t.deleted = 0
  AND sp.program_id IS NOT NULL
  AND t.date >= ?
  AND t.date <= ?
  AND t.subsidiary_id IN (SELECT id FROM scope)
GROUP BY sp.program_id, sp.account_id, t.currency
ORDER BY sp.program_id, sp.account_id, t.currency;

-- name: FundPeriodActivity :many
-- Per (account, program, currency): signed activity over from <= date <= to in
-- scope, restricted to ONE fund (sp.fund_id = ?). It is the fund-FILTERED variant
-- of PeriodActivity, used by the Statement of Activities' FUND selector (p15.5): a
-- fund-scoped income statement lists only the revenue/expense flows tagged that
-- fund. The program_id column is kept (nullable) so the SAME row set can ALSO be
-- narrowed to a program subtree in the report layer (InProgramScope) when a user
-- picks BOTH a fund and a program -- one query serves both filters. Only a real fund
-- (>0) is ever a valid selection (fund 0 is the synthetic unrestricted group,
-- list-only), so the exact-match sp.fund_id = ? never needs a NULL branch. Non-R/E
-- splits of the fund (its asset/liability legs) are returned too but the caller keeps
-- only R/E accounts, exactly as it does over PeriodActivity. Params: scopeSub, from,
-- to, fund_id.
WITH RECURSIVE scope(id) AS (
  SELECT s.id FROM subsidiaries s WHERE s.id = ?
  UNION ALL
  SELECT s.id FROM subsidiaries s JOIN scope ON s.parent_id = scope.id
)
SELECT sp.account_id, sp.program_id, t.currency,
       CAST(SUM(sp.amount) AS INTEGER) AS activity
FROM splits sp
JOIN transactions t ON t.id = sp.transaction_id
WHERE t.deleted = 0
  AND sp.fund_id = ?
  AND t.date >= ?
  AND t.date <= ?
  AND t.subsidiary_id IN (SELECT id FROM scope)
GROUP BY sp.account_id, sp.program_id, t.currency
ORDER BY sp.account_id, sp.program_id, t.currency;

-- name: FundSubtreeBalancesAsOf :many
-- Per (account, currency): cumulative signed balance of ONE fund's non-deleted
-- splits whose txn date <= asof in scope (sp.fund_id = ?). It is the fund-FILTERED
-- variant of SubtreeBalancesAsOf, the source for the Statement of Position's FUND
-- selector (p15.4): a fund-scoped balance sheet lists that fund's OWN assets,
-- liabilities, and net assets. Because every transaction nets to zero WITHIN a fund
-- group (D20/Z10), the fund's Assets - Liabilities equals its net-asset (fund)
-- balance, so the balance-sheet identity A = L + NA holds for the single fund exactly
-- as it does org-wide. Only a real fund (>0) is ever selected. Params: scopeSub,
-- fund_id, asof.
WITH RECURSIVE scope(id) AS (
  SELECT s.id FROM subsidiaries s WHERE s.id = ?
  UNION ALL
  SELECT s.id FROM subsidiaries s JOIN scope ON s.parent_id = scope.id
)
SELECT sp.account_id, t.currency, CAST(SUM(sp.amount) AS INTEGER) AS balance
FROM splits sp
JOIN transactions t ON t.id = sp.transaction_id
WHERE t.deleted = 0
  AND sp.fund_id = ?
  AND t.date <= ?
  AND t.subsidiary_id IN (SELECT id FROM scope)
GROUP BY sp.account_id, t.currency
ORDER BY sp.account_id, t.currency;

-- name: BudgetKeyActivity :many
-- Per (subsidiary, account, fund, program, currency, date): signed net-debit
-- activity of the revenue/expense splits over from <= date <= to in scope. This is
-- the ACTUALS grain the budget toolkit (p19.2) compares against a budget, keyed by
-- the SAME (sub, account, fund, program) tuple a budget line carries -- with the
-- unrestricted group as fund id 0 (COALESCE, matching FundBalancesAsOf / D20) and
-- the DATE preserved so the caller can bucket each split by its own date (the same
-- discrete no-pro-rata bucketing occurrences use). Only R/E splits carry a program
-- (D24), so the NOT NULL program filter restricts to exactly the budgetable flows.
-- Params: scopeSub, from, to.
WITH RECURSIVE scope(id) AS (
  SELECT s.id FROM subsidiaries s WHERE s.id = ?
  UNION ALL
  SELECT s.id FROM subsidiaries s JOIN scope ON s.parent_id = scope.id
)
SELECT t.subsidiary_id, sp.account_id, COALESCE(sp.fund_id, 0) AS fund_id,
       sp.program_id, t.currency, t.date,
       CAST(SUM(sp.amount) AS INTEGER) AS activity
FROM splits sp
JOIN transactions t ON t.id = sp.transaction_id
WHERE t.deleted = 0
  AND sp.program_id IS NOT NULL
  AND t.date >= ?
  AND t.date <= ?
  AND t.subsidiary_id IN (SELECT id FROM scope)
GROUP BY t.subsidiary_id, sp.account_id, COALESCE(sp.fund_id, 0),
         sp.program_id, t.currency, t.date
ORDER BY t.subsidiary_id, sp.account_id, fund_id, sp.program_id, t.currency, t.date;

-- name: RegisterPage :many
-- The account register: every non-deleted split on account_id OR any of its
-- descendant accounts (p26.6 parent-account rollup), after filters, with a RUNNING
-- BALANCE per currency computed by a window function over the WHOLE filtered set (a
-- single account is usually one currency, but FX Clearing is multi -- partition by
-- currency). The WINDOW that computes the running balance stays ASCENDING (date,
-- split_id) so each row's running_balance is the cumulative total from the OLDEST
-- split up to that row; only the terminal display ORDER BY is DESCENDING so the
-- register shows the NEWEST transaction on top (p26.9), the top row carrying the
-- latest balance. split_id is globally unique + monotonic, so (date, split_id) is a
-- total order needing no txn-id tiebreak; the same tuple is the window ORDER (asc),
-- the final display ORDER BY (desc), and the keyset cursor tuple.
--
-- ROLLUP (p26.6): the account_id predicate is a recursive descendant closure over
-- the account tree (des CTE, base = the requested account itself, mirroring
-- AccountDescendants). A LEAF's closure is just {itself}, so its register is
-- IDENTICAL to before (same split set, order, running balance -- leaf reconciliation
-- preserved). A PLACEHOLDER (parent) closes over all its descendants, so the window
-- accumulates ONE combined running balance across the merged descendant sequence.
-- Parents cannot hold their own splits (ErrPlaceholderAccount + ledger Z2), so no
-- parent-level split double-counts. Each row carries its OWN account_id so the
-- handler resolves the counter-account against the actual leaf, not the parent.
-- sqlc v1.31.1 accepts a recursive CTE referenced in an inner WHERE ... IN
-- (SELECT ...) (same pattern as DrillSplits / the scope CTEs above).
--
-- KEYSET PAGING IS APPLIED IN GO, not in SQL: the running balance must be computed
-- by the window over the ENTIRE filtered set BEFORE the cursor cuts a page (or
-- page 2's first running balance would restart instead of continuing). sqlc
-- v1.31.1's sqlite analyzer resolves a windowed-CTE's DERIVED columns only in a
-- terminal SELECT/ORDER BY projection -- it rejects referencing them in an outer
-- WHERE (verified: "column split_id does not exist"), so the seek predicate
-- (date, split_id) > (cursor) cannot live in SQL over this CTE. The store runs
-- this query (full filtered set, window-computed, ordered), then seeks past the
-- cursor and takes limit+1 rows in Go. The window still runs in SQL over the whole
-- set, so the running balance is exact and stable across pages. (docs/DECISIONS.md
-- p08.4.)
--
-- Filters are optional; each is passed as a PAIR of positional ? (a "? IS NULL OR
-- ..." guard reuses a param, which sqlc's sqlite parser mangles -- p04.2 quirk --
-- so the store binds each filter value to TWO consecutive ? instead; the text
-- filter binds the wrapped %..% pattern to THREE ? for memo/split-memo/description).
--
-- Param order (positional):
--   account_id (des CTE base),
--   fromActive, from, toActive, to,
--   textActive, text, text, text,
--   fundActive, fund, subActive, sub, programActive, program.
WITH RECURSIVE des(id) AS (
  SELECT a.id FROM accounts a WHERE a.id = ?
  UNION ALL
  SELECT a.id FROM accounts a JOIN des ON a.parent_id = des.id
),
filtered AS (
  SELECT sp.id AS split_id, t.id AS txn_id, t.date, t.subsidiary_id,
         t.currency, sp.account_id, sp.amount, sp.fund_id, sp.program_id,
         sp.functional_class, sp.memo AS split_memo, t.memo AS txn_memo,
         sp.description,
         CAST(SUM(sp.amount) OVER (
           PARTITION BY t.currency
           ORDER BY t.date, sp.id
           ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
         ) AS INTEGER) AS running_balance
  FROM splits sp
  JOIN transactions t ON t.id = sp.transaction_id
  WHERE sp.account_id IN (SELECT id FROM des)
    AND t.deleted = 0
    AND (? = 0 OR t.date >= ?)
    AND (? = 0 OR t.date <= ?)
    AND (? = 0 OR (t.memo LIKE ? OR sp.memo LIKE ? OR sp.description LIKE ?))
    AND (? = 0 OR sp.fund_id = ?)
    AND (? = 0 OR t.subsidiary_id = ?)
    AND (? = 0 OR sp.program_id = ?)
)
SELECT split_id, txn_id, date, subsidiary_id, currency, account_id, amount, fund_id,
       program_id, functional_class, split_memo, txn_memo, description,
       running_balance
FROM filtered
ORDER BY date DESC, split_id DESC;

-- name: DrillSplits :many
-- The report DRILL-DOWN (p15.3d): every non-deleted split contributing to ONE
-- report figure, so the caller's signed sum reconciles to that figure. This
-- MIRRORS SubtreeBalancesAsOf / PeriodActivity (the recursive scope descendant
-- closure over subsidiaries, D18; t.deleted = 0) rather than the register (which
-- filters ONE subsidiary by equality) -- a root-scope trial-balance cell sums an
-- account across every descendant sub, so the drill must too. The rows are the raw
-- ungrouped splits (NOT summed) ordered by (date, split_id) so the drill shows the
-- individual transactions; the store sums their signed amounts to prove the
-- reconciliation invariant.
--
-- Filters (each optional, paired-? active-flag trick like RegisterPage -- a reused
-- "? IS NULL OR ..." param is mangled by sqlc's sqlite parser, p04.2, so each value
-- is bound to TWO consecutive ?):
--   account_id (required equality; the drill targets one leaf account per cell),
--   currency (required equality; each toolkit cell is per-currency, so FX Clearing's
--     MXN cell reconciles only when currency is filtered),
--   asofActive/asof (t.date <= asof, for an as-of cumulative figure),
--   fromActive/from + toActive/to (from <= t.date <= to, for a period figure),
--   fundActive/fund, programActive/program, classActive/class (optional narrowing).
--
-- Param order (positional):
--   scopeSub (CTE base),
--   account_id,
--   currency,
--   asofActive, asof,
--   fromActive, from, toActive, to,
--   fundActive, fund, programActive, program, classActive, class.
WITH RECURSIVE scope(id) AS (
  SELECT s.id FROM subsidiaries s WHERE s.id = ?
  UNION ALL
  SELECT s.id FROM subsidiaries s JOIN scope ON s.parent_id = scope.id
)
SELECT sp.id AS split_id, t.id AS txn_id, t.date, t.subsidiary_id, t.currency,
       sp.amount, sp.fund_id, sp.program_id, sp.functional_class,
       sp.memo AS split_memo, t.memo AS txn_memo, sp.description
FROM splits sp
JOIN transactions t ON t.id = sp.transaction_id
WHERE t.deleted = 0
  AND sp.account_id = ?
  AND t.currency = ?
  AND t.subsidiary_id IN (SELECT id FROM scope)
  AND (? = 0 OR t.date <= ?)
  AND (? = 0 OR t.date >= ?)
  AND (? = 0 OR t.date <= ?)
  AND (? = 0 OR sp.fund_id = ?)
  AND (? = 0 OR sp.program_id = ?)
  AND (? = 0 OR sp.functional_class = ?)
ORDER BY t.date, sp.id;
