-- +goose Up
-- p26.20: physically remove the payee entity. The payee->per-split-description
-- migration (p26.15..p26.19) already cut every user-facing read/write over to the
-- per-split description; payee has been DORMANT since then (data/columns/routes/entity
-- present but unused). This migration drops it for good. Forward-only; never edit an
-- applied migration; no Down (AGENTS rule 4). The runner backs up the db file first.
--
-- Drop order matters (FK targets): the transactions.payee_id column (an inline FK to
-- payees) must go BEFORE the payees table can be dropped, and the child *_versions
-- table drops before its parent. No index or trigger references payee_id (00010 indexes
-- only txn_date/txn_sub; triggers touch splits/accounts), and SQLite's ALTER TABLE
-- DROP COLUMN removes the inline column-level FK together with the column, so no
-- index/trigger has to be dropped first. The surviving indexes (txn_date, txn_sub)
-- do not name payee_id and are preserved by the drop.
--
-- transactions_versions.payee_id is a plain audit-snapshot column (no FK, no index),
-- so it drops trivially. After both columns are gone, the FK from transactions to
-- payees no longer exists, so payees_versions (child audit twin) then payees (parent)
-- can be dropped.
--
-- Versioning stays consistent (rule 5/14): the snapshot INSERT, every as-of/history
-- SELECT, the txn version structs, and the Z3 current-vs-snapshot comparison in
-- internal/ledger/checks.go all lost payee_id in the same step, so Z3 parity holds
-- without it (a transactions_versions row is still byte-identical to its live row).
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the byte-offset
-- quirk in docs/DECISIONS.md p04.2 applies here too).

-- Drop the live FK column first (removes the inline REFERENCES payees(id)).
ALTER TABLE transactions DROP COLUMN payee_id;

-- Drop the audit-snapshot column (plain INTEGER, no FK/index).
ALTER TABLE transactions_versions DROP COLUMN payee_id;

-- Now that no transactions.payee_id FK targets payees, drop the entity: child audit
-- twin first, then the parent live table.
DROP TABLE payees_versions;
DROP TABLE payees;
