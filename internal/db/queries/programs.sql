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

-- ---------------------------------------------------------------------------
-- program merge (p11.5b) -- fold a SOURCE program into a DESTINATION program:
-- repoint every reference from src to dst (transaction splits' program_id, the
-- program-subtree scope on report grants, and any child program's parent_id),
-- versioning each moved row op='update', then deactivate src. Mirrors the account
-- merge (p08.5): dst is never written; src keeps its history (active=0), so
-- pre-merge snapshots that carry program_id/parent_id = src still resolve.
-- ---------------------------------------------------------------------------

-- name: SplitIdsByProgram :many
-- All split ids currently on a program, oldest first. Used by MergeProgram to
-- repoint each split's program_id individually and version it (snapshot-from-live),
-- exactly as SplitIdsByAccount does for the account merge. NOT filtered by
-- transaction.deleted: a merge clears the source program entirely so its history
-- reads coherently. Captured BEFORE any repoint write so the moved rows are not
-- confused with the destination's pre-existing splits.
SELECT id FROM splits WHERE program_id = ? ORDER BY id;

-- name: RepointSplitProgram :exec
-- Move ONE split to a new program_id (the merge repoint). The store versions the
-- split op='update' AFTER this so the snapshot-from-live row records program_id =
-- the destination. id last.
UPDATE splits SET program_id = ? WHERE id = ?;

-- name: ProgramChildIDs :many
-- The ids of the DIRECT children of a program (parent_id = ?), oldest first. Used
-- by MergeProgram to reparent each child onto the destination and version it. The
-- store guards dst against being src or a descendant of src (ErrCycle) BEFORE this,
-- so reparenting the children under dst can never form a cycle.
SELECT id FROM programs WHERE parent_id = ? ORDER BY id;

-- name: RepointProgramParent :exec
-- Reparent ONE program to a new parent_id (the merge child-repoint). The store
-- versions the program op='update' AFTER this. id last.
UPDATE programs SET parent_id = ? WHERE id = ?;

-- name: ReportGrantsByProgram :many
-- Every (user_id, group_name) grant currently scoped to a program (program_id = ?).
-- MergeProgram re-scopes each to the destination: because the program scope is a
-- mutable ATTRIBUTE (not part of the (user, group) key), a re-scope is a
-- delete+create version pair (no 'update' op, mirroring GrantReportGroup) -- so the
-- store snapshots op='delete' (old scope) BEFORE the live update and op='create'
-- (new scope) AFTER. Two grants for the same (user, group) can never both exist
-- (the PK forbids it), so repointing program_id never collides.
SELECT user_id, group_name FROM user_report_grants
WHERE program_id = ? ORDER BY user_id, group_name;

-- name: RepointReportGrantProgram :exec
-- Move ONE grant's program scope to a new program_id (the merge re-scope). The
-- store snapshots op='delete' BEFORE this and op='create' AFTER (a scope change is
-- delete+create, no 'update' op). Keyed on (user_id, group_name); program_id first.
UPDATE user_report_grants SET program_id = ? WHERE user_id = ? AND group_name = ?;

-- name: CountSplitsForProgram :one
-- How many splits currently carry this program_id (p11.5b). The merge preview
-- reports it as the number of transaction lines that will move to the destination,
-- reusing the SAME predicate MergeProgram repoints from so the preview never lies.
SELECT COUNT(*) FROM splits WHERE program_id = ?;

-- name: CountFundScopeBlockingSplits :one
-- How many splits on the SOURCE program (program_id = src) would leave their fund's
-- program scope if repointed to the DESTINATION (p11.5b Z15b block-guard). A split
-- tagged a FUND whose program scope R is set must carry a program inside R's subtree
-- (D20, Z15b; the same rule PostTransaction enforces as ErrFundProgramScope). Merging
-- src into dst repoints the split's program to dst, so the merge is REFUSED when any
-- such split's fund program scope R does NOT contain dst -- otherwise the repoint would
-- write a Z15b hole (nothing at the trigger layer guards the fund-subtree scope). Full
-- fund-scope repointing is out of scope (mirrors the account merge's recon backlog).
-- Params: dst (the candidate destination), src (the source program).
SELECT COUNT(*)
FROM splits s
JOIN funds f ON f.id = s.fund_id
WHERE s.program_id = @src
  AND f.program_id IS NOT NULL
  AND @dst NOT IN (
    WITH RECURSIVE sub(id) AS (
      SELECT f.program_id
      UNION ALL
      SELECT p.id FROM programs p JOIN sub ON p.parent_id = sub.id
    )
    SELECT id FROM sub);
