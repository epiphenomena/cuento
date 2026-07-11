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
-- The report groups a user has been granted read access to (D10). Read by the
-- permission-enforcement middleware ONLY when a route's Perm is ReportGroup, so
-- it never taxes the anonymous / non-report hot path. ORDER BY for determinism.
SELECT group_name
FROM user_report_grants
WHERE user_id = ?
ORDER BY group_name;
