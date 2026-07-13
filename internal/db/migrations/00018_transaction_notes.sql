-- +goose Up
-- p24.2: transaction-level NOTES -- a longer, multiline free-text explanation on the
-- transaction header, distinct from the short per-split memo (D2). Forward-only;
-- never edit an applied migration; no Down (AGENTS rule 4). Migration files stay
-- pure ASCII (the p04.2 sqlc byte-offset quirk: sqlc reads migrations as its schema).
--
-- notes is a VERSIONED business column: it is added to BOTH the live transactions
-- table AND its versions twin, and InsertTransactionVersion's snapshot SELECT (plus
-- every other transactions_versions SELECT: AsOf, History) gains it, so Z3 (current
-- == latest snapshot) stays clean and as-of reconstruction shows the right notes.
-- Existing rows (live and historical) backfill to '' (notes did not exist before);
-- the snapshot-from-live version append supplies notes explicitly from then on.
ALTER TABLE transactions ADD COLUMN notes TEXT NOT NULL DEFAULT '';
ALTER TABLE transactions_versions ADD COLUMN notes TEXT NOT NULL DEFAULT '';
