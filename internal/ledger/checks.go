package ledger

// The reviewed integrity-check SQL (AGENTS rule 6: const strings in this package
// are the sanctioned exception to sqlc-only; they are static and reviewed here as
// a set). Every query returns ONE text column (a detail naming the offending
// id(s)) and one row per violation; no rows means the rule passed.
//
// Conventions used throughout:
//   - "non-deleted transaction" = transactions.deleted = 0; a soft-deleted txn
//     (rule 14) is out of the ledger and out of every balance/scope check.
//   - NULL-safe comparison uses SQLite's `IS` / `IS NOT`, so a nullable column
//     (fund_id, program_id, functional_class, parent_id, payee_id...) compares
//     correctly whether or not it is NULL.
//   - "latest version" of an entity = its *_versions row with the greatest
//     (valid_from, id) -- the same (valid_from DESC, id DESC) tiebreak the store's
//     as-of reconstruction and testutil.AssertVersioned use, so Z3 agrees with
//     the store's own notion of "current snapshot".

// --- Z1: every non-deleted transaction sums to zero (D2) ---------------------
// Group splits by transaction; a non-deleted txn whose split amounts do not net
// to 0 in its (single) currency is a violation.
const sqlZ1 = `
SELECT 'transaction ' || CAST(t.id AS TEXT) || ' sums to ' || CAST(SUM(s.amount) AS TEXT)
FROM transactions t
JOIN splits s ON s.transaction_id = t.id
WHERE t.deleted = 0
GROUP BY t.id
HAVING SUM(s.amount) <> 0`

// --- Z2: splits reference leaf accounts (D11) --------------------------------
// A split whose account has ANY child account is on a placeholder -- a violation.
const sqlZ2 = `
SELECT 'split ' || CAST(s.id AS TEXT) || ' on placeholder account ' || CAST(s.account_id AS TEXT)
FROM splits s
WHERE EXISTS (SELECT 1 FROM accounts c WHERE c.parent_id = s.account_id)`

// --- Z3: each current row equals its latest version snapshot (D4) ------------
// For every versioned table, a current live row is a violation when its latest
// version (max valid_from, id) is MISSING, is op='delete', or differs in any
// business column. Composite-key tables (account_names, account_subsidiaries,
// fund_subsidiaries) match the version on the composite identity; membership
// tables (account_subsidiaries, fund_subsidiaries) have no 'update' op, so a live
// membership whose latest version is 'delete' or missing is the violation, and a
// version whose latest op is 'create' but has no live row is a dangling snapshot.
//
// The per-table blocks are UNION ALL'd; each yields details like 'accounts:5'.
// Every block finds the latest version via a correlated-id subquery so the
// (valid_from, id) tiebreak is explicit and identical everywhere.
const sqlZ3 = `
-- subsidiaries
SELECT 'subsidiaries:' || CAST(c.id AS TEXT)
FROM subsidiaries c
LEFT JOIN subsidiaries_versions v
  ON v.id = (SELECT id FROM subsidiaries_versions x WHERE x.entity_id = c.id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
   OR v.parent_id IS NOT c.parent_id OR v.name IS NOT c.name
   OR v.base_currency IS NOT c.base_currency OR v.active IS NOT c.active
   OR v.sort_order IS NOT c.sort_order
UNION ALL
-- programs
SELECT 'programs:' || CAST(c.id AS TEXT)
FROM programs c
LEFT JOIN programs_versions v
  ON v.id = (SELECT id FROM programs_versions x WHERE x.entity_id = c.id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
   OR v.parent_id IS NOT c.parent_id OR v.name IS NOT c.name
   OR v.active IS NOT c.active OR v.sort_order IS NOT c.sort_order
UNION ALL
-- accounts
SELECT 'accounts:' || CAST(c.id AS TEXT)
FROM accounts c
LEFT JOIN accounts_versions v
  ON v.id = (SELECT id FROM accounts_versions x WHERE x.entity_id = c.id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
   OR v.parent_id IS NOT c.parent_id OR v.type IS NOT c.type
   OR v.default_currency IS NOT c.default_currency
   OR v.functional_class IS NOT c.functional_class
   OR v.default_program_id IS NOT c.default_program_id
   OR v.form990_code IS NOT c.form990_code
   OR v.intercompany IS NOT c.intercompany OR v.reconcilable IS NOT c.reconcilable
   OR v.active IS NOT c.active OR v.sort_order IS NOT c.sort_order
   OR v.created_at IS NOT c.created_at
UNION ALL
-- account_names (composite: account_id + lang)
SELECT 'account_names:' || CAST(c.account_id AS TEXT) || '/' || c.lang
FROM account_names c
LEFT JOIN account_names_versions v
  ON v.id = (SELECT id FROM account_names_versions x
             WHERE x.entity_id = c.account_id AND x.lang = c.lang
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete' OR v.name IS NOT c.name
UNION ALL
-- account_subsidiaries (composite membership: account_id + subsidiary_id)
SELECT 'account_subsidiaries:' || CAST(c.account_id AS TEXT) || '/' || CAST(c.subsidiary_id AS TEXT)
FROM account_subsidiaries c
LEFT JOIN account_subsidiaries_versions v
  ON v.id = (SELECT id FROM account_subsidiaries_versions x
             WHERE x.entity_id = c.account_id AND x.subsidiary_id = c.subsidiary_id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
UNION ALL
-- account_subsidiaries: a membership whose latest version is 'create' but that
-- has no live row is a dangling snapshot (deleted live without a delete version).
SELECT 'account_subsidiaries(dangling):' || CAST(v.entity_id AS TEXT) || '/' || CAST(v.subsidiary_id AS TEXT)
FROM account_subsidiaries_versions v
WHERE v.id = (SELECT id FROM account_subsidiaries_versions x
              WHERE x.entity_id = v.entity_id AND x.subsidiary_id = v.subsidiary_id
              ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
  AND v.op <> 'delete'
  AND NOT EXISTS (SELECT 1 FROM account_subsidiaries c
                  WHERE c.account_id = v.entity_id AND c.subsidiary_id = v.subsidiary_id)
UNION ALL
-- funds
SELECT 'funds:' || CAST(c.id AS TEXT)
FROM funds c
LEFT JOIN funds_versions v
  ON v.id = (SELECT id FROM funds_versions x WHERE x.entity_id = c.id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
   OR v.name IS NOT c.name OR v.funder IS NOT c.funder OR v.purpose IS NOT c.purpose
   OR v.restriction IS NOT c.restriction OR v.program_id IS NOT c.program_id
   OR v.start_date IS NOT c.start_date OR v.end_date IS NOT c.end_date
   OR v.notes IS NOT c.notes OR v.active IS NOT c.active
UNION ALL
-- fund_subsidiaries (composite membership: fund_id + subsidiary_id)
SELECT 'fund_subsidiaries:' || CAST(c.fund_id AS TEXT) || '/' || CAST(c.subsidiary_id AS TEXT)
FROM fund_subsidiaries c
LEFT JOIN fund_subsidiaries_versions v
  ON v.id = (SELECT id FROM fund_subsidiaries_versions x
             WHERE x.entity_id = c.fund_id AND x.subsidiary_id = c.subsidiary_id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
UNION ALL
SELECT 'fund_subsidiaries(dangling):' || CAST(v.entity_id AS TEXT) || '/' || CAST(v.subsidiary_id AS TEXT)
FROM fund_subsidiaries_versions v
WHERE v.id = (SELECT id FROM fund_subsidiaries_versions x
              WHERE x.entity_id = v.entity_id AND x.subsidiary_id = v.subsidiary_id
              ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
  AND v.op <> 'delete'
  AND NOT EXISTS (SELECT 1 FROM fund_subsidiaries c
                  WHERE c.fund_id = v.entity_id AND c.subsidiary_id = v.subsidiary_id)
UNION ALL
-- budget_schedules (p19.1 single-id twin)
SELECT 'budget_schedules:' || CAST(c.id AS TEXT)
FROM budget_schedules c
LEFT JOIN budget_schedules_versions v
  ON v.id = (SELECT id FROM budget_schedules_versions x WHERE x.entity_id = c.id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
   OR v.name IS NOT c.name OR v.kind IS NOT c.kind
   OR v.day_of_month IS NOT c.day_of_month OR v.day_of_month_2 IS NOT c.day_of_month_2
   OR v.ordinal IS NOT c.ordinal OR v.weekday IS NOT c.weekday
   OR v.anchor_date IS NOT c.anchor_date OR v.weekend_adjust IS NOT c.weekend_adjust
   OR v.notes IS NOT c.notes
UNION ALL
-- budget_schedule_dates (composite membership: schedule_id + occurs_on)
SELECT 'budget_schedule_dates:' || CAST(c.schedule_id AS TEXT) || '/' || c.occurs_on
FROM budget_schedule_dates c
LEFT JOIN budget_schedule_dates_versions v
  ON v.id = (SELECT id FROM budget_schedule_dates_versions x
             WHERE x.entity_id = c.schedule_id AND x.occurs_on = c.occurs_on
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
UNION ALL
-- budget_schedule_dates: a list row whose latest version is 'create' but that has
-- no live row is a dangling snapshot (deleted live without a delete version).
SELECT 'budget_schedule_dates(dangling):' || CAST(v.entity_id AS TEXT) || '/' || v.occurs_on
FROM budget_schedule_dates_versions v
WHERE v.id = (SELECT id FROM budget_schedule_dates_versions x
              WHERE x.entity_id = v.entity_id AND x.occurs_on = v.occurs_on
              ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
  AND v.op <> 'delete'
  AND NOT EXISTS (SELECT 1 FROM budget_schedule_dates c
                  WHERE c.schedule_id = v.entity_id AND c.occurs_on = v.occurs_on)
UNION ALL
-- budgets (p19.1 single-id twin)
SELECT 'budgets:' || CAST(c.id AS TEXT)
FROM budgets c
LEFT JOIN budgets_versions v
  ON v.id = (SELECT id FROM budgets_versions x WHERE x.entity_id = c.id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
   OR v.name IS NOT c.name OR v.period_start IS NOT c.period_start
   OR v.period_end IS NOT c.period_end OR v.notes IS NOT c.notes
UNION ALL
-- budget_lines (p19.1 single-id twin; a line can be hard-deleted, so a missing
-- live row for a 'delete' version is legitimate -- handled by the standard
-- single-id block, which only checks CURRENT live rows against their snapshot).
SELECT 'budget_lines:' || CAST(c.id AS TEXT)
FROM budget_lines c
LEFT JOIN budget_lines_versions v
  ON v.id = (SELECT id FROM budget_lines_versions x WHERE x.entity_id = c.id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
   OR v.budget_id IS NOT c.budget_id OR v.subsidiary_id IS NOT c.subsidiary_id
   OR v.account_id IS NOT c.account_id OR v.fund_id IS NOT c.fund_id
   OR v.program_id IS NOT c.program_id OR v.amount IS NOT c.amount
   OR v.currency IS NOT c.currency OR v.schedule_id IS NOT c.schedule_id
UNION ALL
-- payees
SELECT 'payees:' || CAST(c.id AS TEXT)
FROM payees c
LEFT JOIN payees_versions v
  ON v.id = (SELECT id FROM payees_versions x WHERE x.entity_id = c.id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
   OR v.name IS NOT c.name OR v.active IS NOT c.active
UNION ALL
-- transactions: a soft-deleted txn's latest version IS op='delete' by design, so
-- only compare the business columns and treat a missing version as the violation
-- (a live deleted=1 row correctly has a 'delete' latest version -- NOT a Z3 miss).
SELECT 'transactions:' || CAST(c.id AS TEXT)
FROM transactions c
LEFT JOIN transactions_versions v
  ON v.id = (SELECT id FROM transactions_versions x WHERE x.entity_id = c.id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL
   OR v.date IS NOT c.date OR v.subsidiary_id IS NOT c.subsidiary_id
   OR v.payee_id IS NOT c.payee_id OR v.memo IS NOT c.memo
   OR v.currency IS NOT c.currency OR v.deleted IS NOT c.deleted
UNION ALL
-- splits: soft-deleting a transaction leaves its splits live but writes NO split
-- delete-version (p08.2), so the latest split version legitimately stays 'create'
-- /'update' on a deleted txn. Compare business columns; a missing version is the
-- violation. (A split removed by an edit is DELETEd live with a 'delete' version,
-- so it has no live row to check here.)
SELECT 'splits:' || CAST(c.id AS TEXT)
FROM splits c
LEFT JOIN splits_versions v
  ON v.id = (SELECT id FROM splits_versions x WHERE x.entity_id = c.id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
   OR v.transaction_id IS NOT c.transaction_id OR v.account_id IS NOT c.account_id
   OR v.amount IS NOT c.amount OR v.fund_id IS NOT c.fund_id
   OR v.program_id IS NOT c.program_id
   OR v.functional_class IS NOT c.functional_class
   OR v.memo IS NOT c.memo OR v.position IS NOT c.position
UNION ALL
-- users (password_hash is NEVER in the version snapshot, rule 5 -- excluded here).
SELECT 'users:' || CAST(c.id AS TEXT)
FROM users c
LEFT JOIN users_versions v
  ON v.id = (SELECT id FROM users_versions x WHERE x.entity_id = c.id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
   OR v.username IS NOT c.username OR v.display_name IS NOT c.display_name
   OR v.created_at IS NOT c.created_at OR v.disabled_at IS NOT c.disabled_at
   OR v.is_admin IS NOT c.is_admin OR v.txn_perm IS NOT c.txn_perm
   OR v.locale IS NOT c.locale OR v.date_format IS NOT c.date_format
   OR v.number_format IS NOT c.number_format OR v.display_mode IS NOT c.display_mode
   OR v.neg_style IS NOT c.neg_style OR v.theme IS NOT c.theme
   OR v.default_subsidiary_id IS NOT c.default_subsidiary_id
   OR v.can_submit_expenses IS NOT c.can_submit_expenses
UNION ALL
-- expense_reports (p20.1 single-id twin; mutable header, no delete op -- a report
-- terminates as 'rejected'/'converted', never hard-deleted).
SELECT 'expense_reports:' || CAST(c.id AS TEXT)
FROM expense_reports c
LEFT JOIN expense_reports_versions v
  ON v.id = (SELECT id FROM expense_reports_versions x WHERE x.entity_id = c.id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
   OR v.submitter_id IS NOT c.submitter_id OR v.subsidiary_id IS NOT c.subsidiary_id
   OR v.status IS NOT c.status OR v.review_notes IS NOT c.review_notes
   OR v.posted_transaction_id IS NOT c.posted_transaction_id
   OR v.created_at IS NOT c.created_at
UNION ALL
-- expense_report_lines (p20.1 single-id twin; a line can be hard-deleted, so a
-- missing live row for a 'delete' version is legitimate -- the standard single-id
-- block only checks CURRENT live rows against their snapshot).
SELECT 'expense_report_lines:' || CAST(c.id AS TEXT)
FROM expense_report_lines c
LEFT JOIN expense_report_lines_versions v
  ON v.id = (SELECT id FROM expense_report_lines_versions x WHERE x.entity_id = c.id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
   OR v.report_id IS NOT c.report_id OR v.account_id IS NOT c.account_id
   OR v.amount IS NOT c.amount OR v.fund_id IS NOT c.fund_id
   OR v.program_id IS NOT c.program_id OR v.memo IS NOT c.memo
UNION ALL
-- reconciliations (p16.1 single-id twin; status flips open<->finalized, op='update'
-- on finalize/reopen -- never hard-deleted, so a missing/'delete' latest version is
-- the violation, mirroring the expense_reports block).
SELECT 'reconciliations:' || CAST(c.id AS TEXT)
FROM reconciliations c
LEFT JOIN reconciliations_versions v
  ON v.id = (SELECT id FROM reconciliations_versions x WHERE x.entity_id = c.id
             ORDER BY x.valid_from DESC, x.id DESC LIMIT 1)
WHERE v.id IS NULL OR v.op = 'delete'
   OR v.account_id IS NOT c.account_id OR v.statement_date IS NOT c.statement_date
   OR v.statement_balance IS NOT c.statement_balance OR v.currency IS NOT c.currency
   OR v.status IS NOT c.status OR v.notes IS NOT c.notes`

// --- Z4: PRAGMA foreign_key_check returns nothing ----------------------------
// The pragma table-valued function yields one row per FK violation with the
// offending table and rowid.
const sqlZ4 = `
SELECT 'FK violation in ' || "table" || ' rowid ' || CAST(rowid AS TEXT)
       || ' -> ' || "parent" || ' (fkid ' || CAST(fkid AS TEXT) || ')'
FROM pragma_foreign_key_check()`

// --- Z5: every *_versions.change_id references a real changes row ------------
// A version row pointing at a missing change is audit corruption. Covers every
// versioned table's twin.
const sqlZ5 = `
SELECT tbl || ' version ' || CAST(vid AS TEXT) || ' -> missing change ' || CAST(change_id AS TEXT)
FROM (
  SELECT 'subsidiaries_versions' AS tbl, id AS vid, change_id FROM subsidiaries_versions
  UNION ALL SELECT 'programs_versions', id, change_id FROM programs_versions
  UNION ALL SELECT 'accounts_versions', id, change_id FROM accounts_versions
  UNION ALL SELECT 'account_names_versions', id, change_id FROM account_names_versions
  UNION ALL SELECT 'account_subsidiaries_versions', id, change_id FROM account_subsidiaries_versions
  UNION ALL SELECT 'funds_versions', id, change_id FROM funds_versions
  UNION ALL SELECT 'fund_subsidiaries_versions', id, change_id FROM fund_subsidiaries_versions
  UNION ALL SELECT 'payees_versions', id, change_id FROM payees_versions
  UNION ALL SELECT 'budget_schedules_versions', id, change_id FROM budget_schedules_versions
  UNION ALL SELECT 'budget_schedule_dates_versions', id, change_id FROM budget_schedule_dates_versions
  UNION ALL SELECT 'budgets_versions', id, change_id FROM budgets_versions
  UNION ALL SELECT 'budget_lines_versions', id, change_id FROM budget_lines_versions
  UNION ALL SELECT 'transactions_versions', id, change_id FROM transactions_versions
  UNION ALL SELECT 'splits_versions', id, change_id FROM splits_versions
  UNION ALL SELECT 'users_versions', id, change_id FROM users_versions
  UNION ALL SELECT 'user_report_grants_versions', id, change_id FROM user_report_grants_versions
  UNION ALL SELECT 'expense_reports_versions', id, change_id FROM expense_reports_versions
  UNION ALL SELECT 'expense_report_lines_versions', id, change_id FROM expense_report_lines_versions
  UNION ALL SELECT 'reconciliations_versions', id, change_id FROM reconciliations_versions
) vr
WHERE NOT EXISTS (SELECT 1 FROM changes ch WHERE ch.id = vr.change_id)`

// --- Z6: no orphan splits (transaction_id must exist) ------------------------
const sqlZ6 = `
SELECT 'orphan split ' || CAST(s.id AS TEXT) || ' -> missing transaction ' || CAST(s.transaction_id AS TEXT)
FROM splits s
WHERE NOT EXISTS (SELECT 1 FROM transactions t WHERE t.id = s.transaction_id)`

// --- Z7: account tree acyclic ------------------------------------------------
// Walk each account UP its parent chain; if the walk revisits the starting node
// (or exceeds a generous bound), the tree has a cycle. The bounded depth guards
// against a self- or mutual-parent cycle that would otherwise loop forever.
const sqlZ7 = `
WITH RECURSIVE up(start, id, depth) AS (
  SELECT id, id, 0 FROM accounts
  UNION ALL
  SELECT up.start, a.parent_id, up.depth + 1
  FROM accounts a JOIN up ON a.id = up.id
  WHERE a.parent_id IS NOT NULL AND up.depth < 100000
)
SELECT 'account tree cycle at ' || CAST(start AS TEXT)
FROM up
WHERE (depth > 0 AND id = start) OR depth >= 100000
GROUP BY start`

// --- Z8: a cleared split matches its reconciliation's account and currency (D13) --
// A reconciliation is per (account, currency) and a bank statement covers one
// balance, so every split cleared against a recon (reconciliation_id NOT NULL)
// must be on that recon's account and carry that recon's currency (the currency
// lives on the split's transaction, D3). Any status (open or finalized): a
// cross-account or cross-currency clearing is wrong regardless. Non-deleted txns
// only (a soft-deleted txn is out of the ledger; its splits' stale clearing does
// not corrupt a statement).
const sqlZ8 = `
SELECT 'split ' || CAST(s.id AS TEXT) || ' cleared against reconciliation '
       || CAST(s.reconciliation_id AS TEXT)
       || ' but account/currency mismatch (split account ' || CAST(s.account_id AS TEXT)
       || '/' || t.currency || ' vs recon account ' || CAST(r.account_id AS TEXT)
       || '/' || r.currency || ')'
FROM splits s
JOIN transactions t ON t.id = s.transaction_id
JOIN reconciliations r ON r.id = s.reconciliation_id
WHERE t.deleted = 0
  AND (s.account_id <> r.account_id OR t.currency <> r.currency)`

// --- Z9: a finalized reconciliation reconciles to its statement chain (D13) ----
// For each FINALIZED reconciliation, opening + the net-debit sum of the splits
// cleared against it must equal its statement_balance (all in minor units, D2
// sign -- no flip). Opening = the statement_balance of the prior finalized recon
// for the SAME (account, currency), by (statement_date, id) order; COALESCE 0
// when this is the first. Splits on a soft-deleted txn are excluded from the sum
// (out of the ledger). A mismatch means the finalized statement no longer proves
// (tampering after finalize, or a bad chain).
const sqlZ9 = `
SELECT 'finalized reconciliation ' || CAST(r.id AS TEXT)
       || ' opening + cleared <> statement_balance ' || CAST(r.statement_balance AS TEXT)
FROM reconciliations r
WHERE r.status = 'finalized'
  AND (
    COALESCE((
      SELECT p.statement_balance FROM reconciliations p
      WHERE p.status = 'finalized' AND p.account_id = r.account_id
        AND p.currency = r.currency
        AND (p.statement_date < r.statement_date
             OR (p.statement_date = r.statement_date AND p.id < r.id))
      ORDER BY p.statement_date DESC, p.id DESC LIMIT 1
    ), 0)
    + COALESCE((
      SELECT SUM(s.amount) FROM splits s
      JOIN transactions t ON t.id = s.transaction_id
      WHERE s.reconciliation_id = r.id AND t.deleted = 0
    ), 0)
  ) <> r.statement_balance`

// --- Z10: every non-deleted txn sums to zero within each fund group (D20) -----
// NULL fund_id is one group (the unrestricted group). A (transaction, fund-group)
// whose splits do not net to 0 is a violation. COALESCE keys NULL to a sentinel
// that no real fund id can take (fund ids are positive AUTOINCREMENT).
const sqlZ10 = `
SELECT 'transaction ' || CAST(t.id AS TEXT) || ' fund '
       || CASE WHEN s.fund_id IS NULL THEN 'unrestricted' ELSE CAST(s.fund_id AS TEXT) END
       || ' sums to ' || CAST(SUM(s.amount) AS TEXT)
FROM transactions t
JOIN splits s ON s.transaction_id = t.id
WHERE t.deleted = 0
GROUP BY t.id, COALESCE(s.fund_id, -1)
HAVING SUM(s.amount) <> 0`

// --- Z11: every split's account is mapped to its txn's subsidiary (D18) -------
// The split's account must have an account_subsidiaries row for the txn's
// subsidiary. Checked on non-deleted txns (a soft-deleted txn is out of scope).
const sqlZ11 = `
SELECT 'split ' || CAST(s.id AS TEXT) || ' account ' || CAST(s.account_id AS TEXT)
       || ' not mapped to subsidiary ' || CAST(t.subsidiary_id AS TEXT)
FROM splits s
JOIN transactions t ON t.id = s.transaction_id
WHERE t.deleted = 0
  AND NOT EXISTS (
    SELECT 1 FROM account_subsidiaries m
    WHERE m.account_id = s.account_id AND m.subsidiary_id = t.subsidiary_id)`

// --- Z12: parent's subsidiary set superset-of union of children's (D18) -------
// For each parent/child account edge, every subsidiary mapped to the CHILD must
// also be mapped to the PARENT. A child membership missing on the parent is a
// violation.
const sqlZ12 = `
SELECT 'account ' || CAST(child.id AS TEXT) || ' maps subsidiary ' || CAST(cm.subsidiary_id AS TEXT)
       || ' but parent ' || CAST(child.parent_id AS TEXT) || ' does not'
FROM accounts child
JOIN account_subsidiaries cm ON cm.account_id = child.id
WHERE child.parent_id IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM account_subsidiaries pm
    WHERE pm.account_id = child.parent_id AND pm.subsidiary_id = cm.subsidiary_id)`

// --- Z13: a non-NULL split fund's subsidiary set contains the txn's sub (D20) --
// Every split tagged a fund, on a non-deleted txn, must have that fund scoped to
// the transaction's subsidiary.
const sqlZ13 = `
SELECT 'split ' || CAST(s.id AS TEXT) || ' fund ' || CAST(s.fund_id AS TEXT)
       || ' not scoped to subsidiary ' || CAST(t.subsidiary_id AS TEXT)
FROM splits s
JOIN transactions t ON t.id = s.transaction_id
WHERE t.deleted = 0 AND s.fund_id IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM fund_subsidiaries fs
    WHERE fs.fund_id = s.fund_id AND fs.subsidiary_id = t.subsidiary_id)`

// --- Z14: functional_class present iff the split's account is expense (D21) ---
const sqlZ14 = `
SELECT 'split ' || CAST(s.id AS TEXT) || ' functional_class/account-type mismatch (account '
       || CAST(s.account_id AS TEXT) || ')'
FROM splits s
JOIN accounts a ON a.id = s.account_id
WHERE (a.type = 'expense' AND s.functional_class IS NULL)
   OR (a.type <> 'expense' AND s.functional_class IS NOT NULL)`

// --- Z15: program_id present iff R/E, and within the fund's program subtree ----
// Two failure modes:
//
//	(a) presence: a revenue/expense split must carry program_id; an A/L/E split
//	    must not.
//	(b) scope: when BOTH a fund (with a program scope) and a program are set on an
//	    R/E split, the program must lie in the fund's program subtree. The subtree
//	    is computed by a recursive descent from the fund's program_id.
const sqlZ15 = `
SELECT 'split ' || CAST(s.id AS TEXT) || ' program/account-type mismatch (account '
       || CAST(s.account_id AS TEXT) || ')'
FROM splits s
JOIN accounts a ON a.id = s.account_id
WHERE (a.type IN ('revenue','expense') AND s.program_id IS NULL)
   OR (a.type NOT IN ('revenue','expense') AND s.program_id IS NOT NULL)
UNION ALL
SELECT 'split ' || CAST(s.id AS TEXT) || ' program ' || CAST(s.program_id AS TEXT)
       || ' outside fund ' || CAST(s.fund_id AS TEXT) || ' program scope'
FROM splits s
JOIN funds f ON f.id = s.fund_id
WHERE s.program_id IS NOT NULL AND f.program_id IS NOT NULL
  AND s.program_id NOT IN (
    WITH RECURSIVE sub(id) AS (
      SELECT f.program_id
      UNION ALL
      SELECT p.id FROM programs p JOIN sub ON p.parent_id = sub.id
    )
    SELECT id FROM sub)`

// --- Z16: subsidiary and program trees acyclic, exactly one root each ---------
// Two trees, four conditions: each must have exactly one NULL-parent root and be
// acyclic. Acyclicity uses the same upward-walk-with-bound as Z7.
const sqlZ16 = `
SELECT 'subsidiary roots = ' || CAST(COUNT(*) AS TEXT) || ' (want 1)'
FROM subsidiaries WHERE parent_id IS NULL
HAVING COUNT(*) <> 1
UNION ALL
SELECT 'program roots = ' || CAST(COUNT(*) AS TEXT) || ' (want 1)'
FROM programs WHERE parent_id IS NULL
HAVING COUNT(*) <> 1
UNION ALL
SELECT 'subsidiary tree cycle at ' || CAST(start AS TEXT) FROM (
  WITH RECURSIVE up(start, id, depth) AS (
    SELECT id, id, 0 FROM subsidiaries
    UNION ALL
    SELECT up.start, x.parent_id, up.depth + 1
    FROM subsidiaries x JOIN up ON x.id = up.id
    WHERE x.parent_id IS NOT NULL AND up.depth < 100000
  )
  SELECT start FROM up WHERE (depth > 0 AND id = start) OR depth >= 100000 GROUP BY start)
UNION ALL
SELECT 'program tree cycle at ' || CAST(start AS TEXT) FROM (
  WITH RECURSIVE up(start, id, depth) AS (
    SELECT id, id, 0 FROM programs
    UNION ALL
    SELECT up.start, x.parent_id, up.depth + 1
    FROM programs x JOIN up ON x.id = up.id
    WHERE x.parent_id IS NOT NULL AND up.depth < 100000
  )
  SELECT start FROM up WHERE (depth > 0 AND id = start) OR depth >= 100000 GROUP BY start)`

// --- Z17 (warning): intercompany accounts net to zero per currency (D19) ------
// At full consolidation, the splits on intercompany-flagged accounts must net to
// zero within each currency. A non-zero net per currency is a warning row (never
// silently dropped). Non-deleted txns only.
const sqlZ17 = `
SELECT 'intercompany net for ' || t.currency || ' is ' || CAST(SUM(s.amount) AS TEXT)
FROM splits s
JOIN transactions t ON t.id = s.transaction_id
JOIN accounts a ON a.id = s.account_id
WHERE t.deleted = 0 AND a.intercompany = 1
GROUP BY t.currency
HAVING SUM(s.amount) <> 0`

// --- Z18 (warning): no restricted fund has a negative cumulative balance (D23) -
// Sign sense (net-debit, D2: debits +, credits -). The literal "sum of ALL splits
// tagged the fund" is provably vacuous: Z10 forces every transaction to net zero
// WITHIN each fund group, so that total is ALWAYS 0 on Z10-clean data and could
// only go negative when Z10 already fired -- it would never independently surface
// the overspend D23 targets. So Z18 tracks the fund's CASH/ASSET balance instead
// (the task's own clarifying clause: "a restricted-fund cash/asset balance going
// negative is the flag"): the net-debit sum of the fund's splits ON ASSET
// accounts, per currency. Because the whole fund nets to zero, that asset sum
// equals the fund's unexpended resources; it goes NEGATIVE exactly when the fund
// was spent past its receipts (receive 50k then spend 90k -- each txn fund-
// balanced, yet the restricted cash is -40k). Non-expense applications (buying a
// building, paying loan principal on the restriction) move the balance between
// asset accounts and never false-positive, since they stay non-negative in
// aggregate. NULL fund_id (unrestricted) is excluded -- only restricted funds.
// See DECISIONS p08.3 for the recorded sense.
const sqlZ18 = `
SELECT 'restricted fund ' || CAST(s.fund_id AS TEXT) || ' asset balance in ' || t.currency
       || ' is ' || CAST(SUM(s.amount) AS TEXT)
FROM splits s
JOIN transactions t ON t.id = s.transaction_id
JOIN accounts a ON a.id = s.account_id
WHERE t.deleted = 0 AND s.fund_id IS NOT NULL AND a.type = 'asset'
GROUP BY s.fund_id, t.currency
HAVING SUM(s.amount) < 0`

// --- Z19 (warning): active R/E leaf with activity has an effective 990 code ---
// An active leaf account (no children) of type revenue/expense that HAS splits
// must resolve an effective 990 code -- its own form990_code or the nearest
// ancestor's (D25). A leaf with activity and no effective code anywhere up its
// chain is a warning (it would land in the reports' Unmapped bucket). The upward
// walk stops as soon as it finds a code; a leaf is flagged when the whole chain
// yields none.
const sqlZ19 = `
SELECT 'active R/E leaf ' || CAST(a.id AS TEXT) || ' has activity but no effective 990 code'
FROM accounts a
WHERE a.active = 1
  AND a.type IN ('revenue','expense')
  AND NOT EXISTS (SELECT 1 FROM accounts c WHERE c.parent_id = a.id)
  AND EXISTS (SELECT 1 FROM splits s WHERE s.account_id = a.id)
  AND NOT EXISTS (
    WITH RECURSIVE chain(id, parent_id, code, depth) AS (
      SELECT a.id, a.parent_id, a.form990_code, 0
      UNION ALL
      SELECT p.id, p.parent_id, p.form990_code, chain.depth + 1
      FROM accounts p JOIN chain ON p.id = chain.parent_id
      WHERE chain.depth < 100000
    )
    SELECT 1 FROM chain WHERE code IS NOT NULL)`
