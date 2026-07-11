-- name: GetUser :one
SELECT id, username, display_name, created_at, disabled_at
FROM users
WHERE id = ?;

-- name: CountUsers :one
SELECT COUNT(*) FROM users;
