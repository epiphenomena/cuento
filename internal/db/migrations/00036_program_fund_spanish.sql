-- +goose Up
-- Spanish (secondary) name for programs AND funds, plus a free-text DESCRIPTION on
-- programs. Forward-only; never edit an applied migration; no Down (AGENTS rule 4).
--
-- name stays the ENGLISH/primary name (programs.name keeps its UNIQUE constraint).
-- name_es is the OPTIONAL Spanish rendering, shown to a Spanish-locale viewer with
-- en-fallback when blank -- so it is NOT unique and defaults to the empty string
-- ('' = "no Spanish name"). description is a program's free-text note shown on the
-- programs list. Both are NOT NULL DEFAULT '' so the ALTER is legal on existing rows
-- (they read '' going forward) with NO nullString mapping in the store.
--
-- Versioning ripple (rule 5): programs and funds are VERSIONED entities, so every
-- new business column must ALSO be snapshotted in the _versions twin or Z3 (current
-- == latest snapshot) diverges for any entity touched after this migration. The
-- twins get the same NOT NULL DEFAULT '' columns; existing version rows predate them
-- and default to '' (fine for pre-existing snapshots). The store's InsertProgramVersion
-- / InsertFundVersion queries are updated to SELECT the new columns from the live row,
-- keeping live and latest-snapshot consistent going forward. This mirrors 00031's
-- accounts.notes ripple exactly (base table + versions twin, version-append updated).
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the p04.2
-- byte-offset quirk applies here too).

ALTER TABLE programs          ADD COLUMN name_es     TEXT NOT NULL DEFAULT '';
ALTER TABLE programs          ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE programs_versions ADD COLUMN name_es     TEXT NOT NULL DEFAULT '';
ALTER TABLE programs_versions ADD COLUMN description TEXT NOT NULL DEFAULT '';

ALTER TABLE funds             ADD COLUMN name_es TEXT NOT NULL DEFAULT '';
ALTER TABLE funds_versions    ADD COLUMN name_es TEXT NOT NULL DEFAULT '';
