-- p17.2: bank-CSV-import store SQL (upload + mapping + staging). All SQL for the
-- import store methods lives here (rule 6). These three tables are NON-VERSIONED
-- OPERATIONAL/STAGING tables (DECISIONS p17.1): like currencies/report_groups they
-- have NO *_versions twin, so there is NO snapshot-from-live version append here.
-- The mutations STILL run through the store write funnel (rule 2) -- one changes
-- row anchors the tx boundary + actor -- exactly like PutRates and
-- SetSplitReconciled; only the version-append step is skipped (no twin to write).
--
-- The dedupe_hash = sha256(account|date|amount|normalized(payee+memo)) is computed
-- in Go (internal/bankimport.DedupeHash); the SQL here only STORES it and provides
-- the two LOOKUP sources a staging pass consults to FLAG (never enforce) a
-- duplicate: (1) prior staged import_rows on the account, and (2) the natural keys
-- of already-posted ledger splits on the account (re-derived in Go to the identical
-- hash).

-- name: InsertMappingProfile :one
-- Save a reusable CSV column-mapping. config is the opaque JSON blob the store
-- encodes with encoding/json (the store owns the shape; the schema stores TEXT).
INSERT INTO mapping_profiles (name, config)
VALUES (?, ?)
RETURNING id;

-- name: GetMappingProfile :one
SELECT id, name, config FROM mapping_profiles WHERE id = ?;

-- name: ListMappingProfiles :many
-- The saved profiles the mapping UI offers for reuse, newest first. Soft-deleted
-- (deactivated) profiles are excluded (p26.63); a batch's profile_id FK still
-- resolves to the deactivated row (its audit), it is just no longer offered.
SELECT id, name, config FROM mapping_profiles WHERE active = 1 ORDER BY id DESC;

-- name: DeactivateMappingProfile :execrows
-- Soft-delete a saved profile: flip active to 0 so it drops out of the load list.
-- No row is deleted (import_batches.profile_id keeps referencing it). Returns the
-- affected row count so the store can distinguish a missing/already-gone profile.
UPDATE mapping_profiles SET active = 0 WHERE id = ? AND active = 1;

-- name: InsertImportBatch :one
-- One upload, binding ONE account AND ONE subsidiary (the account-maps-to-subsidiary
-- check is done in the store via HasAccountSubsidiaryMap before this runs).
INSERT INTO import_batches (filename, account_id, subsidiary_id, profile_id, uploaded_by, uploaded_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetImportBatch :one
SELECT id, filename, account_id, subsidiary_id, profile_id, uploaded_by, uploaded_at
FROM import_batches WHERE id = ?;

-- name: InsertImportRow :one
-- Stage one parsed row. account_id is denormalized from the batch (the dedupe
-- scope, DECISIONS p17.1). status is 'pending'; dedupe_hash is precomputed in Go.
INSERT INTO import_rows
  (batch_id, account_id, raw_json, parsed_date, parsed_amount, parsed_payee, parsed_memo, status, dedupe_hash)
VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?)
RETURNING id;

-- name: ImportRowsByBatch :many
-- Every staged row of a batch, in stage order -- the batch review list source.
SELECT id, batch_id, account_id, raw_json, parsed_date, parsed_amount, parsed_payee,
       parsed_memo, status, dedupe_hash, posted_transaction_id
FROM import_rows
WHERE batch_id = ?
ORDER BY id;

-- name: GetImportRow :one
-- One staged row joined to its batch, for the p17.3 review queue (edit&post /
-- discard). It carries the batch's subsidiary_id (the SUB the edit&post editor LOCKS)
-- and the row's own account_id (denormalized from the batch = the bank side).
SELECT r.id, r.batch_id, r.account_id, r.raw_json, r.parsed_date, r.parsed_amount,
       r.parsed_payee, r.parsed_memo, r.status, r.dedupe_hash, r.posted_transaction_id,
       b.subsidiary_id AS subsidiary_id
FROM import_rows r
JOIN import_batches b ON b.id = r.batch_id
WHERE r.id = ?;

-- name: MarkImportRowPosted :exec
-- p17.3: LINK a staged row to the ledger transaction that posted it (status=posted,
-- posted_transaction_id set). Guarded to status='pending' so a double-submit does not
-- re-link an already-posted/discarded row (the store also re-reads status first).
UPDATE import_rows
SET status = 'posted', posted_transaction_id = ?
WHERE id = ? AND status = 'pending';

-- name: MarkImportRowDiscarded :exec
-- p17.3: mark a staged row discarded (status=discarded). The DISCARD REASON is the
-- `changes` row's note (DECISIONS p17.1: a discarded row's audit is that change);
-- there is no discard_reason column by design. Guarded to status='pending'.
UPDATE import_rows
SET status = 'discarded'
WHERE id = ? AND status = 'pending';

-- name: PendingOrPostedDedupeHashes :many
-- Dedupe LOOKUP source (1): the dedupe_hashes of import rows ALREADY staged
-- (pending) or posted on this account -- across ALL batches (a re-upload is a new
-- batch), which is what makes cross-batch duplicate flagging work. Discarded rows
-- are excluded (a discarded row is not a live duplicate to re-flag against).
SELECT dedupe_hash FROM import_rows
WHERE account_id = ? AND status IN ('pending','posted');

-- name: LedgerSplitDedupeKeys :many
-- Dedupe LOOKUP source (2): the natural keys of already-posted LEDGER splits on
-- this account, on NON-DELETED transactions. Each row yields (date, amount,
-- description, memo) which the store re-hashes in Go with bankimport.DedupeHash to
-- the SAME hash a matching bank row would produce -- so a bank line that is already
-- a posted transaction is flagged. The split MEMO is used, with the transaction
-- memo as the fallback when the split memo is empty (documented in the store);
-- description is the split's per-line free text (p26.20 -- it replaces the retired
-- payee name; the bank-import write path sets it on the bank-account split).
SELECT t.date        AS date,
       s.amount      AS amount,
       s.description AS description,
       s.memo        AS split_memo,
       t.memo        AS txn_memo
FROM splits s
JOIN transactions t ON t.id = s.transaction_id
WHERE s.account_id = ? AND t.deleted = 0;
