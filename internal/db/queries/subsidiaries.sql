-- p04.2: subsidiary operations. All SQL for the store's subsidiary methods lives
-- here (rule 6). The version-append convention established here is THE pattern for
-- every later versioned business table (programs p07, funds p07, accounts p05,
-- transactions/splits p08): the entity op does the live write inside the funnel's
-- fn, then appends a snapshot-from-live version row via InsertSubsidiaryVersion,
-- so the version row is byte-identical to the live row (Z3 can never diverge) and
-- valid_from == changes.at BY CONSTRUCTION (both structural, never asserted).
--
-- NOTE: keep every comment and identifier in this file pure ASCII. sqlc v1.31.1
-- miscounts byte offsets when a query file contains multi-byte UTF-8, corrupting
-- the generated SQL for the WHOLE file (see docs/DECISIONS.md p04.2).

-- name: InsertSubsidiary :one
-- Live insert of a CHILD. parent_id is validated present/valid in the store
-- (ErrSecondRoot / ErrParentMissing) BEFORE this runs, so a second root never
-- reaches the trigger. Returns the new id for the store to snapshot + return.
INSERT INTO subsidiaries (parent_id, name, base_currency, active, sort_order)
VALUES (?, ?, ?, ?, ?)
RETURNING id;

-- name: GetSubsidiary :one
SELECT id, parent_id, name, base_currency, active, sort_order
FROM subsidiaries
WHERE id = ?;

-- name: UpdateSubsidiary :exec
-- Live update: rename / move (parent) / change base_currency / active / sort.
-- The store reads the current row (GetSubsidiary), overrides the caller's fields,
-- and writes the full desired state here, keeping snapshot-from-live trivial.
UPDATE subsidiaries
SET parent_id = ?, name = ?, base_currency = ?, active = ?, sort_order = ?
WHERE id = ?;

-- name: InsertSubsidiaryVersion :exec
-- The snapshot-from-live version append (rule 5, D4). Reads the CURRENT live row
-- (so it must run AFTER the live write) and copies every business column, so the
-- version row is byte-identical to the live row; valid_from is taken from the
-- change's own `at`, so valid_from == changes.at BY CONSTRUCTION. Snapshot column
-- set matches 00004_subsidiaries.sql exactly.
--
-- Params are PLAIN POSITIONAL (?), each used exactly once so no numbered/named
-- form is needed: op, then change_id (bound in the WHERE, its id + at selected
-- into the row), then entity_id. sqlc's sqlite parser rejects sqlc.arg()/@name in
-- this INSERT...SELECT...WHERE form and mangles the file on numbered ?N, so the
-- generated struct fields are Op, ID (change_id = c.id), ID_2 (entity_id = s.id).
-- The store wraps that behind one insertSubsidiaryVersion helper so callers never
-- see the positional-naming quirk.
--
-- Composite-key versioned tables (account_names, account_subsidiaries,
-- fund_subsidiaries, later) copy this shape and change ONLY the entity_id
-- expression and the WHERE that selects the live row; the structure is identical.
INSERT INTO subsidiaries_versions
  (entity_id, change_id, valid_from, op, parent_id, name, base_currency, active, sort_order)
SELECT s.id, c.id, c.at, ?, s.parent_id, s.name, s.base_currency, s.active, s.sort_order
FROM subsidiaries s, changes c
WHERE c.id = ? AND s.id = ?;

-- name: CountActiveChildren :one
-- Active children of a subsidiary. DeactivateSubsidiary is blocked while > 0
-- (ErrHasActiveChildren): deactivating a parent with live descendants would
-- orphan active subs behind an inactive one.
SELECT COUNT(*) FROM subsidiaries
WHERE parent_id = ? AND active = 1;

-- name: SubTree :many
-- All subsidiaries in DEPTH-FIRST (pre-order) order. A materialized path of
-- zero-padded (sort_order, id) pairs per level makes ORDER BY path a true
-- pre-order traversal; ordering by a depth counter would give breadth-first.
-- Children are ordered by sort_order then id. 20-digit zero-pad safely holds any
-- realistic sort_order/id.
WITH RECURSIVE tree(id, parent_id, name, base_currency, active, sort_order, path) AS (
  SELECT s.id, s.parent_id, s.name, s.base_currency, s.active, s.sort_order,
         printf('%020d.%020d', s.sort_order, s.id)
  FROM subsidiaries s
  WHERE s.parent_id IS NULL
  UNION ALL
  SELECT s.id, s.parent_id, s.name, s.base_currency, s.active, s.sort_order,
         t.path || '/' || printf('%020d.%020d', s.sort_order, s.id)
  FROM subsidiaries s
  JOIN tree t ON s.parent_id = t.id
)
SELECT tree.id, tree.parent_id, tree.name, tree.base_currency, tree.active, tree.sort_order
FROM tree
ORDER BY tree.path;

-- name: Descendants :many
-- Self + transitive closure of a subsidiary (the primitive report scoping uses,
-- D18). Self is the recursive base case, so it is ALWAYS included; the store's
-- cycle check (new parent must not be self nor any descendant) relies on that.
WITH RECURSIVE sub(id, parent_id, name, base_currency, active, sort_order) AS (
  SELECT s.id, s.parent_id, s.name, s.base_currency, s.active, s.sort_order
  FROM subsidiaries s
  WHERE s.id = ?
  UNION ALL
  SELECT s.id, s.parent_id, s.name, s.base_currency, s.active, s.sort_order
  FROM subsidiaries s
  JOIN sub ON s.parent_id = sub.id
)
SELECT sub.id, sub.parent_id, sub.name, sub.base_currency, sub.active, sub.sort_order
FROM sub;
