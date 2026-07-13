-- p19.1: budget operations (Phase 19 budgeting). All SQL for the store's budget
-- methods lives here (rule 6). Copies the version-append convention established in
-- funds.sql (p07.3): the entity op does the live write inside the funnel's fn, then
-- appends a snapshot-from-live version row, so each version row is byte-identical to
-- its live row (Z3 can never diverge) and valid_from == changes.at BY CONSTRUCTION.
--
-- budget_schedule_dates is a COMPOSITE (schedule_id, occurs_on) SET, versioned like
-- fund_subsidiaries: an import is op='create', a removal op='delete' (captured
-- BEFORE the live DELETE -- the removal-op ordering).
--
-- Query names are DISTINCT from every other domain's (InsertBudget vs InsertFund,
-- ...): sqlc emits into one package, so a name collision fails generation.
--
-- NOTE: keep every comment and identifier in this file PURE ASCII. sqlc v1.31.1
-- miscounts byte offsets when a query file contains multi-byte UTF-8, corrupting
-- the generated SQL for the WHOLE file (see docs/DECISIONS.md p04.2). Version
-- params use plain positional `?` (the append-query convention).

-- ===========================================================================
-- budget_schedules
-- ===========================================================================

-- name: InsertBudgetSchedule :one
-- Live insert of a named schedule. kind is a CHECK enum; the kind-specific fields
-- are validated in the store per kind. Returns the new id for the store to
-- snapshot + return.
INSERT INTO budget_schedules
  (name, kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date,
   weekend_adjust, notes)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetBudgetSchedule :one
SELECT id, name, kind, day_of_month, day_of_month_2, ordinal, weekday,
       anchor_date, weekend_adjust, notes
FROM budget_schedules
WHERE id = ?;

-- name: ListBudgetSchedules :many
-- Every schedule, id-ordered (the library list + line editor option source, p19.3).
SELECT id, name, kind, day_of_month, day_of_month_2, ordinal, weekday,
       anchor_date, weekend_adjust, notes
FROM budget_schedules
ORDER BY id;

-- name: UpdateBudgetSchedule :exec
-- Live update: the store reads the current row (GetBudgetSchedule), overrides the
-- caller's fields, and writes the full desired state here (snapshot-from-live).
UPDATE budget_schedules
SET name = ?, kind = ?, day_of_month = ?, day_of_month_2 = ?, ordinal = ?,
    weekday = ?, anchor_date = ?, weekend_adjust = ?, notes = ?
WHERE id = ?;

-- name: InsertBudgetScheduleVersion :exec
-- Snapshot-from-live version append for budget_schedules (STANDARD single-column
-- entity). Reads the CURRENT live row (MUST run AFTER the live write) and copies
-- every business column, so the version row is byte-identical to the live row;
-- valid_from is the change's own `at`. Snapshot column set matches 00016 exactly.
-- Params (positional): op, change_id, entity_id -> Op, ID (change_id), ID_2.
INSERT INTO budget_schedules_versions
  (entity_id, change_id, valid_from, op, name, kind, day_of_month, day_of_month_2,
   ordinal, weekday, anchor_date, weekend_adjust, notes)
SELECT bs.id, c.id, c.at, ?, bs.name, bs.kind, bs.day_of_month, bs.day_of_month_2,
       bs.ordinal, bs.weekday, bs.anchor_date, bs.weekend_adjust, bs.notes
FROM budget_schedules bs, changes c
WHERE c.id = ? AND bs.id = ?;

-- name: InsertBudgetScheduleDate :exec
-- Import one (schedule_id, occurs_on) date. The list is a SET (the PK forbids
-- duplicates); callers dedupe before adding.
INSERT INTO budget_schedule_dates (schedule_id, occurs_on)
VALUES (?, ?);

-- name: DeleteBudgetScheduleDate :exec
-- Remove one imported date. For op=delete the version row is captured BEFORE this
-- runs (the live row must still exist to snapshot).
DELETE FROM budget_schedule_dates
WHERE schedule_id = ? AND occurs_on = ?;

-- name: InsertBudgetScheduleDateVersion :exec
-- Snapshot-from-live version append for a COMPOSITE (schedule_id, occurs_on) list
-- row. entity_id = schedule_id; occurs_on is both a snapshot column and part of the
-- entity identity (00016). For op='create' this runs AFTER the live insert; for
-- op='delete' it runs BEFORE the live delete. Params (positional): op, change_id,
-- schedule_id, occurs_on -> Op, ID, ScheduleID, OccursOn.
INSERT INTO budget_schedule_dates_versions
  (entity_id, change_id, valid_from, op, occurs_on)
SELECT bsd.schedule_id, c.id, c.at, ?, bsd.occurs_on
FROM budget_schedule_dates bsd, changes c
WHERE c.id = ? AND bsd.schedule_id = ? AND bsd.occurs_on = ?;

-- name: BudgetScheduleDates :many
-- The imported date list for a schedule (order-insensitive; the store builds a set
-- for the diff and the engine sorts).
SELECT occurs_on FROM budget_schedule_dates
WHERE schedule_id = ?
ORDER BY occurs_on;

-- ===========================================================================
-- budgets
-- ===========================================================================

-- name: InsertBudget :one
INSERT INTO budgets (name, period_start, period_end, notes)
VALUES (?, ?, ?, ?)
RETURNING id;

-- name: GetBudget :one
SELECT id, name, period_start, period_end, notes
FROM budgets
WHERE id = ?;

-- name: ListBudgets :many
SELECT id, name, period_start, period_end, notes
FROM budgets
ORDER BY id;

-- name: UpdateBudget :exec
UPDATE budgets
SET name = ?, period_start = ?, period_end = ?, notes = ?
WHERE id = ?;

-- name: InsertBudgetVersion :exec
-- Snapshot-from-live version append for budgets (single-column entity). Runs AFTER
-- the live write. Params (positional): op, change_id, entity_id.
INSERT INTO budgets_versions
  (entity_id, change_id, valid_from, op, name, period_start, period_end, notes)
SELECT b.id, c.id, c.at, ?, b.name, b.period_start, b.period_end, b.notes
FROM budgets b, changes c
WHERE c.id = ? AND b.id = ?;

-- ===========================================================================
-- budget_lines
-- ===========================================================================

-- name: InsertBudgetLine :one
-- Live insert of a budget line. amount is minor units per occurrence (rule 3);
-- fund_id may be NULL (unrestricted); the store validates the R/E account, the
-- fund/program/sub/schedule refs, and the currency. Returns the new id.
INSERT INTO budget_lines
  (budget_id, subsidiary_id, account_id, fund_id, program_id, amount, currency,
   schedule_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetBudgetLine :one
SELECT id, budget_id, subsidiary_id, account_id, fund_id, program_id, amount,
       currency, schedule_id
FROM budget_lines
WHERE id = ?;

-- name: ListBudgetLines :many
-- The lines of one budget, id-ordered (the budget editor + p19.2 expansion source).
SELECT id, budget_id, subsidiary_id, account_id, fund_id, program_id, amount,
       currency, schedule_id
FROM budget_lines
WHERE budget_id = ?
ORDER BY id;

-- name: UpdateBudgetLine :exec
UPDATE budget_lines
SET budget_id = ?, subsidiary_id = ?, account_id = ?, fund_id = ?, program_id = ?,
    amount = ?, currency = ?, schedule_id = ?
WHERE id = ?;

-- name: DeleteBudgetLine :exec
-- Hard delete of a budget line. For op=delete the version row is captured BEFORE
-- this runs (rule 14: everything but transactions hard-deletes with an audit
-- version; the live row must still exist to snapshot).
DELETE FROM budget_lines
WHERE id = ?;

-- name: InsertBudgetLineVersion :exec
-- Snapshot-from-live version append for budget_lines (single-column entity). For
-- op='create'/'update' runs AFTER the live write; for op='delete' runs BEFORE the
-- live delete. Params (positional): op, change_id, entity_id.
INSERT INTO budget_lines_versions
  (entity_id, change_id, valid_from, op, budget_id, subsidiary_id, account_id,
   fund_id, program_id, amount, currency, schedule_id)
SELECT bl.id, c.id, c.at, ?, bl.budget_id, bl.subsidiary_id, bl.account_id,
       bl.fund_id, bl.program_id, bl.amount, bl.currency, bl.schedule_id
FROM budget_lines bl, changes c
WHERE c.id = ? AND bl.id = ?;
