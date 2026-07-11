-- name: ListCurrencies :many
-- ORDER BY code for deterministic output (tests key by code, not order).
SELECT code, exponent, symbol, name, active
FROM currencies
ORDER BY code;

-- name: GetCurrency :one
SELECT code, exponent, symbol, name, active
FROM currencies
WHERE code = ?;
