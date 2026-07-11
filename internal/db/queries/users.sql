-- p06.1 adds InsertUser + InsertUserVersion. Keep this file PURE ASCII: sqlc
-- v1.31.1 miscounts byte offsets on multi-byte UTF-8 and corrupts the WHOLE
-- file's generated SQL (see docs/DECISIONS.md p04.2).

-- name: GetUser :one
SELECT id, username, display_name, created_at, disabled_at
FROM users
WHERE id = ?;

-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: InsertUser :one
-- Live insert of a user. password_hash is nullable (a passwordless user, like
-- the system user, passes NULL). Settings columns are omitted so their schema
-- DEFAULTs apply; the version append reads them back from the live row. Returns
-- the new id for the store to snapshot + return.
INSERT INTO users (username, display_name, created_at, password_hash, is_admin, txn_perm)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: InsertUserVersion :exec
-- Snapshot-from-live version append for users (rule 5, D4). Runs AFTER the live
-- insert; copies every business column EXCEPT password_hash, which is
-- DELIBERATELY omitted so the audit trail never carries the secret (rule 5).
-- valid_from is the change's own `at`, so valid_from == changes.at BY
-- CONSTRUCTION. Snapshot column set matches 00006_credentials_perms.sql exactly.
--
-- Params are PLAIN POSITIONAL (?), each used once (op, change_id, entity_id), so
-- no numbered/named form is needed -- matching InsertSubsidiaryVersion's shape.
-- Generated struct fields: Op, ID (change_id = c.id), ID_2 (entity_id = u.id).
-- The store wraps that behind one insertUserVersion helper.
INSERT INTO users_versions
  (entity_id, change_id, valid_from, op,
   username, display_name, created_at, disabled_at, is_admin, txn_perm,
   locale, date_format, number_format, display_mode, neg_style, theme,
   default_subsidiary_id)
SELECT u.id, c.id, c.at, ?,
       u.username, u.display_name, u.created_at, u.disabled_at, u.is_admin, u.txn_perm,
       u.locale, u.date_format, u.number_format, u.display_mode, u.neg_style, u.theme,
       u.default_subsidiary_id
FROM users u, changes c
WHERE c.id = ? AND u.id = ?;
