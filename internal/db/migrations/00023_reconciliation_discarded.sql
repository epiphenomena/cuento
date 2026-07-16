-- +goose NO TRANSACTION
-- +goose Up
-- p26.58: widen reconciliations.status to allow 'discarded' -- a SOFT status for an
-- abandoned OPEN reconciliation (RULE 14: audit sacred, NO hard delete; the recon row,
-- its versions, and its changes all remain). 00014 pinned status IN ('open','finalized'),
-- and SQLite cannot ALTER a CHECK constraint in place -- the only path is a full table
-- rebuild (create with the widened CHECK, copy, drop, rename). Forward-only; never edit an
-- applied migration; no Down (AGENTS rule 4). The runner backs up the db file first.
--
-- WHY 'discarded' is free elsewhere: every reconciliation predicate keys on the exact
-- literal it wants -- CountOpenReconciliations / reconList / WorkspaceSplits on
-- status='open'; PriorFinalizedStatementBalance / HasLaterFinalizedReconciliation / Z9 /
-- FinalizedReconciledSplitIDs on status='finalized'. A third value 'discarded' is
-- excluded from ALL of them automatically: a discarded recon does not count as open (so a
-- fresh recon can start for the same account+currency and the ErrOpenReconciliationExists
-- guard passes) and does not count as finalized (so it is not in the opening chain and Z9
-- ignores it). The discard store op also UN-clears the recon's splits (reconciliation_id ->
-- NULL), so Z8 (a cleared split must match its recon's account/currency) sees no split
-- pointing at the discarded recon.
--
-- FK/TRANSACTION note: splits.reconciliation_id is an FK INTO reconciliations, and Open()
-- sets PRAGMA foreign_keys=ON per connection (db.go DSN). DROP TABLE reconciliations does
-- an implicit delete of its rows, which -- with FKs enforced -- would trip that FK from any
-- cleared split and fail the DROP. PRAGMA foreign_keys is a NO-OP inside a transaction, and
-- goose wraps each migration in one, so this migration is marked NO TRANSACTION (goose runs
-- all its statements on ONE *sql.Conn) and toggles foreign_keys OFF around the rebuild, then
-- back ON. Losing the wrapping transaction is covered by the runner's pre-migration backup
-- (rule 4). foreign_key_check confirms integrity before re-enabling.
--
-- reconciliations_versions is UNTOUCHED: its status column is a plain audit snapshot with no
-- CHECK (00014), so a version row can already carry 'discarded' -- no rebuild needed there.
-- The reconciliations_versions.change_id FK -> changes is unaffected (we do not drop changes).
--
-- TRIGGER note: trg_split_locked_when_finalized (00014, ON splits) references reconciliations
-- in its body. SQLite validates a trigger's referenced tables when a referenced table is
-- DROPped, so DROP TABLE reconciliations fails while that trigger exists ("no such table:
-- main.reconciliations" from inside the trigger). We DROP the trigger first, rebuild the
-- table, then RECREATE the trigger verbatim against the renamed table -- forward-only, and
-- the trigger body is byte-identical to 00014.
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the p04.2 byte-offset quirk).

PRAGMA foreign_keys=OFF;

-- Drop the splits trigger that references reconciliations (blocks the DROP below); recreated
-- verbatim after the rebuild.
DROP TRIGGER trg_split_locked_when_finalized;

-- Rebuilt reconciliations with the widened status CHECK. Column set + defaults are
-- byte-identical to 00014 except the CHECK's IN list gains 'discarded'.
CREATE TABLE reconciliations_new (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id        INTEGER NOT NULL REFERENCES accounts(id),
  statement_date    TEXT NOT NULL CHECK (statement_date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  statement_balance INTEGER NOT NULL,
  currency          TEXT NOT NULL REFERENCES currencies(code),
  status            TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','finalized','discarded')),
  notes             TEXT NOT NULL DEFAULT ''
);

INSERT INTO reconciliations_new (id, account_id, statement_date, statement_balance, currency, status, notes)
SELECT id, account_id, statement_date, statement_balance, currency, status, notes
FROM reconciliations;

DROP TABLE reconciliations;

ALTER TABLE reconciliations_new RENAME TO reconciliations;

-- Recreate the split-lock trigger VERBATIM from 00014 (guarded columns + body unchanged); it
-- now resolves reconciliations to the rebuilt table by name. The splits.reconciliation_id FK
-- likewise re-resolves to the renamed table by name.
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

-- Confirm no dangling references before re-enabling enforcement.
PRAGMA foreign_key_check;

PRAGMA foreign_keys=ON;
