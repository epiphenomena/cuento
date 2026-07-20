-- +goose Up
-- p-golive: header fields on an expense report -- date, description, memo, notes --
-- so a submitter frames the whole report (its transaction date + a one-line
-- description + a short memo + longer notes), and the reviewer's convert PREFILLS the
-- posted transaction from them (date -> txn date, description -> the main/header split
-- description, memo -> txn memo, notes -> txn notes). Forward-only; never edit an
-- applied migration; no Down (AGENTS rule 4). Keep PURE ASCII (the p04.2 sqlc
-- byte-offset quirk: sqlc reads migrations as its schema).
--
-- All four are VERSIONED business columns, so -- exactly like p24.2 transaction notes
-- (00018) and p26.15 description (00020) -- each is ALTER-added to BOTH the live table
-- AND its versions twin, with the SAME default. The InsertExpenseReportVersion snapshot
-- SELECT and the Z3 current==snapshot comparison (internal/ledger/checks.go) both gain
-- the four columns so as-of reconstruction and `cuento check` stay clean.
--
-- `date` is NOT NULL DEFAULT '' with NO GLOB CHECK: an ADD COLUMN with a GLOB'd NOT
-- NULL default would fail immediately (SQLite evaluates the CHECK against the ''
-- default for existing rows and '' fails the GLOB). '' reads as "unset -> the reviewer
-- falls back to today at convert"; the web layer validates a supplied date via
-- parseEditorDate. `description`/`memo`/`notes` match the existing ''-default TEXT
-- pattern (00018/00020). A fresh-migrated db has ZERO reports, and both a live row and
-- its already-written version row default to '' (Z3 sees '' IS NOT '' = false), so no
-- backfill row is needed.
ALTER TABLE expense_reports ADD COLUMN date TEXT NOT NULL DEFAULT '';
ALTER TABLE expense_reports_versions ADD COLUMN date TEXT NOT NULL DEFAULT '';
ALTER TABLE expense_reports ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE expense_reports_versions ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE expense_reports ADD COLUMN memo TEXT NOT NULL DEFAULT '';
ALTER TABLE expense_reports_versions ADD COLUMN memo TEXT NOT NULL DEFAULT '';
ALTER TABLE expense_reports ADD COLUMN notes TEXT NOT NULL DEFAULT '';
ALTER TABLE expense_reports_versions ADD COLUMN notes TEXT NOT NULL DEFAULT '';
