-- +goose Up
-- p07.1: programs -- a dimension (D24). Forward-only; never edit an applied
-- migration; no Down (AGENTS rule 4).
--
-- programs is STRUCTURALLY IDENTICAL to subsidiaries (00004) -- a single-root
-- tree with a versions twin, a single-root trigger PAIR, and an audit-consistent
-- seed -- MINUS base_currency. Every revenue and expense split carries a
-- program_id (required, defaulted; enforced in p08); A/L/E splits carry none. The
-- chart holds NATURAL categories; mission structure lives in the program tree.
--
-- This migration also:
--   * ALTERs accounts to add default_program_id (meaningful only on R/E accounts;
--     store-enforced), the column deferred from 00005 until programs exists;
--   * ripples that new column into accounts_versions (ALTER ADD COLUMN) so the
--     account version snapshot stays complete (Z3: current == latest snapshot).
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the p04.2
-- byte-offset quirk applies here too).

-- The live table: latest denormalized state (D4). parent_id is NULL only for the
-- single seeded root (enforced by trg_programs_single_root below).
CREATE TABLE programs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,          -- D17
  parent_id  INTEGER REFERENCES programs(id),            -- NULL only for the single root ("General")
  name       TEXT NOT NULL UNIQUE,
  active     INTEGER NOT NULL DEFAULT 1,
  sort_order INTEGER NOT NULL DEFAULT 0
);

-- Append-only version twin (rule 5, D4, Appendix A versions pattern). Never sees
-- UPDATE/DELETE. The SNAPSHOT COLUMN SET after the op column is:
--     parent_id, name, active, sort_order
-- i.e. the full business-column set of `programs` (id excluded). The store's
-- InsertProgramVersion query MUST write these exact columns/values so the two
-- version-emitting paths (this seed and the store) never diverge (Z3).
CREATE TABLE programs_versions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id  INTEGER NOT NULL,                           -- the programs.id this snapshot is of
  change_id  INTEGER NOT NULL REFERENCES changes(id),
  valid_from TEXT NOT NULL,                              -- equals changes.at (rule 5)
  op         TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of programs' business columns:
  parent_id  INTEGER,
  name       TEXT NOT NULL,
  active     INTEGER NOT NULL,
  sort_order INTEGER NOT NULL
);
CREATE INDEX programs_versions_entity ON programs_versions(entity_id, valid_from);

-- trg_programs_single_root: at most one row with parent_id IS NULL (D24). Two
-- triggers, because SQLite triggers fire per-row with no deferred constraint: a
-- BEFORE INSERT guard (reject a second root) AND a BEFORE UPDATE guard (reject
-- orphaning an existing row into a second root).
--
-- On BEFORE INSERT the new row is not yet in the table and its AUTOINCREMENT id
-- is unassigned (NEW.id IS NULL), so we check plain existence of any other root --
-- an `id <> NEW.id` clause would compare against NULL and never fire. On BEFORE
-- UPDATE the row already has an id, so we exclude it with `id <> NEW.id`.
-- +goose StatementBegin
CREATE TRIGGER trg_programs_single_root_insert
BEFORE INSERT ON programs
WHEN NEW.parent_id IS NULL
BEGIN
  SELECT RAISE(ABORT, 'programs: exactly one root allowed')
  WHERE EXISTS (SELECT 1 FROM programs WHERE parent_id IS NULL);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_programs_single_root_update
BEFORE UPDATE ON programs
WHEN NEW.parent_id IS NULL
BEGIN
  SELECT RAISE(ABORT, 'programs: exactly one root allowed')
  WHERE EXISTS (SELECT 1 FROM programs WHERE parent_id IS NULL AND id <> NEW.id);
END;
-- +goose StatementEnd

-- Seed the root program "General" WITH full audit consistency (three rows, in
-- order), the p04.1 pattern. Deterministic literals only -- no dynamic clock in a
-- migration (rule 4). This is change id 3: id 1 is 00004's root-subsidiary seed,
-- id 2 is 00006's system-user version backfill; 00003/00005/00007 seed no change.
-- (The stored name is "General"; a UI catalog label can come later -- no i18n here.)
--
-- 1. the changes row: system actor (user 1), fixed epoch timestamp, kind 'seed'.
INSERT INTO changes (id, actor_id, at, kind, note)
VALUES (3, 1, '1970-01-01T00:00:00Z', 'seed', 'seed root program');

-- 2. the live root row (passes the single-root trigger: no other root yet).
INSERT INTO programs (id, parent_id, name, active, sort_order)
VALUES (1, NULL, 'General', 1, 0);

-- 3. the op='create' version row: valid_from EQUALS changes.at exactly, snapshot
--    columns identical to the live row.
INSERT INTO programs_versions
  (entity_id, change_id, valid_from, op, parent_id, name, active, sort_order)
VALUES
  (1, 3, '1970-01-01T00:00:00Z', 'create', NULL, 'General', 1, 0);

-- ---------------------------------------------------------------------------
-- accounts.default_program_id (deferred from 00005 until programs exists, D24):
-- an optional default program, MEANINGFUL ONLY on revenue/expense accounts
-- (store-enforced next -- prefills a split's required program_id, else the root).
-- Nullable, no default, so the ALTER is legal on the existing (user-created)
-- account rows, which predate the column and stay NULL.
-- ---------------------------------------------------------------------------
ALTER TABLE accounts ADD COLUMN default_program_id INTEGER REFERENCES programs(id);

-- Versioning ripple: accounts_versions currently snapshots the pre-p07.1 account
-- columns. Adding default_program_id to accounts means the account version
-- snapshot must ALSO capture it, or Z3 (current == latest snapshot) diverges for
-- any account touched after this migration. Add the column to the twin (nullable,
-- no default -- existing version rows predate it and stay NULL, which is fine for
-- pre-existing entities). The store's InsertAccountVersion query is updated to
-- select default_program_id too, keeping live and latest-snapshot consistent
-- going forward.
ALTER TABLE accounts_versions ADD COLUMN default_program_id INTEGER;
