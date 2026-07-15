-- Per-split description autocomplete + per-row prefill (p26.18). Step 4a of the
-- payee->per-split-description migration: the server backend feeding the entry-grid
-- description field (the UI wires it in 4b). Both are READ-ONLY (rule 2 permits
-- reads via sqlc); the web handlers render what these return verbatim.

-- name: SuggestDescriptions :many
-- Autocomplete ranking (p26.18): DISTINCT non-empty splits.description values whose
-- text SUBSTRING-matches the query (case-insensitive; splits.description is a plain
-- TEXT column, LIKE is case-insensitive for ASCII), across NON-DELETED transactions
-- only, ranked MOST-RECENTLY-USED first (greatest transaction date, then greatest
-- split id as the recency tiebreak). Limited to 10.
-- p26.38: descriptions are SCOPED to the given subsidiary (FILTER, not prefer) so an
-- entry in subsidiary A never surfaces subsidiary B's descriptions (their accounts
-- differ per sub, so a cross-sub prefill would carry an out-of-sub account). sub=0
-- means "no subsidiary chosen yet" -> UNSCOPED (all subs), so the `? = 0 OR ...` guard
-- passes the subsidiary id TWICE. The pattern is the caller-built LIKE pattern ('%' +
-- query + '%'); the store neutralizes LIKE metacharacters (% _ \) in the raw query
-- before wrapping (sqlc's parser rejects an explicit ESCAPE clause, so escaping is done
-- in Go against the default backslash-free LIKE -- see the store). Params, in appearance
-- order: description LIKE pattern, then the subsidiary id TWICE.
SELECT s.description
FROM splits s
JOIN transactions t ON t.id = s.transaction_id AND t.deleted = 0
WHERE s.description <> '' AND s.description LIKE ?
  AND (? = 0 OR t.subsidiary_id = ?)
GROUP BY s.description
ORDER BY MAX(t.date) DESC, MAX(s.id) DESC
LIMIT 10;

-- name: PrefillDescription :one
-- Per-row prefill (p26.18): the MOST-RECENT non-deleted split whose description
-- EQUALS the query exactly. Returns the split's account/amount/fund/program/class/
-- memo plus its transaction currency (the amount's true minor-unit scale -- the
-- endpoint has no in-progress-txn currency, so the matched split's own currency
-- drives the money formatter, mirroring payeeTemplate). Returns no rows when no
-- split carries that exact description.
-- p26.38: SCOPED to the given subsidiary (FILTER, not prefer) so a prefill in
-- subsidiary A never pulls subsidiary B's account/fund/program (they differ per sub).
-- sub=0 (no subsidiary chosen yet) is UNSCOPED, so the `? = 0 OR ...` guard passes the
-- subsidiary id TWICE. Params, in appearance order: the exact description, then the
-- subsidiary id TWICE.
SELECT s.account_id, s.amount, s.fund_id, s.program_id, s.functional_class, s.memo, t.currency
FROM splits s
JOIN transactions t ON t.id = s.transaction_id AND t.deleted = 0
WHERE s.description = ?
  AND (? = 0 OR t.subsidiary_id = ?)
ORDER BY t.date DESC, s.id DESC
LIMIT 1;
