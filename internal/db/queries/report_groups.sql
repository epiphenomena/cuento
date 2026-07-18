-- report_groups is code-declared reference data (D10), synced to the db at
-- startup (p06.3). It is NOT a versioned business table, so the sync is a plain
-- idempotent upsert -- outside the write funnel, like currencies (rule 2 permits
-- reads and reference-data upserts via sqlc). ASCII only (p04.2 sqlc quirk).

-- name: UpsertReportGroup :exec
-- Idempotent insert-or-update of one code-declared group, keyed by its name.
-- Safe to run on every boot: an existing group has its sort refreshed, a new one
-- is created. No changes/version wiring -- reference data has no audit twin.
INSERT INTO report_groups (name, sort)
VALUES (?, ?)
ON CONFLICT(name) DO UPDATE SET sort = excluded.sort;

-- name: ReportGrantsForUser :many
-- The report groups a user has been granted read access to, WITH each grant's
-- optional program-subtree scope (D10, p27.4). program_id NULL = unscoped
-- (org-wide). Read by the permission-enforcement middleware ONLY when a route's
-- Perm is ReportGroup, so it never taxes the anonymous / non-report hot path.
-- ORDER BY for determinism.
SELECT group_name, program_id
FROM user_report_grants
WHERE user_id = ?
ORDER BY group_name;

-- name: ListReportGroups :many
-- All code-declared report groups, in their declared sort order (p13.2): the set
-- of grant checkboxes the admin per-user page offers. A read (reference data).
SELECT name, sort FROM report_groups ORDER BY sort, name;

-- name: GetReportGrantScope :one
-- The program-subtree scope of an existing (user, group) grant (p27.4): program_id
-- NULL = unscoped. Returns no rows when the grant does not exist. Grant management
-- reads this to decide whether a re-grant is a true no-op (same scope) or a scope
-- CHANGE (revoke+grant), since the composite key is (user, group) -- the scope is a
-- mutable attribute, not part of the key.
SELECT program_id FROM user_report_grants
WHERE user_id = ? AND group_name = ?;

-- name: HasReportGrant :one
-- 1 if the user already holds the grant. Grant management guards with this first
-- so a re-grant is a no-op with no duplicate PK and no spurious version row --
-- mirroring HasAccountSubsidiary (the composite-membership pattern).
SELECT COUNT(*) FROM user_report_grants
WHERE user_id = ? AND group_name = ?;

-- name: InsertReportGrant :exec
-- Add one (user_id, group_name) grant with an optional program-subtree scope
-- (program_id NULL = unscoped). Callers guard with HasReportGrant first (membership
-- is a set keyed on user+group; the PK forbids duplicates -- a scope CHANGE is a
-- revoke+grant, not an update). The version append that follows (op='create')
-- snapshots this row (incl. program_id) under the acting admin's change.
INSERT INTO user_report_grants (user_id, group_name, program_id)
VALUES (?, ?, ?);

-- name: DeleteReportGrant :exec
-- Remove one grant. For op=delete the version row is captured BEFORE this runs
-- (the live row must still exist to snapshot) -- the removal-op ordering the
-- account-subsidiaries store path documents.
DELETE FROM user_report_grants
WHERE user_id = ? AND group_name = ?;

-- name: InsertReportGrantVersion :exec
-- Snapshot-from-live version append for a COMPOSITE (user_id, group_name) grant
-- (00006 twin: entity_id = user_id, snapshot group_name + program_id, p27.4). For
-- op='create' this runs AFTER the live insert; for op='delete' BEFORE the live
-- delete (the row must still exist to snapshot -- so program_id is read from the
-- still-live row). Params (positional): op, change_id, user_id, group_name ->
-- generated fields Op, ID, UserID, GroupName. Mirrors
-- InsertAccountSubsidiaryVersion. ASCII only (p04.2 sqlc quirk).
INSERT INTO user_report_grants_versions
  (entity_id, change_id, valid_from, op, group_name, program_id)
SELECT g.user_id, c.id, c.at, ?, g.group_name, g.program_id
FROM user_report_grants g, changes c
WHERE c.id = ? AND g.user_id = ? AND g.group_name = ?;
