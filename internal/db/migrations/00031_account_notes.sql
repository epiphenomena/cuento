-- +goose Up
-- p28.7: a free-text notes/description column on accounts. Forward-only; never
-- edit an applied migration; no Down (AGENTS rule 4).
--
-- notes is a nullable TEXT field holding a short human note ABOUT the account (a
-- description, a reminder) -- NOT the account NAME (which lives in account_names,
-- per-language). It is presentation/documentation data with no invariant attached:
-- no trigger, no store validation. The chart of accounts shows it in the column
-- the per-row type used to occupy (the chart already groups by type, p26.74, so the
-- per-row type is redundant).
--
-- notes is TEXT with NO default (nullable): existing account rows predate the
-- column and read NULL (= "no note"), so the ALTER is legal without a rewrite. The
-- store maps NULL <-> "" via nullString (the Form990Code pattern), so a blank note
-- round-trips as NULL.
--
-- Versioning ripple (rule 5): accounts_versions must ALSO snapshot notes, or Z3
-- (current == latest snapshot) diverges for any account touched after this
-- migration. The version twin gets the same nullable column (existing version rows
-- predate it and stay NULL, fine); the store's InsertAccountVersion selects it going
-- forward. This mirrors 00008's default_program_id and 00027's current_cash/open_item
-- ripples exactly.
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the p04.2
-- byte-offset quirk applies here too).

ALTER TABLE accounts          ADD COLUMN notes TEXT;
ALTER TABLE accounts_versions ADD COLUMN notes TEXT;
