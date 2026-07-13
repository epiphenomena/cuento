-- +goose Up
-- p17.1: bank-CSV-import staging schema. Forward-only; never edit an applied
-- migration; no Down (AGENTS rule 4). Keep this file PURE ASCII (sqlc reads
-- migrations as its schema; the p04.2 byte-offset quirk applies).
--
-- These three tables are OPERATIONAL/STAGING reference data -- like currencies
-- and report_groups, NOT bitemporal ledger tables. So, by DECISION (DECISIONS
-- p17.1):
--   * NO *_versions twin, no changes-row wiring, no version trigger.
--   * They are NOT in the Z3/Z5 versioned-table UNIONs (which enumerate twins by
--     hardcoded name; no twin => not included), so `cuento check` stays clean.
-- The AUDIT is preserved WITHOUT versioning them: a POSTED row links to its real,
-- fully versioned ledger transaction (import_rows.posted_transaction_id -> the
-- audited transaction created in p17.3), and a DISCARDED row's audit is the
-- `changes` row p17.3 writes with a reason. The staging rows themselves are
-- disposable scaffolding around those two audited outcomes.
--
-- Create order (FK targets must pre-exist): mapping_profiles -> import_batches
-- (references accounts, subsidiaries, mapping_profiles, users) -> import_rows
-- (references import_batches, accounts, transactions).
--
-- The store lifecycle (upload, mapping, the dedupe_hash computation, staging,
-- posting, discard) is p17.2/p17.3. This step is schema + the dedupe index only,
-- exercised by direct-SQL tests (internal/db/import_test.go).

-- ---------------------------------------------------------------------------
-- mapping_profiles: a saved CSV column-mapping. config is JSON (TEXT), decoded
-- with stdlib encoding/json at the store layer (p17.2): which columns map to
-- date/amount/payee/memo, delimiter, sign handling, date format. Schema stores
-- it as opaque TEXT (no json_valid CHECK -- the store owns the shape).
-- ---------------------------------------------------------------------------
CREATE TABLE mapping_profiles (
  id     INTEGER PRIMARY KEY AUTOINCREMENT,   -- D17
  name   TEXT NOT NULL,
  config TEXT NOT NULL DEFAULT '{}'           -- JSON column-mapping (p17.2)
);

-- ---------------------------------------------------------------------------
-- import_batches: one CSV upload. A batch binds ONE target account AND ONE
-- subsidiary (D: the account must map to that subsidiary). That cross-table
-- account<->subsidiary membership is NOT a row-local property, so per rule 7 it
-- is validated in the store (p17.2 TestBatchSubValidated), NOT by a schema
-- trigger; here the binding is just two independent FKs.
-- ---------------------------------------------------------------------------
CREATE TABLE import_batches (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,                 -- D17
  filename      TEXT NOT NULL,
  account_id    INTEGER NOT NULL REFERENCES accounts(id),         -- target account
  subsidiary_id INTEGER NOT NULL REFERENCES subsidiaries(id),     -- target subsidiary (account must map to it: p17.2)
  profile_id    INTEGER NOT NULL REFERENCES mapping_profiles(id),
  uploaded_by   INTEGER NOT NULL REFERENCES users(id),
  uploaded_at   TEXT NOT NULL                                     -- RFC3339
);

-- ---------------------------------------------------------------------------
-- import_rows: one staged CSV row. raw_json is the original row as JSON; the
-- parsed_* columns are the mapped values (parsed_amount is INTEGER minor units,
-- rule 3 -- never a float). status flows pending -> posted | discarded.
--
-- account_id is DENORMALIZED from the batch (batch.account_id) so the dedupe
-- scope is a direct column, not a join through batch_id. This is what makes
-- dedupe PER ACCOUNT and lets it span batches (a re-upload is a new batch).
--
-- posted_transaction_id is NULL until the row is posted (p17.3), then points at
-- the audited ledger transaction it created -- this link is the row's audit.
--
-- Dedupe-scoping DECISION (DECISIONS p17.1): idx_import_rows_dedupe on
-- (account_id, dedupe_hash) is NON-UNIQUE. dedupe_hash =
-- sha256(account|date|amount|normalized payee+memo) is computed in p17.2. A
-- duplicate is DETECTED by a LOOKUP on this index, then FLAGGED -- deliberately
-- NOT enforced by a UNIQUE constraint, because a legitimate idempotent re-upload
-- (p17.3 TestReimportFlagsDuplicates) must be flagged, not crash at INSERT. The
-- composite (account_id, dedupe_hash) -- not a bare dedupe_hash -- is what scopes
-- detection per account: the same hash under two accounts is two distinct,
-- allowed rows.
-- ---------------------------------------------------------------------------
CREATE TABLE import_rows (
  id                    INTEGER PRIMARY KEY AUTOINCREMENT,             -- D17
  batch_id              INTEGER NOT NULL REFERENCES import_batches(id),
  account_id            INTEGER NOT NULL REFERENCES accounts(id),      -- denormalized from batch: dedupe scope
  raw_json              TEXT NOT NULL,                                 -- original row as JSON
  parsed_date           TEXT,                                          -- YYYY-MM-DD (NULL if unparsed)
  parsed_amount         INTEGER,                                       -- minor units (rule 3); NULL if unparsed
  parsed_payee          TEXT,
  parsed_memo           TEXT,
  status                TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','posted','discarded')),
  dedupe_hash           TEXT NOT NULL,                                 -- sha256(...) computed p17.2
  posted_transaction_id INTEGER REFERENCES transactions(id)           -- NULL until posted (p17.3); the row's audit link
);

-- Per-account dedupe LOOKUP index (NON-UNIQUE, see decision above).
CREATE INDEX idx_import_rows_dedupe ON import_rows(account_id, dedupe_hash);

-- Batch progress / row listing (p17.2/p17.3 read rows by batch and status).
CREATE INDEX idx_import_rows_batch ON import_rows(batch_id, status);
