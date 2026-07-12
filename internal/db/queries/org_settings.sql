-- org_settings is static CONFIG reference data (p11.4): a plain key/value table,
-- NOT a versioned business table, so these are plain reads and an idempotent
-- upsert -- OUTSIDE the write funnel, like currencies/report_groups (rule 2 permits
-- reads and config upserts via sqlc). ASCII only (p04.2 sqlc quirk).

-- name: GetOrgSetting :one
-- The value for one config key. sql.ErrNoRows when the key is unset; the store
-- turns that into a caller-supplied default.
SELECT value FROM org_settings WHERE key = ?;

-- name: ListOrgSettings :many
-- Every config key/value, ordered by key for deterministic output.
SELECT key, value FROM org_settings ORDER BY key;

-- name: UpsertOrgSetting :exec
-- Idempotent insert-or-update of one config key. Admin writes are configuration,
-- not audited business mutations, so there is no changes/version wiring.
INSERT INTO org_settings (key, value)
VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value;
