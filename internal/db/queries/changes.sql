-- name: InsertChange :one
INSERT INTO changes (actor_id, at, kind, note)
VALUES (?, ?, ?, ?)
RETURNING id;

-- name: GetChange :one
SELECT id, actor_id, at, kind, note
FROM changes
WHERE id = ?;
