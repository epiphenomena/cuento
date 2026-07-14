-- Per-split description autocomplete + per-row prefill (p26.18). Step 4a of the
-- payee->per-split-description migration: the server backend feeding the entry-grid
-- description field (the UI wires it in 4b). Both are READ-ONLY (rule 2 permits
-- reads via sqlc); the web handlers render what these return verbatim.

-- name: SuggestDescriptions :many
-- Autocomplete ranking (p26.18): DISTINCT non-empty splits.description values whose
-- text SUBSTRING-matches the query (case-insensitive; splits.description is a plain
-- TEXT column, LIKE is case-insensitive for ASCII), across NON-DELETED transactions
-- only, ranked MOST-RECENTLY-USED first (greatest transaction date, then greatest
-- split id as the recency tiebreak). Matches in the given subsidiary sort FIRST
-- (prefer, not filter -- a cross-sub description is still useful); when sub=0 the
-- CASE term is uniformly false and the order falls to pure recency. Limited to 10.
-- The pattern is the caller-built LIKE pattern ('%' + query + '%'); the store
-- neutralizes LIKE metacharacters (% _ \) in the raw query before wrapping (sqlc's
-- parser rejects an explicit ESCAPE clause, so escaping is done in Go against the
-- default backslash-free LIKE -- see the store). Params, in appearance order:
-- description LIKE pattern, then the subsidiary id.
SELECT s.description
FROM splits s
JOIN transactions t ON t.id = s.transaction_id AND t.deleted = 0
WHERE s.description <> '' AND s.description LIKE ?
GROUP BY s.description
ORDER BY MAX(CASE WHEN t.subsidiary_id = ? THEN 1 ELSE 0 END) DESC,
         MAX(t.date) DESC, MAX(s.id) DESC
LIMIT 10;

-- name: PrefillDescription :one
-- Per-row prefill (p26.18): the MOST-RECENT non-deleted split whose description
-- EQUALS the query exactly, preferring the given subsidiary (sub-match first), else
-- the most recent anywhere. Returns the split's account/amount/fund/program/class/
-- memo plus its transaction currency (the amount's true minor-unit scale -- the
-- endpoint has no in-progress-txn currency, so the matched split's own currency
-- drives the money formatter, mirroring payeeTemplate). Returns no rows when no
-- split carries that exact description. Params, in appearance order: the exact
-- description, then the subsidiary id.
SELECT s.account_id, s.amount, s.fund_id, s.program_id, s.functional_class, s.memo, t.currency
FROM splits s
JOIN transactions t ON t.id = s.transaction_id AND t.deleted = 0
WHERE s.description = ?
ORDER BY (t.subsidiary_id = ?) DESC, t.date DESC, s.id DESC
LIMIT 1;
