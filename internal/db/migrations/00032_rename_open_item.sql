-- +goose Up
-- p31.x: rename the account flag `open_item` -> `receivable_payable`. The flag marks
-- receivable/payable accounts whose lines stay OPEN until settled (asset -> A/R,
-- liability -> A/P); the clearer name states what it IS. Forward-only; never edit an
-- applied migration; no Down (AGENTS rule 4).
--
-- Renamed on BOTH the live table and its version twin (rule 5: accounts_versions must
-- stay column-compatible with accounts, or Z3 diverges). SQLite ALTER TABLE RENAME
-- COLUMN also rewrites references inside triggers/views, but NOT trigger NAMES or string
-- literals, so the two open_item guard triggers are dropped and recreated with the new
-- name + message. The two triggers are dropped BEFORE the rename so their recreation is
-- unambiguous.
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the p04.2 byte-offset
-- quirk applies here too).

DROP TRIGGER IF EXISTS trg_accounts_open_item_al_only_insert;
DROP TRIGGER IF EXISTS trg_accounts_open_item_al_only_update;

ALTER TABLE accounts          RENAME COLUMN open_item TO receivable_payable;
ALTER TABLE accounts_versions RENAME COLUMN open_item TO receivable_payable;

-- trg_accounts_receivable_payable_al_only: receivable_payable=1 is allowed ONLY on
-- asset/liability accounts (the store rejects with ErrReceivablePayableBadType first;
-- trigger backstop). INSERT + UPDATE, because SQLite triggers fire per-row.
-- +goose StatementBegin
CREATE TRIGGER trg_accounts_receivable_payable_al_only_insert
BEFORE INSERT ON accounts
WHEN NEW.receivable_payable = 1 AND NEW.type NOT IN ('asset','liability')
BEGIN
  SELECT RAISE(ABORT, 'accounts: receivable_payable is allowed only on asset or liability accounts');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_accounts_receivable_payable_al_only_update
BEFORE UPDATE ON accounts
WHEN NEW.receivable_payable = 1 AND NEW.type NOT IN ('asset','liability')
BEGIN
  SELECT RAISE(ABORT, 'accounts: receivable_payable is allowed only on asset or liability accounts');
END;
-- +goose StatementEnd
