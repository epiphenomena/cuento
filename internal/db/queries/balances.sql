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

-- name: RegisterPage :many
-- The account register: every non-deleted split on account_id (after filters),
-- with a RUNNING BALANCE per currency computed by a window function over the WHOLE
-- filtered set (a single account is usually one currency, but FX Clearing is
-- multi -- partition by currency), ordered ascending by the total order
-- (date, split_id). split_id is globally unique + monotonic, so (date, split_id)
-- is a total order needing no txn-id tiebreak; the same tuple is the window ORDER,
-- the final ORDER BY, and the keyset cursor tuple.
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
-- filter binds the wrapped %..% pattern to THREE ? for memo/split-memo/payee).
--
-- Param order (positional):
--   account_id,
--   fromActive, from, toActive, to,
--   textActive, text, text, text,
--   fundActive, fund, subActive, sub, programActive, program.
WITH filtered AS (
  SELECT sp.id AS split_id, t.id AS txn_id, t.date, t.subsidiary_id,
         t.currency, sp.amount, sp.fund_id, sp.program_id, sp.functional_class,
         sp.memo AS split_memo, t.memo AS txn_memo, t.payee_id,
         CAST(SUM(sp.amount) OVER (
           PARTITION BY t.currency
           ORDER BY t.date, sp.id
           ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
         ) AS INTEGER) AS running_balance
  FROM splits sp
  JOIN transactions t ON t.id = sp.transaction_id
  LEFT JOIN payees py ON py.id = t.payee_id
  WHERE sp.account_id = ?
    AND t.deleted = 0
    AND (? = 0 OR t.date >= ?)
    AND (? = 0 OR t.date <= ?)
    AND (? = 0 OR (t.memo LIKE ? OR sp.memo LIKE ? OR py.name LIKE ?))
    AND (? = 0 OR sp.fund_id = ?)
    AND (? = 0 OR t.subsidiary_id = ?)
    AND (? = 0 OR sp.program_id = ?)
)
SELECT split_id, txn_id, date, subsidiary_id, currency, amount, fund_id,
       program_id, functional_class, split_memo, txn_memo, payee_id,
       running_balance
FROM filtered
ORDER BY date, split_id;
