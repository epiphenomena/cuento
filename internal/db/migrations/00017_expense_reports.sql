-- +goose Up
-- p20.1: expense-report model + submit permission (Phase 20). Forward-only; never
-- edit an applied migration; no Down (AGENTS rule 4).
--
-- A submission -> review workflow DECOUPLED from book-editing: a low-privilege user
-- SUBMITS an expense report (a set of proposed revenue/expense splits that need NOT
-- balance); an editing user later CONVERTS it into a real balanced ledger
-- transaction (p20.3) or REJECTS it with a reason. The posted transaction links
-- back to its source report for audit (posted_transaction_id, set ONLY on convert).
--
-- This migration:
--   1. ALTERs users to add the standalone `can_submit_expenses` capability -- a
--      per-user boolean INDEPENDENT of txn_perm (a pure submitter has NO ledger
--      access). Perms are audited, so it is VERSIONED like the other user perm
--      columns: the same column is ALTER-added to users_versions with the SAME
--      default, so the pre-existing system-user backfill version row (00006, id=2)
--      stays byte-equal to its live row and Z3 needs no new backfill.
--   2. CREATEs expense_reports + its version twin (single-id, mutable header, no
--      delete op -- mirrors budgets, 00016). status is a fixed enum; the state
--      machine (draft->submitted->rejected->submitted->converted) is store-enforced.
--   3. CREATEs expense_report_lines + its version twin (single-id; a line can be
--      hard-deleted with a delete version -- mirrors budget_lines, rule 14).
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the byte-offset
-- quirk in docs/DECISIONS.md p04.2 applies here too).

-- ---------------------------------------------------------------------------
-- 1. ALTER users + users_versions: the standalone submit capability. INTEGER
--    boolean, NOT NULL DEFAULT 0 (the ALTER is legal on the existing system-user
--    row). The SAME column + SAME default is added to users_versions so the
--    already-written system-user backfill snapshot (00006, change_id=2) equals its
--    live row (both default 0) and Z3 stays clean without a new backfill row.
-- ---------------------------------------------------------------------------
ALTER TABLE users ADD COLUMN can_submit_expenses INTEGER NOT NULL DEFAULT 0;             -- D10: standalone, independent of txn_perm
ALTER TABLE users_versions ADD COLUMN can_submit_expenses INTEGER NOT NULL DEFAULT 0;    -- perms are audited (rule 5); same default keeps the 00006 backfill Z3-clean

-- ---------------------------------------------------------------------------
-- 2. expense_reports -- the report header. status is a fixed enum; the transitions
--    are store-enforced (there is no 'resubmitted' status -- resubmit is
--    rejected->'submitted'). review_notes holds the reviewer's REASON on reject
--    (kept across a resubmit so the submitter still sees it, p20.2). subsidiary_id
--    is the single subsidiary the report's splits belong to. posted_transaction_id
--    is NULL until convert, when it links the real ledger txn (p20.3) -- the ONLY
--    place it is set. Mirrors budgets (00016): single-id versioned twin, mutable
--    header, no delete op (a report is never hard-deleted -- it terminates as
--    'rejected' or 'converted').
-- ---------------------------------------------------------------------------
CREATE TABLE expense_reports (
  id                    INTEGER PRIMARY KEY AUTOINCREMENT,                 -- D17
  submitter_id          INTEGER NOT NULL REFERENCES users(id),
  subsidiary_id         INTEGER NOT NULL REFERENCES subsidiaries(id),
  status                TEXT NOT NULL DEFAULT 'draft'
                          CHECK (status IN ('draft','submitted','rejected','converted')),
  review_notes          TEXT NOT NULL DEFAULT '',                          -- reviewer's reject reason (kept across resubmit)
  posted_transaction_id INTEGER REFERENCES transactions(id),              -- NULL until convert; the source-of-truth audit link
  created_at            TEXT NOT NULL
                          CHECK (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*')
);
CREATE INDEX expense_reports_submitter ON expense_reports(submitter_id);
CREATE INDEX expense_reports_status ON expense_reports(status);

-- expense_report_lines -- one proposed split. amount is INTEGER minor units (rule
-- 3), SIGNED (the report need NOT balance and R/E direction is not enforced here --
-- the reviewer resolves accounts/sign when building the balanced txn at convert,
-- p20.3). fund_id and program_id are NULLABLE (a submitter may not know the
-- restriction/program; the reviewer fills them at convert -- unlike a budget line,
-- which enforces R/E + program because it is a projection of ledger flows). memo is
-- the submitter's free-text note. Mirrors budget_lines (00016): single-id twin, a
-- line can be hard-deleted with a delete version (rule 14).
CREATE TABLE expense_report_lines (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,                            -- D17
  report_id  INTEGER NOT NULL REFERENCES expense_reports(id),
  account_id INTEGER NOT NULL REFERENCES accounts(id),
  amount     INTEGER NOT NULL,                                             -- minor units, SIGNED (rule 3); need not balance
  fund_id    INTEGER REFERENCES funds(id),                                -- NULL allowed (reviewer resolves)
  program_id INTEGER REFERENCES programs(id),                             -- NULL allowed (reviewer resolves)
  memo       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX expense_report_lines_report ON expense_report_lines(report_id);

-- ---------------------------------------------------------------------------
-- 3. Version twins (rule 5, D4). Append-only; never UPDATE/DELETE. Snapshot columns
--    carry NO CHECK/FK (history, not live); the only referential FK is
--    change_id -> changes. Each InsertXVersion query does INSERT ... SELECT from the
--    just-written live row on the same tx, so a version row is byte-identical to its
--    live row (Z3 can never diverge) and valid_from == changes.at BY CONSTRUCTION.
--    These mirror 00016 EXACTLY.
-- ---------------------------------------------------------------------------

-- expense_reports_versions: STANDARD single-column entity (entity_id =
-- expense_reports.id). Snapshot column set (business columns, id excluded) which
-- InsertExpenseReportVersion writes EXACTLY:
--   submitter_id, subsidiary_id, status, review_notes, posted_transaction_id,
--   created_at
CREATE TABLE expense_reports_versions (
  id                    INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id             INTEGER NOT NULL,                                  -- expense_reports.id
  change_id             INTEGER NOT NULL REFERENCES changes(id),
  valid_from            TEXT NOT NULL,                                     -- equals changes.at (rule 5)
  op                    TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of expense_reports' business columns (no CHECK/FK -- history):
  submitter_id          INTEGER NOT NULL,
  subsidiary_id         INTEGER NOT NULL,
  status                TEXT NOT NULL,
  review_notes          TEXT NOT NULL,
  posted_transaction_id INTEGER,
  created_at            TEXT NOT NULL
);
CREATE INDEX expense_reports_versions_entity ON expense_reports_versions(entity_id, valid_from);

-- expense_report_lines_versions: STANDARD single-column entity (entity_id =
-- expense_report_lines.id). A line CAN be op='delete' (hard delete + delete
-- version, rule 14). Snapshot column set which InsertExpenseReportLineVersion writes
-- EXACTLY:
--   report_id, account_id, amount, fund_id, program_id, memo
CREATE TABLE expense_report_lines_versions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id  INTEGER NOT NULL,                                            -- expense_report_lines.id
  change_id  INTEGER NOT NULL REFERENCES changes(id),
  valid_from TEXT NOT NULL,                                               -- equals changes.at (rule 5)
  op         TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of expense_report_lines' business columns:
  report_id  INTEGER NOT NULL,
  account_id INTEGER NOT NULL,
  amount     INTEGER NOT NULL,
  fund_id    INTEGER,
  program_id INTEGER,
  memo       TEXT NOT NULL
);
CREATE INDEX expense_report_lines_versions_entity ON expense_report_lines_versions(entity_id, valid_from);
