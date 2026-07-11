-- +goose Up
-- p07.2: funds + scoping + versions (D20). Forward-only; never edit an applied
-- migration; no Down (AGENTS rule 4).
--
-- Restricted funds are a SPLIT DIMENSION with fund-level conservation (D20).
-- `funds` documents grants/restricted gifts (funder, purpose, restriction type,
-- dates). A fund scopes to ONE OR MORE subsidiaries via `fund_subsidiaries` (not
-- inherited by descendants; the >=1 minimum is store-enforced in p07.3, Q1) and
-- OPTIONALLY to a program subtree (program_id). Every split carries fund_id
-- (NULL = unrestricted); there is NO seeded "general fund" row -- unrestricted is
-- the absence of a fund_id, and its UI label comes from the i18n catalog. So,
-- unlike 00004/00008, this migration seeds NO row and therefore adds NO `changes`
-- row and NO seed version rows: the last change id stays 3 (00008's program seed).
--
-- Store operations (CreateFund/UpdateFund/CloseFund/ReopenFund/ActiveFunds and
-- the >=1-subsidiary rule) are p07.3; this step is schema only, exercised by
-- direct-SQL tests (internal/db/funds_test.go).
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the p04.2
-- byte-offset quirk applies here too -- 00008 established the ASCII convention).

-- ---------------------------------------------------------------------------
-- funds: the live grants/restricted-gifts table (D20). All rows are USER-CREATED
-- (no seed), so there is no seed-with-version audit consistency this step.
--
-- restriction is a fixed enum (CHECK) matching GAAP restriction kinds.
--
-- start_date / end_date are optional (nullable). WHEN NOT NULL each must match the
-- SAME YYYY-MM-DD digit shape as transactions.date (Appendix A) -- a loose,
-- shape-only GLOB (digit count + dashes), NULLs allowed. Adopting the identical
-- pattern keeps date validation consistent across the schema.
-- ---------------------------------------------------------------------------
CREATE TABLE funds (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,                 -- D17
  name        TEXT NOT NULL,
  funder      TEXT NOT NULL DEFAULT '',
  purpose     TEXT NOT NULL DEFAULT '',
  restriction TEXT NOT NULL CHECK (restriction IN ('purpose','time','perpetual')),
  program_id  INTEGER REFERENCES programs(id),                   -- optional program-subtree scope (D20)
  start_date  TEXT CHECK (start_date IS NULL OR start_date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  end_date    TEXT CHECK (end_date IS NULL OR end_date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  notes       TEXT NOT NULL DEFAULT '',
  active      INTEGER NOT NULL DEFAULT 1
);

-- fund_subsidiaries: the fund's subsidiary scope (D20). >=1 row per fund is
-- STORE-enforced (p07.3), not a schema constraint. A transaction's subsidiary
-- must be in its fund's set (Z13, enforced in p08).
CREATE TABLE fund_subsidiaries (
  fund_id       INTEGER NOT NULL REFERENCES funds(id),
  subsidiary_id INTEGER NOT NULL REFERENCES subsidiaries(id),
  PRIMARY KEY (fund_id, subsidiary_id)
);

-- ---------------------------------------------------------------------------
-- Version twins (rule 5, D4, Appendix A versions pattern). Append-only; never
-- UPDATE/DELETE. No seed rows this step (funds are all user-created), so no
-- seed-version consistency wiring. The version-append queries + AssertVersioned
-- extension land in p07.3; these tables define the exact snapshot shapes it must
-- write. As with accounts_versions, the twins carry NO CHECK/FK on the snapshot
-- columns (only change_id -> changes): a snapshot records history, it does not
-- re-validate it (an old snapshot of a since-invalidated value must still be
-- storable). The only referential FK is change_id.
--
-- funds_versions: STANDARD single-column entity (entity_id = funds.id). Snapshot
-- column set (must match `funds` business columns, id excluded), which p07.3's
-- InsertFundVersion must write EXACTLY:
--     name, funder, purpose, restriction, program_id, start_date, end_date,
--     notes, active
-- ---------------------------------------------------------------------------
CREATE TABLE funds_versions (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id   INTEGER NOT NULL,                                  -- funds.id
  change_id   INTEGER NOT NULL REFERENCES changes(id),
  valid_from  TEXT NOT NULL,                                     -- equals changes.at (rule 5)
  op          TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of funds' business columns (no CHECK/FK -- history, not live):
  name        TEXT NOT NULL,
  funder      TEXT NOT NULL,
  purpose     TEXT NOT NULL,
  restriction TEXT NOT NULL,
  program_id  INTEGER,
  start_date  TEXT,
  end_date    TEXT,
  notes       TEXT NOT NULL,
  active      INTEGER NOT NULL
);
CREATE INDEX funds_versions_entity ON funds_versions(entity_id, valid_from);

-- fund_subsidiaries_versions: COMPOSITE entity (fund_id, subsidiary_id), exactly
-- like account_subsidiaries_versions (00005). entity_id = fund_id; subsidiary_id
-- is BOTH the snapshot column AND part of the composite entity identity.
-- Membership is a set: adding a subsidiary to a fund appends op 'create',
-- removing it appends op 'delete' (there is NO 'update' for a pure membership
-- row). p07.3's InsertFundSubsidiaryVersion copies the account_subsidiaries
-- shape (snapshot-from-live), and its as-of / AssertVersioned must filter on
-- (entity_id, subsidiary_id). Snapshot column: subsidiary_id. The index carries
-- subsidiary_id so per-(fund, subsidiary) as-of lookups stay cheap.
CREATE TABLE fund_subsidiaries_versions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id     INTEGER NOT NULL,                                -- fund_subsidiaries.fund_id
  change_id     INTEGER NOT NULL REFERENCES changes(id),
  valid_from    TEXT NOT NULL,                                   -- equals changes.at (rule 5)
  op            TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- snapshot (subsidiary_id is also part of the composite identity):
  subsidiary_id INTEGER NOT NULL
);
CREATE INDEX fund_subsidiaries_versions_entity
  ON fund_subsidiaries_versions(entity_id, subsidiary_id, valid_from);
