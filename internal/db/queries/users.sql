-- p06.1 adds InsertUser + InsertUserVersion. Keep this file PURE ASCII: sqlc
-- v1.31.1 miscounts byte offsets on multi-byte UTF-8 and corrupts the WHOLE
-- file's generated SQL (see docs/DECISIONS.md p04.2).

-- name: GetUser :one
SELECT id, username, display_name, created_at, disabled_at
FROM users
WHERE id = ?;

-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: CountHumanUsers :one
-- Bootstrap hint (p06.4): count real operators, excluding the seeded system
-- user (id 1). Zero means the operator still needs to create the first account,
-- so serve logs the `cuento user add ... --admin` hint on start.
SELECT COUNT(*) FROM users WHERE id <> 1;

-- name: UserIDByUsername :one
-- CLI lookup (p06.4): passwd/disable take a username; resolve it to the id the
-- versioned store methods need. A missing username is sql.ErrNoRows the CLI maps
-- to a clean "no such user" error.
SELECT id FROM users WHERE username = ?;

-- name: SetUserPassword :exec
-- Live update of a user's password_hash (p06.4 `user passwd`). The version append
-- (InsertUserVersion) that follows DELIBERATELY omits password_hash (rule 5), so
-- the new secret enters only the live table, never the audit trail.
UPDATE users SET password_hash = ? WHERE id = ?;

-- name: SetUserDisabled :exec
-- Live update of a user's disabled_at (p06.4 `user disable`). A disabled user
-- cannot log in (login enforces this). Versioned as op='update'; disabled_at IS
-- part of the users_versions snapshot, so the audit trail records the disabling.
UPDATE users SET disabled_at = ? WHERE id = ?;

-- name: UserByUsername :one
-- Login lookup (p06.2). Returns the credential + the columns the auth/lang
-- middleware needs: password_hash (nullable; the system user has none),
-- disabled_at (a disabled user cannot log in), and locale (drives the
-- post-login UI language). A NULL row (no such username) is a sql.ErrNoRows the
-- caller maps to the SAME uniform error as a wrong password (no user
-- enumeration, rule 13).
SELECT id, password_hash, disabled_at, locale
FROM users
WHERE username = ?;

-- name: UserByID :one
-- Session-resolution lookup (p06.2): the middleware turns a stored user id back
-- into the current identity + its UI language on every authenticated request.
-- Kept separate from GetUser (whose projection is pinned by
-- sqlc/users_changes_test.go, p06.1) so this step touches no existing query.
SELECT id, username, disabled_at, txn_perm, is_admin, locale
FROM users
WHERE id = ?;

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
