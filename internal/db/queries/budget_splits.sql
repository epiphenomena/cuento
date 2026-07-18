-- p27.2: split-derived budget model (DECISIONS "Budget redesign"). All SQL for the
-- store's budget-PLAN + budget-SPLIT methods lives here (rule 6). ADDITIVE: the old
-- budgets/budget_lines queries in budgets.sql are UNTOUCHED. Copies the version-
-- append convention (funds.sql / budgets.sql): the entity op does the live write
-- inside the funnel's fn, then appends a snapshot-from-live version row, so each
-- version row is byte-identical to its live row (Z3 can never diverge) and
-- valid_from == changes.at BY CONSTRUCTION.
--
-- Query names are DISTINCT from every other domain's (InsertBudgetPlan vs
-- InsertBudget, ...): sqlc emits into one package, so a name collision fails
-- generation.
--
-- NOTE: keep every comment and identifier in this file PURE ASCII (docs/DECISIONS.md
-- p04.2 byte-offset quirk). Version params use plain positional `?`.

-- ===========================================================================
-- budget_plans
-- ===========================================================================

-- name: InsertBudgetPlan :one
INSERT INTO budget_plans (name, subsidiary_id, notes)
VALUES (?, ?, ?)
RETURNING id;

-- name: GetBudgetPlan :one
SELECT id, name, subsidiary_id, notes
FROM budget_plans
WHERE id = ?;

-- name: ListBudgetPlans :many
SELECT id, name, subsidiary_id, notes
FROM budget_plans
ORDER BY id;

-- name: UpdateBudgetPlan :exec
UPDATE budget_plans
SET name = ?, subsidiary_id = ?, notes = ?
WHERE id = ?;

-- name: DeleteBudgetPlan :exec
DELETE FROM budget_plans
WHERE id = ?;

-- name: InsertBudgetPlanVersion :exec
INSERT INTO budget_plans_versions
  (entity_id, change_id, valid_from, op, name, subsidiary_id, notes)
SELECT bp.id, c.id, c.at, ?, bp.name, bp.subsidiary_id, bp.notes
FROM budget_plans bp, changes c
WHERE c.id = ? AND bp.id = ?;

-- ===========================================================================
-- budget_splits
-- ===========================================================================

-- name: InsertBudgetSplit :one
INSERT INTO budget_splits
  (plan_id, description, date, account_id, fund_id, program_id, amount, currency)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetBudgetSplit :one
SELECT id, plan_id, description, date, account_id, fund_id, program_id, amount, currency
FROM budget_splits
WHERE id = ?;

-- name: ListBudgetSplits :many
SELECT id, plan_id, description, date, account_id, fund_id, program_id, amount, currency
FROM budget_splits
WHERE plan_id = ?
ORDER BY date, id;

-- name: UpdateBudgetSplit :exec
UPDATE budget_splits
SET description = ?, date = ?, account_id = ?, fund_id = ?, program_id = ?,
    amount = ?, currency = ?
WHERE id = ?;

-- name: DeleteBudgetSplit :exec
DELETE FROM budget_splits
WHERE id = ?;

-- name: InsertBudgetSplitVersion :exec
INSERT INTO budget_splits_versions
  (entity_id, change_id, valid_from, op, plan_id, description, date, account_id,
   fund_id, program_id, amount, currency)
SELECT bs.id, c.id, c.at, ?, bs.plan_id, bs.description, bs.date, bs.account_id,
       bs.fund_id, bs.program_id, bs.amount, bs.currency
FROM budget_splits bs, changes c
WHERE c.id = ? AND bs.id = ?;
