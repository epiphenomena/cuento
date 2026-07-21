-- p07.3: fund operations (D20). All SQL for the store's fund methods lives here
-- (rule 6). This copies the version-append convention established in
-- subsidiaries.sql (p04.2) and the COMPOSITE-membership pattern from accounts.sql
-- (p05.2): the entity op does the live write inside the funnel's fn, then appends
-- a snapshot-from-live version row, so each version row is byte-identical to its
-- live row (Z3 can never diverge) and valid_from == changes.at BY CONSTRUCTION.
--
-- Funds are simpler than accounts: fund_subsidiaries is a FLAT set (>=1,
-- store-enforced) with NO ancestor propagation and NO superset invariant. A
-- membership add is op='create', a removal op='delete' (captured BEFORE the live
-- DELETE -- the removal-op ordering).
--
-- Query names are DISTINCT from the account ones (InsertFund vs InsertAccount,
-- FundSubsidiaries vs AccountSubsidiaries, ...): sqlc emits into one package, so a
-- name collision would fail generation. Program-existence validation reuses
-- GetProgram (programs.sql) -- not duplicated here.
--
-- NOTE: keep every comment and identifier in this file PURE ASCII. sqlc v1.31.1
-- miscounts byte offsets when a query file contains multi-byte UTF-8, corrupting
-- the generated SQL for the WHOLE file (see docs/DECISIONS.md p04.2).

-- name: InsertFund :one
-- Live insert of a fund. restriction is a CHECK enum; program_id/start_date/
-- end_date are optional (validated in the store where required). Returns the new
-- id for the store to snapshot + return.
INSERT INTO funds
  (name, funder, purpose, restriction, program_id, start_date, end_date, notes, active)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetFund :one
SELECT id, name, funder, purpose, restriction, program_id,
       start_date, end_date, notes, active
FROM funds
WHERE id = ?;

-- name: ListFunds :many
-- Every fund (active AND closed), id-ordered, for the register (p12.1): the
-- fund-name lookup (a chip may name a now-closed fund) and the fund-filter option
-- list. Unlike ActiveFunds this is NOT scoped to a subsidiary and includes closed
-- funds, because a historical split may reference either.
SELECT id, name, funder, purpose, restriction, program_id,
       start_date, end_date, notes, active
FROM funds
ORDER BY id;

-- name: UpdateFund :exec
-- Live update: rename / funder / purpose / restriction / program-scope / dates /
-- notes / active. The store reads the current row (GetFund), overrides the
-- caller's fields, and writes the full desired state here, keeping
-- snapshot-from-live trivial.
UPDATE funds
SET name = ?, funder = ?, purpose = ?, restriction = ?, program_id = ?,
    start_date = ?, end_date = ?, notes = ?, active = ?
WHERE id = ?;

-- name: InsertFundVersion :exec
-- Snapshot-from-live version append for funds (STANDARD single-column entity,
-- entity_id = funds.id). Reads the CURRENT live row (so it MUST run AFTER the live
-- write) and copies every business column, so the version row is byte-identical to
-- the live row; valid_from is taken from the change's own `at`. Snapshot column
-- set matches 00009_funds.sql exactly. Params (plain positional, each used once):
-- op, change_id, entity_id -> generated fields Op, ID (change_id), ID_2 (entity_id).
INSERT INTO funds_versions
  (entity_id, change_id, valid_from, op, name, funder, purpose, restriction,
   program_id, start_date, end_date, notes, active)
SELECT f.id, c.id, c.at, ?, f.name, f.funder, f.purpose, f.restriction,
       f.program_id, f.start_date, f.end_date, f.notes, f.active
FROM funds f, changes c
WHERE c.id = ? AND f.id = ?;

-- name: HasFundSubsidiary :one
-- 1 if the fund already maps the subsidiary. UpdateFund's set diff uses this to
-- skip re-adding an existing membership (no duplicate PK, no spurious version row).
SELECT COUNT(*) FROM fund_subsidiaries
WHERE fund_id = ? AND subsidiary_id = ?;

-- name: InsertFundSubsidiary :exec
-- Add one (fund_id, subsidiary_id) membership. Membership is a FLAT set (the PK
-- forbids duplicates); callers guard with HasFundSubsidiary first.
INSERT INTO fund_subsidiaries (fund_id, subsidiary_id)
VALUES (?, ?);

-- name: DeleteFundSubsidiary :exec
-- Remove one membership. For op=delete the version row is captured BEFORE this
-- runs (the live row must still exist to snapshot from) -- see the store's
-- removal-op ordering comment.
DELETE FROM fund_subsidiaries
WHERE fund_id = ? AND subsidiary_id = ?;

-- name: InsertFundSubsidiaryVersion :exec
-- Snapshot-from-live version append for a COMPOSITE (fund_id, subsidiary_id)
-- membership. entity_id = fund_id; subsidiary_id is both a snapshot column and
-- part of the entity identity (00009). For op='create' this runs AFTER the live
-- insert; for op='delete' it runs BEFORE the live delete (the row must still
-- exist to snapshot). Params (positional): op, change_id, fund_id, subsidiary_id
-- -> generated fields Op, ID, FundID, SubsidiaryID.
INSERT INTO fund_subsidiaries_versions
  (entity_id, change_id, valid_from, op, subsidiary_id)
SELECT fs.fund_id, c.id, c.at, ?, fs.subsidiary_id
FROM fund_subsidiaries fs, changes c
WHERE c.id = ? AND fs.fund_id = ? AND fs.subsidiary_id = ?;

-- name: FundSubsidiaries :many
-- The subsidiary id set a fund currently maps (order-insensitive; the store
-- builds a set for the diff).
SELECT subsidiary_id FROM fund_subsidiaries
WHERE fund_id = ?
ORDER BY subsidiary_id;

-- name: FundLedger :many
-- The fund STATEMENT (p12.5): every non-deleted split tagged fund_id, across ALL
-- accounts, ordered by the total order (date, split_id), with a per-currency
-- RUNNING BALANCE that tracks the fund's ASSET-side (unexpended) position -- the
-- SAME quantity FundBalancesAsOf returns and Z18 warns on. A running sum over ALL
-- of a fund's splits is identically zero (every txn nets to zero WITHIN the fund
-- group, D20/Z10), so the window sums ONLY the asset splits' amounts (a CASE gates
-- non-asset rows to 0) -- the closing running balance thus equals the fund's
-- FundBalancesAsOf balance per currency BY CONSTRUCTION. Every row is still shown
-- (across all accounts); only asset rows MOVE the balance.
--
-- The window runs over the WHOLE ordered set in SQL so the running balance is exact
-- (no Go recompute); there is no paging (a single fund's split set is bounded).
-- opening balance is 0 (the whole set is shown from the fund's first split).
--
-- IsAsset lets the caller render which rows moved the balance. The as-of bound
-- (t.date <= asof) MATCHES the list's FundBalancesAsOf as-of, so the closing running
-- balance equals the list balance even when a fund carries FUTURE-dated splits (a
-- post-dated payment): both pages agree on the same fund's closing balance BY
-- CONSTRUCTION. Params: fund_id, asof.
SELECT sp.id AS split_id, t.id AS txn_id, t.date, t.subsidiary_id, t.currency,
       sp.amount, sp.account_id,
       CASE WHEN a.type = 'asset' THEN 1 ELSE 0 END AS is_asset,
       sp.program_id, sp.functional_class,
       sp.memo AS split_memo, t.memo AS txn_memo, sp.description AS split_description,
       CAST(SUM(CASE WHEN a.type = 'asset' THEN sp.amount ELSE 0 END) OVER (
         PARTITION BY t.currency
         ORDER BY t.date, sp.id
         ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
       ) AS INTEGER) AS running_balance
FROM splits sp
JOIN transactions t ON t.id = sp.transaction_id
JOIN accounts a ON a.id = sp.account_id
WHERE sp.fund_id = ?
  AND t.deleted = 0
  AND t.date <= ?
ORDER BY t.date, sp.id;

-- name: ActiveFunds :many
-- Active funds whose subsidiary scope contains a given subsidiary (D20/Q1 -- the
-- transaction editor's option source). The JOIN + WHERE on subsidiary_id keeps
-- one row per fund (no dups). Ordered by id for a deterministic option list.
SELECT f.id, f.name, f.funder, f.purpose, f.restriction, f.program_id,
       f.start_date, f.end_date, f.notes, f.active
FROM funds f
JOIN fund_subsidiaries fs ON fs.fund_id = f.id
WHERE f.active = 1 AND fs.subsidiary_id = ?
ORDER BY f.id;
