-- +goose Up
-- p16.1: reconciliations (+versions twin), splits.reconciliation_id, and the
-- split-lock trigger (D13). Forward-only; never edit an applied migration; no
-- Down (AGENTS rule 4). Keep this file PURE ASCII (sqlc reads migrations as its
-- schema; the p04.2 byte-offset quirk applies -- 00008/00009/00010 established
-- the convention).
--
-- Create order (FK targets and trigger-referenced tables must pre-exist):
--   reconciliations -> reconciliations_versions -> ALTER splits (adds the FK
--   column pointing at reconciliations) -> trg_split_locked_when_finalized (its
--   body joins reconciliations, so that table must already exist).
--
-- All rows are USER-CREATED (no seed), so -- like 00009/00010 -- there is NO seed
-- changes/version wiring this step: the last change id is unchanged. Store
-- lifecycle (StartReconciliation/SetSplitReconciled/Finalize/Reopen) and the
-- InsertReconciliationVersion append land in p16.2; this step is schema + the
-- row-local lock trigger only, exercised by direct-SQL tests
-- (internal/db/reconciliations_test.go) and the Z8/Z9 checks.
--
-- D13: a reconciliation is per (account, currency) and spans ALL funds -- a bank
-- statement covers one balance. statement_balance is stored in minor units with
-- the SAME net-debit sign as splits.amount (D1/D2), so Z9's chain equality is a
-- plain integer equality with no sign flip.

-- ---------------------------------------------------------------------------
-- reconciliations: one bank-statement reconciliation (D13). status flips
-- open -> finalized (and back on an audited reopen, p16.2). statement_date is
-- the same loose YYYY-MM-DD digit shape used elsewhere (transactions.date,
-- funds.start_date).
-- ---------------------------------------------------------------------------
CREATE TABLE reconciliations (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,               -- D17
  account_id        INTEGER NOT NULL REFERENCES accounts(id),        -- D13: per account
  statement_date    TEXT NOT NULL CHECK (statement_date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  statement_balance INTEGER NOT NULL,                                -- minor units, net-debit sign (D1/D2)
  currency          TEXT NOT NULL REFERENCES currencies(code),       -- D13: per currency
  status            TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','finalized')),
  notes             TEXT NOT NULL DEFAULT ''
);

-- reconciliations_versions: STANDARD single-id twin (entity_id = reconciliations.id).
-- Snapshot column set (reconciliations business columns, id excluded), which
-- p16.2's InsertReconciliationVersion must write EXACTLY:
--   account_id, statement_date, statement_balance, currency, status, notes.
-- As with the other twins the snapshot columns carry NO CHECK/FK (history, not
-- live); the only referential FK is change_id -> changes.
CREATE TABLE reconciliations_versions (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id         INTEGER NOT NULL,                                -- reconciliations.id
  change_id         INTEGER NOT NULL REFERENCES changes(id),
  valid_from        TEXT NOT NULL,                                   -- equals changes.at (rule 5)
  op                TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of reconciliations' business columns:
  account_id        INTEGER NOT NULL,
  statement_date    TEXT NOT NULL,
  statement_balance INTEGER NOT NULL,
  currency          TEXT NOT NULL,
  status            TEXT NOT NULL,
  notes             TEXT NOT NULL
);
CREATE INDEX reconciliations_versions_entity ON reconciliations_versions(entity_id, valid_from);

-- ---------------------------------------------------------------------------
-- splits.reconciliation_id: which reconciliation this split is cleared against
-- (D13). NULL == uncleared. Deliberately DEFERRED from 00010 to here (00010's
-- header note): it references reconciliations, which did not exist until now.
--
-- LIVE-ONLY, NOT VERSIONED (this step's decision, per D13): the reconciliation
-- state of a split (which recon it is cleared against) is OPERATIONAL metadata,
-- not audited business content. Clearing/unclearing a split must NOT mint a new
-- split VERSION -- the audited reconciliation event is finalization, recorded on
-- the reconciliations table (and its versions twin). So reconciliation_id is
-- ADDED to splits but NOT to splits_versions, and InsertSplitVersion's snapshot
-- column set stays exactly the 00010 set (no reconciliation_id) -- keeping the
-- Z3/Z5 version-consistency checks clean (a toggle changes no versioned column).
-- ---------------------------------------------------------------------------
ALTER TABLE splits ADD COLUMN reconciliation_id INTEGER REFERENCES reconciliations(id);

CREATE INDEX splits_reconciliation ON splits(reconciliation_id);

-- ---------------------------------------------------------------------------
-- trg_split_locked_when_finalized (D13): a split cleared against a FINALIZED
-- reconciliation is locked -- its financial content (amount, account_id,
-- transaction_id, fund_id) and its recon membership (reconciliation_id) may not
-- change while that recon is finalized. Editing requires an audited unreconcile
-- (reopen) first (p16.2). The guard fires when the OLD or the NEW reconciliation
-- is finalized, so it also blocks moving a split INTO or OUT OF a finalized recon
-- and re-pointing it between recons while finalized.
--
-- Guarded columns EXACTLY: amount, account_id, transaction_id, fund_id,
-- reconciliation_id. NOT guarded: memo, position, program_id, functional_class
-- (non-financial / not part of a bank statement's provable balance; the p16.2
-- store rule "memo/payee allowed" matches). NULL-safe IS NOT so a NULL->value or
-- value->NULL fund_id / reconciliation_id change is caught. The "date" lock D13
-- mentions is a transactions column, enforced store-side in p16.2 (not here).
-- +goose StatementBegin
CREATE TRIGGER trg_split_locked_when_finalized
BEFORE UPDATE ON splits
WHEN (
       NEW.amount           IS NOT OLD.amount
    OR NEW.account_id        IS NOT OLD.account_id
    OR NEW.transaction_id    IS NOT OLD.transaction_id
    OR NEW.fund_id           IS NOT OLD.fund_id
    OR NEW.reconciliation_id IS NOT OLD.reconciliation_id
     )
BEGIN
  SELECT RAISE(ABORT, 'splits: split is locked by a finalized reconciliation')
  WHERE EXISTS (
    SELECT 1 FROM reconciliations r
    WHERE r.status = 'finalized'
      AND (r.id = OLD.reconciliation_id OR r.id = NEW.reconciliation_id)
  );
END;
-- +goose StatementEnd
