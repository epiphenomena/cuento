-- p07.1: program operations. All SQL for the store's program methods lives here
-- (rule 6). This copies the version-append convention established in
-- subsidiaries.sql (p04.2) MINUS base_currency: the entity op does the live write
-- inside the funnel's fn, then appends a snapshot-from-live version row via
-- InsertProgramVersion, so the version row is byte-identical to the live row (Z3
-- can never diverge) and valid_from == changes.at BY CONSTRUCTION.
--
-- Query names are DISTINCT from the subsidiary ones (InsertProgram vs
-- InsertSubsidiary, ProgramTree vs SubTree, ProgramDescendants vs Descendants,
-- CountActiveProgramChildren vs CountActiveChildren): sqlc emits into one package,
-- so a name collision would fail generation.
--
-- NOTE: keep every comment and identifier in this file PURE ASCII. sqlc v1.31.1
-- miscounts byte offsets when a query file contains multi-byte UTF-8, corrupting
-- the generated SQL for the WHOLE file (see docs/DECISIONS.md p04.2).

-- name: InsertProgram :one
-- Live insert of a CHILD. parent_id is validated present/valid in the store
-- (ErrProgramSecondRoot / ErrProgramParentMissing) BEFORE this runs, so a second
-- root never reaches the trigger. Returns the new id for the store to snapshot.
INSERT INTO programs (parent_id, name, active, sort_order)
VALUES (?, ?, ?, ?)
RETURNING id;

-- name: GetProgram :one
SELECT id, parent_id, name, active, sort_order
FROM programs
WHERE id = ?;

-- name: UpdateProgram :exec
-- Live update: rename / move (parent) / active / sort. The store reads the
-- current row (GetProgram), overrides the caller's fields, and writes the full
-- desired state here, keeping snapshot-from-live trivial.
UPDATE programs
SET parent_id = ?, name = ?, active = ?, sort_order = ?
WHERE id = ?;

-- name: InsertProgramVersion :exec
-- The snapshot-from-live version append (rule 5, D4). Reads the CURRENT live row
-- (so it must run AFTER the live write) and copies every business column, so the
-- version row is byte-identical to the live row; valid_from is taken from the
-- change's own `at`, so valid_from == changes.at BY CONSTRUCTION. Snapshot column
-- set matches 00008_programs.sql exactly.
--
-- Params are PLAIN POSITIONAL (?), each used once: op, then change_id (bound in
-- the WHERE, its id + at selected into the row), then entity_id. The generated
-- struct fields are Op, ID (change_id = c.id), ID_2 (entity_id = p.id); the store
-- wraps that behind one insertProgramVersion helper.
INSERT INTO programs_versions
  (entity_id, change_id, valid_from, op, parent_id, name, active, sort_order)
SELECT p.id, c.id, c.at, ?, p.parent_id, p.name, p.active, p.sort_order
FROM programs p, changes c
WHERE c.id = ? AND p.id = ?;

-- name: CountActiveProgramChildren :one
-- Active children of a program. DeactivateProgram is blocked while > 0
-- (ErrProgramHasActiveChildren): deactivating a parent with live descendants
-- would orphan active programs behind an inactive one.
SELECT COUNT(*) FROM programs
WHERE parent_id = ? AND active = 1;

-- name: ProgramTree :many
-- All programs in DEPTH-FIRST (pre-order) order. A materialized path of
-- zero-padded (sort_order, id) pairs per level makes ORDER BY path a true
-- pre-order traversal; children are ordered by sort_order then id.
WITH RECURSIVE tree(id, parent_id, name, active, sort_order, path) AS (
  SELECT p.id, p.parent_id, p.name, p.active, p.sort_order,
         printf('%020d.%020d', p.sort_order, p.id)
  FROM programs p
  WHERE p.parent_id IS NULL
  UNION ALL
  SELECT p.id, p.parent_id, p.name, p.active, p.sort_order,
         t.path || '/' || printf('%020d.%020d', p.sort_order, p.id)
  FROM programs p
  JOIN tree t ON p.parent_id = t.id
)
SELECT tree.id, tree.parent_id, tree.name, tree.active, tree.sort_order
FROM tree
ORDER BY tree.path;

-- name: ProgramDescendants :many
-- Self + transitive closure of a program (the primitive report scoping and the
-- move cycle check use, D24). Self is the recursive base case, so it is ALWAYS
-- included; the store's cycle check (new parent must not be self nor any
-- descendant) relies on that.
WITH RECURSIVE prog(id, parent_id, name, active, sort_order) AS (
  SELECT p.id, p.parent_id, p.name, p.active, p.sort_order
  FROM programs p
  WHERE p.id = ?
  UNION ALL
  SELECT p.id, p.parent_id, p.name, p.active, p.sort_order
  FROM programs p
  JOIN prog ON p.parent_id = prog.id
)
SELECT prog.id, prog.parent_id, prog.name, prog.active, prog.sort_order
FROM prog;
