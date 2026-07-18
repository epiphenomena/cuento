-- +goose Up
-- p27.4: program-subtree scope on report grants (D10, budget-redesign DECISIONS
-- "Program-tree-scoped report permissions"). Forward-only; never edit an applied
-- migration; no Down (AGENTS rule 4).
--
-- A report grant is (user, report-group). This migration adds an OPTIONAL
-- program-subtree SCOPE: program_id (nullable FK to programs). A grant scoped to a
-- program node covers that node AND all its descendants (hierarchical, resolved via
-- ProgramSubtreeIDs at report time). program_id NULL = UNSCOPED = org-wide (the
-- current behavior). This narrows the ROWS a program-dimensioned report returns for
-- the grantee; it does NOT change route reachability except that a PURELY
-- program-scoped grant does not grant a non-program report (which cannot be filtered
-- by program) -- that denial lives in the enforcement policy (routes.go decide), not
-- the schema.
--
-- Model choice (DECISIONS): ONE scope per (user, group). The PK stays
-- (user_id, group_name); program_id is a nullable ATTRIBUTE, NOT part of the key.
-- This keeps a single grant row per group (a scope CHANGE is revoke+grant, no
-- 'update' op, mirroring the existing membership convention) and sidesteps the
-- SQLite "NULL columns in a composite PRIMARY KEY do not deduplicate" hazard (two
-- unscoped grants would not collide if NULL were in the key).
--
-- Versioning ripple (rule 5): user_report_grants_versions must ALSO snapshot
-- program_id, or Z3 (current == latest snapshot) diverges for any grant written
-- after this migration. The twin gets the same column (nullable, no default --
-- pre-existing snapshots predate it and stay NULL, which is correct: every grant
-- before this migration was unscoped). The Z3 membership block gains an
-- IS-comparison of program_id (NULL-safe), mirroring the budget_plans single-id
-- block's `v.x IS NOT c.x` pattern.
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the p04.2
-- byte-offset quirk applies here too).

ALTER TABLE user_report_grants ADD COLUMN program_id INTEGER REFERENCES programs(id);

ALTER TABLE user_report_grants_versions ADD COLUMN program_id INTEGER;
