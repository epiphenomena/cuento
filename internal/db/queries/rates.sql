-- exchange_rates queries (p14.1). ASCII only, POSITIONAL ? params (p04.2 sqlc
-- quirk). exchange_rates is change-id-anchored reference data (NOT versioned): the
-- INSERT carries the funnel's changeID directly, there is no snapshot-from-live
-- version append, so no INSERT...SELECT and no numbered-param concern.

-- name: InsertRate :exec
-- One rate row. Called in a loop by PutRates, once per pair in a batch, all under
-- ONE change_id (the funnel binds the batch to a single changes row). ON CONFLICT
-- makes a re-load of an existing (date, base, quote) idempotent -- it refreshes
-- the rate/source and re-anchors change_id to the loading batch (a ratesync retry
-- or a manual correction over the same day/pair overwrites cleanly rather than
-- PK-colliding).
INSERT INTO exchange_rates (rate_date, base, quote, rate, source, change_id)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(rate_date, base, quote) DO UPDATE SET
  rate = excluded.rate, source = excluded.source, change_id = excluded.change_id;

-- name: RateOnOrBefore :one
-- The rate for base->quote effective ON OR BEFORE the given date: the row with the
-- greatest rate_date <= the target. Returns rate AND its ACTUAL rate_date so a
-- report can footnote staleness ("as of <rate_date>"). sql.ErrNoRows when no row
-- for the pair is on-or-before the date; the store turns that into the reciprocal
-- fallback or ErrRateMissing.
SELECT rate_date, rate
FROM exchange_rates
WHERE base = ? AND quote = ? AND rate_date <= ?
ORDER BY rate_date DESC
LIMIT 1;
