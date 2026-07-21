-- p20.1: expense-report operations (Phase 20). All SQL for the store's
-- expense-report methods lives here (rule 6). Copies the version-append convention
-- established in funds.sql/budgets.sql: the entity op does the live write inside the
-- funnel's fn, then appends a snapshot-from-live version row, so each version row is
-- byte-identical to its live row (Z3 can never diverge) and valid_from == changes.at
-- BY CONSTRUCTION.
--
-- Query names are DISTINCT from every other domain's (sqlc emits into one package, so
-- a name collision fails generation). Version params use plain positional `?` (the
-- append-query convention).
--
-- NOTE: keep every comment and identifier in this file PURE ASCII. sqlc v1.31.1
-- miscounts byte offsets when a query file contains multi-byte UTF-8, corrupting the
-- generated SQL for the WHOLE file (see docs/DECISIONS.md p04.2).

-- ===========================================================================
-- expense_reports
-- ===========================================================================

-- name: InsertExpenseReport :one
-- Live insert of a report header (status defaults to 'draft'). created_at is the
-- store's clock. posted_transaction_id starts NULL (set only on convert). The header
-- fields (description/memo/ap_account_id) carry the creation-time PREFILLS: the
-- creator's display name (description), the localized "Expense report" default (memo),
-- and the subsidiary's default AP account (ap_account_id, NULL when the sub has none).
-- Returns the new id for the store to snapshot + return.
INSERT INTO expense_reports (submitter_id, subsidiary_id, created_at, description, memo, ap_account_id)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetExpenseReport :one
SELECT id, submitter_id, subsidiary_id, status, review_notes,
       posted_transaction_id, created_at, date, description, memo, notes, ap_account_id
FROM expense_reports
WHERE id = ?;

-- name: ListExpenseReportsBySubmitter :many
-- A submitter's own reports, newest first (the p20.2 my-reports list).
SELECT id, submitter_id, subsidiary_id, status, review_notes,
       posted_transaction_id, created_at, date, description, memo, notes, ap_account_id
FROM expense_reports
WHERE submitter_id = ?
ORDER BY id DESC;

-- name: ListExpenseReportsByStatus :many
-- Reports in a given status, id-ordered (the p20.3 reviewer queue reads 'submitted').
SELECT id, submitter_id, subsidiary_id, status, review_notes,
       posted_transaction_id, created_at, date, description, memo, notes, ap_account_id
FROM expense_reports
WHERE status = ?
ORDER BY id;

-- name: SetExpenseReportStatus :exec
-- Live update of a report's status + review_notes (the state-machine transition).
-- The store validates the transition first; this writes the resulting state. Both
-- columns are set together so a reject stores its reason and a resubmit preserves it
-- (the store passes the current notes through).
UPDATE expense_reports
SET status = ?, review_notes = ?
WHERE id = ?;

-- name: SetExpenseReportHeader :exec
-- Live update of a report's HEADER fields (p-golive): the report/txn date, a one-line
-- description, a short memo, and longer notes -- the values the reviewer's convert
-- prefills into the posted transaction. Editable while draft/rejected (the store guards
-- the status). All four are plain TEXT ('' = unset).
UPDATE expense_reports
SET date = ?, description = ?, memo = ?, notes = ?
WHERE id = ?;

-- name: SetExpenseReportSubsidiary :exec
-- Live update of a draft report's subsidiary (p25.3: the submitter picks the sub on
-- the report page, editable ONLY while the report has no lines -- the store guards the
-- line-count precondition so a change can't orphan sub-scoped line accounts). ap_account_id
-- is RE-SEEDED to the NEW subsidiary's default_ap_account_id in the SAME write (the AP is a
-- sub-scoped account; leaving the old sub's AP would be a cross-subsidiary main split that
-- trips a ledger invariant at convert). NULL when the new sub has no default.
UPDATE expense_reports
SET subsidiary_id = ?, ap_account_id = ?
WHERE id = ?;

-- name: DeleteExpenseReport :exec
-- Hard delete of a DRAFT report (p25.3 discard). Its lines are deleted first (each with
-- an op=delete version); for op=delete the report's own version row is captured BEFORE
-- this runs (rule 14: the live row must still exist to snapshot).
DELETE FROM expense_reports
WHERE id = ?;

-- name: SetExpenseReportConverted :exec
-- Live update flipping a report to 'converted' and LINKING the posted transaction.
-- posted_transaction_id is set ONLY here (convert is its sole writer). The store
-- validates the txn exists and the report is 'submitted' first.
UPDATE expense_reports
SET status = 'converted', posted_transaction_id = ?
WHERE id = ?;

-- name: InsertExpenseReportVersion :exec
-- Snapshot-from-live version append for expense_reports (STANDARD single-column
-- entity). Reads the CURRENT live row (MUST run AFTER the live write) and copies
-- every business column, so the version row is byte-identical to the live row;
-- valid_from is the change's own `at`. Snapshot column set matches 00017 exactly.
-- Params (positional): op, change_id, entity_id -> Op, ID (change_id), ID_2.
INSERT INTO expense_reports_versions
  (entity_id, change_id, valid_from, op, submitter_id, subsidiary_id, status,
   review_notes, posted_transaction_id, created_at, date, description, memo, notes,
   ap_account_id)
SELECT er.id, c.id, c.at, ?, er.submitter_id, er.subsidiary_id, er.status,
       er.review_notes, er.posted_transaction_id, er.created_at,
       er.date, er.description, er.memo, er.notes, er.ap_account_id
FROM expense_reports er, changes c
WHERE c.id = ? AND er.id = ?;

-- ===========================================================================
-- expense_report_lines
-- ===========================================================================

-- name: InsertExpenseReportLine :one
-- Live insert of a report line. amount is minor units, SIGNED (rule 3; the report
-- need not balance). fund_id/program_id may be NULL (the reviewer resolves them at
-- convert). Returns the new id.
INSERT INTO expense_report_lines (report_id, account_id, amount, fund_id, program_id, memo, description)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetExpenseReportLine :one
SELECT id, report_id, account_id, amount, fund_id, program_id, memo, description
FROM expense_report_lines
WHERE id = ?;

-- name: ListExpenseReportLines :many
-- The lines of one report, id-ordered (the editor + the p20.3 convert prefill).
SELECT id, report_id, account_id, amount, fund_id, program_id, memo, description
FROM expense_report_lines
WHERE report_id = ?
ORDER BY id;

-- name: CountExpenseReportLines :one
-- Count of a report's lines (the submit guard requires >= 1 line).
SELECT COUNT(*) FROM expense_report_lines WHERE report_id = ?;

-- name: UpdateExpenseReportLine :exec
UPDATE expense_report_lines
SET account_id = ?, amount = ?, fund_id = ?, program_id = ?, memo = ?, description = ?
WHERE id = ?;

-- name: DeleteExpenseReportLine :exec
-- Hard delete of a report line. For op=delete the version row is captured BEFORE
-- this runs (rule 14: everything but transactions hard-deletes with an audit
-- version; the live row must still exist to snapshot).
DELETE FROM expense_report_lines
WHERE id = ?;

-- name: InsertExpenseReportLineVersion :exec
-- Snapshot-from-live version append for expense_report_lines (single-column
-- entity). For op='create'/'update' runs AFTER the live write; for op='delete' runs
-- BEFORE the live delete. Params (positional): op, change_id, entity_id.
INSERT INTO expense_report_lines_versions
  (entity_id, change_id, valid_from, op, report_id, account_id, amount, fund_id,
   program_id, memo, description)
SELECT erl.id, c.id, c.at, ?, erl.report_id, erl.account_id, erl.amount,
       erl.fund_id, erl.program_id, erl.memo, erl.description
FROM expense_report_lines erl, changes c
WHERE c.id = ? AND erl.id = ?;
