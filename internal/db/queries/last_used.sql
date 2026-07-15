-- p26.37 last-used header account. When a user opens a NEW transaction from the top
-- nav (no register origin), the header (balancing / position-0) account is prefilled
-- with the position-0 split account of the user's MOST-RECENTLY-ENTERED transaction, as
-- a data-entry convenience. READ-ONLY (rule 2 permits reads via sqlc).

-- name: LastHeaderAccountForActor :one
-- The position-0 split account of the transaction MOST RECENTLY CREATED by the given
-- actor. "Most recently entered" = the greatest create-change id authored by the actor
-- (change id is monotonic insertion order), NOT the transaction's business date -- a
-- backdated entry should not win. Only non-deleted transactions count. Joins the live
-- splits row (position 0) so the returned account reflects the transaction's current
-- header account. Returns no rows when the actor has entered no (non-deleted)
-- transaction. Param: the actor's user id.
SELECT s.account_id
FROM transactions_versions tv
JOIN changes c ON c.id = tv.change_id
JOIN transactions t ON t.id = tv.entity_id AND t.deleted = 0
JOIN splits s ON s.transaction_id = t.id AND s.position = 0
WHERE tv.op = 'create' AND c.actor_id = ?
ORDER BY c.id DESC
LIMIT 1;
