-- +goose Up
-- p04.1: subsidiaries — the FIRST versioned business table (D18).
-- Forward-only; never edit an applied migration; no Down (AGENTS rule 4).
--
-- This migration is the TEMPLATE for every later versioned business table
-- (programs, accounts, funds, transactions, splits, ...). It establishes three
-- things that all of them repeat:
--   1. a versioned business table + its append-only *_versions twin (rule 5, D4);
--   2. schema triggers covering the row-local invariant subset (rule 7);
--   3. an AUDIT-CONSISTENT seed: a seeded versioned row also inserts its own
--      changes row and its op='create' *_versions row (valid_from == changes.at),
--      so the future Z3/Z5 integrity checks hold uniformly with no seed special-
--      casing.

-- The live table: latest denormalized state (D4). parent_id is NULL only for the
-- single root (enforced by trg_subsidiaries_single_root below).
CREATE TABLE subsidiaries (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,           -- D17
  parent_id     INTEGER REFERENCES subsidiaries(id),         -- NULL only for the single root
  name          TEXT NOT NULL UNIQUE,
  base_currency TEXT NOT NULL REFERENCES currencies(code),   -- D18: each sub has a base currency
  active        INTEGER NOT NULL DEFAULT 1,
  sort_order    INTEGER NOT NULL DEFAULT 0
);

-- Append-only version twin (rule 5, D4, Appendix A versions pattern). Never sees
-- UPDATE/DELETE. The SNAPSHOT COLUMN SET after the op column is:
--     parent_id, name, base_currency, active, sort_order
-- i.e. the full business-column set of `subsidiaries` (id/audit columns excluded).
-- p04.2's InsertSubsidiaryVersion query MUST write these exact columns/values so
-- the two version-emitting paths (this seed and the store) never diverge (Z3
-- correctness). Keep this list in lockstep if a business column is ever added.
CREATE TABLE subsidiaries_versions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id     INTEGER NOT NULL,                            -- the subsidiaries.id this snapshot is of
  change_id     INTEGER NOT NULL REFERENCES changes(id),
  valid_from    TEXT NOT NULL,                               -- equals changes.at (rule 5)
  op            TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of subsidiaries' business columns:
  parent_id     INTEGER,
  name          TEXT NOT NULL,
  base_currency TEXT NOT NULL,
  active        INTEGER NOT NULL,
  sort_order    INTEGER NOT NULL
);
CREATE INDEX subsidiaries_versions_entity ON subsidiaries_versions(entity_id, valid_from);

-- trg_subsidiaries_single_root: at most one row with parent_id IS NULL (D18).
-- Two triggers, because SQLite triggers fire per-row and there is no deferred
-- constraint: a BEFORE INSERT guard (reject a second root) AND a BEFORE UPDATE
-- guard (reject orphaning an existing row into a second root).
--
-- On BEFORE INSERT the new row is not yet in the table and its AUTOINCREMENT id
-- is unassigned (NEW.id IS NULL), so we check plain existence of any other root —
-- an `id <> NEW.id` clause would compare against NULL and never fire. On BEFORE
-- UPDATE the row already has an id, so we exclude it with `id <> NEW.id`.
-- +goose StatementBegin
CREATE TRIGGER trg_subsidiaries_single_root_insert
BEFORE INSERT ON subsidiaries
WHEN NEW.parent_id IS NULL
BEGIN
  SELECT RAISE(ABORT, 'subsidiaries: exactly one root allowed')
  WHERE EXISTS (SELECT 1 FROM subsidiaries WHERE parent_id IS NULL);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_subsidiaries_single_root_update
BEFORE UPDATE ON subsidiaries
WHEN NEW.parent_id IS NULL
BEGIN
  SELECT RAISE(ABORT, 'subsidiaries: exactly one root allowed')
  WHERE EXISTS (SELECT 1 FROM subsidiaries WHERE parent_id IS NULL AND id <> NEW.id);
END;
-- +goose StatementEnd

-- Seed the root subsidiary WITH full audit consistency (three rows, in order).
-- Deterministic literals only — no dynamic clock in a migration (rule 4). This
-- is change id 1 (changes is empty before this: 00002 seeds only `users`).
--
-- 1. the changes row: system actor (user 1), fixed epoch timestamp, kind 'seed'.
INSERT INTO changes (id, actor_id, at, kind, note)
VALUES (1, 1, '1970-01-01T00:00:00Z', 'seed', 'seed root subsidiary');

-- 2. the live root row (passes the single-root trigger: no other root yet).
INSERT INTO subsidiaries (id, parent_id, name, base_currency, active, sort_order)
VALUES (1, NULL, 'Organization', 'USD', 1, 0);

-- 3. the op='create' version row: valid_from EQUALS changes.at exactly, snapshot
--    columns identical to the live row.
INSERT INTO subsidiaries_versions
  (entity_id, change_id, valid_from, op, parent_id, name, base_currency, active, sort_order)
VALUES
  (1, 1, '1970-01-01T00:00:00Z', 'create', NULL, 'Organization', 'USD', 1, 0);
