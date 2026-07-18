-- +goose Up
-- p27.1: shared account attributes current_cash + open_item. Forward-only; never
-- edit an applied migration; no Down (AGENTS rule 4).
--
-- Two boolean flags accrete onto the account model (the design deliberately grows
-- a small set of orthogonal boolean columns -- intercompany already, now these two
-- -- rather than an account-kind taxonomy):
--
--   * current_cash -- marks accounts representing spendable cash. The budget
--     cash-flow projection's per-fund opening balance sums the actual balances of
--     current_cash accounts. Meaningful ONLY on asset accounts (store-enforced +
--     Z20 + trigger backstop below).
--
--   * open_item -- marks receivable/payable accounts whose individual lines stay
--     OPEN until settled (the A/R-A/P attribute). Whether a flagged account reads
--     as A/R vs A/P is DERIVED from its type (asset -> receivable, liability ->
--     payable), so it is ONE boolean, not an enum. Meaningful ONLY on asset or
--     liability accounts (store-enforced + Z20 + trigger backstop below).
--
-- Both columns are INTEGER NOT NULL DEFAULT 0 so the ALTER is legal on the existing
-- account rows (they predate the columns and stay 0 = false).
--
-- Versioning ripple (rule 5): accounts_versions must ALSO snapshot the two new
-- columns, or Z3 (current == latest snapshot) diverges for any account touched
-- after this migration. The version twin gets the same columns (nullable, no
-- default -- existing version rows predate them and stay NULL, fine for pre-existing
-- snapshots; the store's InsertAccountVersion is updated to select them going
-- forward). This mirrors 00008's default_program_id ripple exactly.
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the p04.2
-- byte-offset quirk applies here too).

ALTER TABLE accounts ADD COLUMN current_cash INTEGER NOT NULL DEFAULT 0;
ALTER TABLE accounts ADD COLUMN open_item    INTEGER NOT NULL DEFAULT 0;

ALTER TABLE accounts_versions ADD COLUMN current_cash INTEGER;
ALTER TABLE accounts_versions ADD COLUMN open_item    INTEGER;

-- trg_accounts_current_cash_asset_only: current_cash=1 is allowed ONLY on
-- type='asset' accounts (the store rejects with ErrCurrentCashNotAsset first; this
-- is the trigger backstop, mirroring trg_accounts_function_expense_only). INSERT +
-- UPDATE, because SQLite triggers fire per-row with no deferred constraint.
-- +goose StatementBegin
CREATE TRIGGER trg_accounts_current_cash_asset_only_insert
BEFORE INSERT ON accounts
WHEN NEW.current_cash = 1 AND NEW.type <> 'asset'
BEGIN
  SELECT RAISE(ABORT, 'accounts: current_cash is allowed only on asset accounts');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_accounts_current_cash_asset_only_update
BEFORE UPDATE ON accounts
WHEN NEW.current_cash = 1 AND NEW.type <> 'asset'
BEGIN
  SELECT RAISE(ABORT, 'accounts: current_cash is allowed only on asset accounts');
END;
-- +goose StatementEnd

-- trg_accounts_open_item_al_only: open_item=1 is allowed ONLY on asset/liability
-- accounts (the store rejects with ErrOpenItemBadType first; trigger backstop).
-- INSERT + UPDATE.
-- +goose StatementBegin
CREATE TRIGGER trg_accounts_open_item_al_only_insert
BEFORE INSERT ON accounts
WHEN NEW.open_item = 1 AND NEW.type NOT IN ('asset','liability')
BEGIN
  SELECT RAISE(ABORT, 'accounts: open_item is allowed only on asset or liability accounts');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_accounts_open_item_al_only_update
BEFORE UPDATE ON accounts
WHEN NEW.open_item = 1 AND NEW.type NOT IN ('asset','liability')
BEGIN
  SELECT RAISE(ABORT, 'accounts: open_item is allowed only on asset or liability accounts');
END;
-- +goose StatementEnd
