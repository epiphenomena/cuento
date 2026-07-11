-- +goose Up
-- p08.1: payees, transactions, splits (+versions, triggers, indexes) (D2, D18,
-- D20, D21, D24). Forward-only; never edit an applied migration; no Down
-- (AGENTS rule 4). Migration files stay pure ASCII (the p04.2 sqlc byte-offset
-- quirk: sqlc reads migrations as its schema).
--
-- FK targets must pre-exist, so the create order is:
--   payees -> transactions -> splits -> the three *_versions twins -> indexes
--   -> triggers (the accounts trigger references splits, so splits comes first).
--
-- All rows are USER-CREATED (no seed), so -- like 00005/00009 -- there is no
-- seed changes/version wiring this step. Store version-append queries and
-- AssertVersioned extensions land in p08.2; these tables define the exact
-- snapshot shapes it must write.
--
-- reconciliation_id is DELIBERATELY OMITTED from splits AND splits_versions:
-- Appendix A lists it on splits, but it references the reconciliations table
-- which does not exist until p16.1, which adds the column then (D13). Do not add
-- it in this step.
--
-- NOT enforced here (Appendix A note): zero-sum per transaction AND per
-- (transaction, fund), single currency/subsidiary per transaction,
-- split-account within the txn's subsidiary, and fund subsidiary/program scope.
-- SQLite triggers fire per-row with no deferred constraint, so these cross-row
-- invariants are enforced in the store (the only writer, p08.2) and proven by
-- cuento check (Z1, Z10, Z11, Z13, Z15 in p08.3).

-- ---------------------------------------------------------------------------
-- payees: a versioned business table. name is unique case-insensitively
-- (COLLATE NOCASE) so 'Acme' and 'acme' are the same payee.
-- ---------------------------------------------------------------------------
CREATE TABLE payees (
  id     INTEGER PRIMARY KEY AUTOINCREMENT,                  -- D17
  name   TEXT NOT NULL UNIQUE COLLATE NOCASE,
  active INTEGER NOT NULL DEFAULT 1
);

-- payees_versions: STANDARD single-id twin (entity_id = payees.id). Snapshot
-- column set (payees business columns, id excluded), which p08.2's
-- InsertPayeeVersion must write exactly: name, active.
CREATE TABLE payees_versions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id  INTEGER NOT NULL,                               -- payees.id
  change_id  INTEGER NOT NULL REFERENCES changes(id),
  valid_from TEXT NOT NULL,                                  -- equals changes.at (rule 5)
  op         TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of payees' business columns:
  name       TEXT NOT NULL,
  active     INTEGER NOT NULL
);
CREATE INDEX payees_versions_entity ON payees_versions(entity_id, valid_from);

-- ---------------------------------------------------------------------------
-- transactions: the transaction header (D2, D3, D18). date is a plain
-- YYYY-MM-DD digit-shape (the same loose GLOB as funds.start_date). Exactly one
-- subsidiary per transaction (D18); single currency (D3). Soft-delete only
-- (rule 14): deleted flips to 1, the row is never removed.
-- ---------------------------------------------------------------------------
CREATE TABLE transactions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,           -- D17
  date          TEXT NOT NULL CHECK (date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  subsidiary_id INTEGER NOT NULL REFERENCES subsidiaries(id),  -- D18: exactly one
  payee_id      INTEGER REFERENCES payees(id),               -- optional
  memo          TEXT NOT NULL DEFAULT '',
  currency      TEXT NOT NULL REFERENCES currencies(code),   -- D3: single currency
  deleted       INTEGER NOT NULL DEFAULT 0                   -- soft-delete (rule 14)
);

-- transactions_versions: STANDARD single-id twin (entity_id = transactions.id).
-- Snapshot columns (transactions business columns, id excluded), which p08.2's
-- InsertTransactionVersion must write exactly:
--   date, subsidiary_id, payee_id, memo, currency, deleted.
CREATE TABLE transactions_versions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id     INTEGER NOT NULL,                            -- transactions.id
  change_id     INTEGER NOT NULL REFERENCES changes(id),
  valid_from    TEXT NOT NULL,                               -- equals changes.at (rule 5)
  op            TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of transactions' business columns:
  date          TEXT NOT NULL,
  subsidiary_id INTEGER NOT NULL,
  payee_id      INTEGER,
  memo          TEXT NOT NULL,
  currency      TEXT NOT NULL,
  deleted       INTEGER NOT NULL
);
CREATE INDEX transactions_versions_entity ON transactions_versions(entity_id, valid_from);

-- ---------------------------------------------------------------------------
-- splits: the transaction lines (D2, D20, D21, D24). amount is int64 minor
-- units, net-debit sign (D1/D2), CHECK amount <> 0. fund_id NULL == unrestricted
-- (D20). program_id required iff the account is revenue/expense (trigger +
-- Z15). functional_class required iff the account is expense (trigger + Z14).
-- reconciliation_id is NOT here -- deferred to p16.1 (see the header note).
-- ---------------------------------------------------------------------------
CREATE TABLE splits (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,        -- D17
  transaction_id   INTEGER NOT NULL REFERENCES transactions(id),
  account_id       INTEGER NOT NULL REFERENCES accounts(id),
  amount           INTEGER NOT NULL CHECK (amount <> 0),     -- minor units, net-debit sign (D1/D2)
  fund_id          INTEGER REFERENCES funds(id),             -- NULL = unrestricted (D20)
  program_id       INTEGER REFERENCES programs(id),          -- required iff R/E account (trigger + Z15)
  functional_class TEXT CHECK (functional_class IN ('program','management','fundraising')),
                                                             -- required iff expense account (trigger + Z14)
  memo             TEXT NOT NULL DEFAULT '',
  position         INTEGER NOT NULL                          -- display order within the transaction
);

-- splits_versions: STANDARD single-id twin (entity_id = splits.id). Snapshot
-- columns (splits business columns, id excluded, reconciliation_id deferred to
-- p16.1), which p08.2's InsertSplitVersion must write exactly:
--   transaction_id, account_id, amount, fund_id, program_id, functional_class,
--   memo, position.
CREATE TABLE splits_versions (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id        INTEGER NOT NULL,                         -- splits.id
  change_id        INTEGER NOT NULL REFERENCES changes(id),
  valid_from       TEXT NOT NULL,                            -- equals changes.at (rule 5)
  op               TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of splits' business columns:
  transaction_id   INTEGER NOT NULL,
  account_id       INTEGER NOT NULL,
  amount           INTEGER NOT NULL,
  fund_id          INTEGER,
  program_id       INTEGER,
  functional_class TEXT,
  memo             TEXT NOT NULL,
  position         INTEGER NOT NULL
);
CREATE INDEX splits_versions_entity ON splits_versions(entity_id, valid_from);

-- ---------------------------------------------------------------------------
-- Indexes (Appendix A): the split-side foreign keys plus the two transaction
-- lookup columns that reports and registers scan.
-- ---------------------------------------------------------------------------
CREATE INDEX splits_account ON splits(account_id);
CREATE INDEX splits_txn     ON splits(transaction_id);
CREATE INDEX splits_fund    ON splits(fund_id);
CREATE INDEX splits_program ON splits(program_id);
CREATE INDEX txn_date       ON transactions(date);
CREATE INDEX txn_sub        ON transactions(subsidiary_id);

-- ---------------------------------------------------------------------------
-- Triggers: the row-local invariant subset (rule 7). Cross-row invariants
-- (zero-sum, subsidiary/fund/program scope) are store + check, not here.
-- Each trigger joins accounts. INSERT + UPDATE pairs where an update can
-- re-violate (a split can re-point account_id/functional_class/program_id).
-- ---------------------------------------------------------------------------

-- trg_splits_leaf_active_only (D11): a split's account_id must be a LEAF (no
-- account has it as parent) AND active=1. A placeholder (an account with
-- children) holds no splits; an inactive account takes no new splits.
-- +goose StatementBegin
CREATE TRIGGER trg_splits_leaf_active_only_insert
BEFORE INSERT ON splits
BEGIN
  SELECT RAISE(ABORT, 'splits: account must be an active leaf')
  WHERE EXISTS (SELECT 1 FROM accounts c WHERE c.parent_id = NEW.account_id)
     OR NOT EXISTS (SELECT 1 FROM accounts a WHERE a.id = NEW.account_id AND a.active = 1);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_splits_leaf_active_only_update
BEFORE UPDATE ON splits
BEGIN
  SELECT RAISE(ABORT, 'splits: account must be an active leaf')
  WHERE EXISTS (SELECT 1 FROM accounts c WHERE c.parent_id = NEW.account_id)
     OR NOT EXISTS (SELECT 1 FROM accounts a WHERE a.id = NEW.account_id AND a.active = 1);
END;
-- +goose StatementEnd

-- trg_accounts_no_children_over_splits (D11): a new account whose parent_id
-- points at an account that ALREADY has splits is rejected -- a placeholder
-- that holds splits cannot gain children. Insert-only on accounts (the
-- reparent-move case is the store's job in p08.2 + Z-check in p08.3).
-- +goose StatementBegin
CREATE TRIGGER trg_accounts_no_children_over_splits
BEFORE INSERT ON accounts
WHEN NEW.parent_id IS NOT NULL
BEGIN
  SELECT RAISE(ABORT, 'accounts: parent already holds splits and cannot gain children')
  WHERE EXISTS (SELECT 1 FROM splits s WHERE s.account_id = NEW.parent_id);
END;
-- +goose StatementEnd

-- trg_splits_function_matches_type (D21): join the account -- if type='expense'
-- require functional_class NOT NULL; else require it NULL.
-- +goose StatementBegin
CREATE TRIGGER trg_splits_function_matches_type_insert
BEFORE INSERT ON splits
BEGIN
  SELECT RAISE(ABORT, 'splits: functional_class required iff expense account')
  WHERE EXISTS (
    SELECT 1 FROM accounts a WHERE a.id = NEW.account_id
      AND (
        (a.type = 'expense' AND NEW.functional_class IS NULL)
        OR (a.type <> 'expense' AND NEW.functional_class IS NOT NULL)
      )
  );
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_splits_function_matches_type_update
BEFORE UPDATE ON splits
BEGIN
  SELECT RAISE(ABORT, 'splits: functional_class required iff expense account')
  WHERE EXISTS (
    SELECT 1 FROM accounts a WHERE a.id = NEW.account_id
      AND (
        (a.type = 'expense' AND NEW.functional_class IS NULL)
        OR (a.type <> 'expense' AND NEW.functional_class IS NOT NULL)
      )
  );
END;
-- +goose StatementEnd

-- trg_splits_program_matches_type (D24): join the account -- if type is revenue
-- or expense require program_id NOT NULL; else require it NULL.
-- +goose StatementBegin
CREATE TRIGGER trg_splits_program_matches_type_insert
BEFORE INSERT ON splits
BEGIN
  SELECT RAISE(ABORT, 'splits: program_id required iff revenue/expense account')
  WHERE EXISTS (
    SELECT 1 FROM accounts a WHERE a.id = NEW.account_id
      AND (
        (a.type IN ('revenue','expense') AND NEW.program_id IS NULL)
        OR (a.type NOT IN ('revenue','expense') AND NEW.program_id IS NOT NULL)
      )
  );
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_splits_program_matches_type_update
BEFORE UPDATE ON splits
BEGIN
  SELECT RAISE(ABORT, 'splits: program_id required iff revenue/expense account')
  WHERE EXISTS (
    SELECT 1 FROM accounts a WHERE a.id = NEW.account_id
      AND (
        (a.type IN ('revenue','expense') AND NEW.program_id IS NULL)
        OR (a.type NOT IN ('revenue','expense') AND NEW.program_id IS NOT NULL)
      )
  );
END;
-- +goose StatementEnd
