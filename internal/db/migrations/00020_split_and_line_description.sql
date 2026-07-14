-- +goose Up
-- p26.15: per-split / per-line free-text DESCRIPTION -- the schema+store foundation
-- for the payee->description migration. The app is replacing the per-transaction
-- payee with a per-split free-text description (autocomplete + prefill land in later
-- steps); THIS migration only adds the column, additive and INERT (no UI, no read
-- output change, no payee removal yet). Forward-only; never edit an applied
-- migration; no Down (AGENTS rule 4). Migration files stay pure ASCII (the p04.2
-- sqlc byte-offset quirk: sqlc reads migrations as its schema).
--
-- The column is named `description` (NOT `desc` -- a SQL reserved word that would
-- need quoting everywhere). It is a VERSIONED business column, so -- exactly like
-- p24.2 transaction notes (00018) -- it is added to BOTH the live table AND its
-- versions twin, on TWO entities: splits and expense_report_lines. Every version
-- SELECT (InsertSplitVersion / InsertExpenseReportLineVersion snapshots, the AsOf
-- and History reconstructions) and the Z3 current==snapshot comparison gain it so
-- as-of reconstruction and `cuento check` stay clean. Existing rows (live and
-- historical) backfill to '' (description did not exist before); both a live row and
-- its already-written version row default to '' so Z3 sees '' IS NOT '' = false and
-- needs no new backfill row.
ALTER TABLE splits ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE splits_versions ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE expense_report_lines ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE expense_report_lines_versions ADD COLUMN description TEXT NOT NULL DEFAULT '';
