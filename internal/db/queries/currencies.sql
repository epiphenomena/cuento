-- name: ListCurrencies :many
-- ORDER BY code for deterministic output (tests key by code, not order).
SELECT code, exponent, symbol, name, active
FROM currencies
ORDER BY code;

-- name: GetCurrency :one
SELECT code, exponent, symbol, name, active
FROM currencies
WHERE code = ?;

-- name: InsertCurrency :exec
-- Add a currency (p13.2 admin; used by FX in p14). currencies is STATIC reference
-- data (D1), NOT a versioned business table -- so this is a plain reference-data
-- write OUTSIDE the write funnel (no changes row, no *_versions twin), like
-- report_groups/org_settings (rule 2). ON CONFLICT keeps the add idempotent: a
-- re-add of an existing code refreshes its metadata + re-enables it rather than
-- erroring, so a same-worker e2e retry never PK-collides.
INSERT INTO currencies (code, exponent, symbol, name, active)
VALUES (?, ?, ?, ?, 1)
ON CONFLICT(code) DO UPDATE SET
  exponent = excluded.exponent, symbol = excluded.symbol,
  name = excluded.name, active = 1;

-- name: SetCurrencyActive :exec
-- Enable/disable a currency (p13.2 admin). A disabled currency is hidden from the
-- subsidiary base-currency picker but its historical rows keep their code. Plain
-- reference-data write, outside the funnel (currencies is not versioned).
UPDATE currencies SET active = ? WHERE code = ?;
