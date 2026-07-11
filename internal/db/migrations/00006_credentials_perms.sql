-- +goose Up
-- p06.1: credentials, permissions, per-user settings (D9, D10, rule 13).
-- Forward-only; never edit an applied migration; no Down (AGENTS rule 4).
--
-- This migration:
--   1. ALTERs users to add the password hash, permission columns, and the
--      per-user settings columns (one ADD COLUMN per statement -- SQLite);
--   2. CREATEs users_versions -- the append-only audit twin whose snapshot
--      DELIBERATELY EXCLUDES password_hash (rule 5): the audit trail must never
--      contain the secret;
--   3. CREATEs report_groups (code-synced reference data, D10; NOT versioned;
--      populated by the startup sync in p06.3 -- no seed here);
--   4. CREATEs user_report_grants + its version twin;
--   5. BACKFILLs the p04.1-deferred users_versions op='create' row for the
--      system user (id 1), so future Z3/Z5 hold uniformly -- WITHOUT any
--      password_hash.

-- ---------------------------------------------------------------------------
-- 1. ALTER users. One ADD COLUMN per statement (SQLite requirement). NOT NULL
--    columns all carry a non-null default so the ALTER is legal on the existing
--    (system-user) row; default_subsidiary_id is the only nullable add and its
--    FK REFERENCES is legal precisely because its default is NULL.
-- ---------------------------------------------------------------------------
ALTER TABLE users ADD COLUMN password_hash TEXT;                                        -- nullable: the system user has none
ALTER TABLE users ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0;                        -- D10: implies all permissions
ALTER TABLE users ADD COLUMN txn_perm TEXT NOT NULL DEFAULT 'none'
  CHECK (txn_perm IN ('none','read','write'));                                          -- D10
ALTER TABLE users ADD COLUMN locale TEXT NOT NULL DEFAULT 'en';                          -- settings (D14/D16)
ALTER TABLE users ADD COLUMN date_format TEXT NOT NULL DEFAULT 'ISO';
ALTER TABLE users ADD COLUMN number_format TEXT NOT NULL DEFAULT 'US';
ALTER TABLE users ADD COLUMN display_mode TEXT NOT NULL DEFAULT 'signed';                -- signed vs DR/CR (D2)
ALTER TABLE users ADD COLUMN neg_style TEXT NOT NULL DEFAULT 'minus';                    -- minus vs parentheses
ALTER TABLE users ADD COLUMN theme TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE users ADD COLUMN default_subsidiary_id INTEGER REFERENCES subsidiaries(id);  -- nullable

-- ---------------------------------------------------------------------------
-- 2. users_versions -- append-only audit twin (rule 5, D4, Appendix A pattern).
--    Never sees UPDATE/DELETE.
--
--    CRITICAL (rule 5): the snapshot columns are ALL of users' business columns
--    EXCEPT password_hash. The secret is stored ONLY in the live users table and
--    NEVER enters the audit trail. The snapshot column set below, which p06.1's
--    InsertUserVersion query MUST write exactly, is:
--        username, display_name, created_at, disabled_at, is_admin, txn_perm,
--        locale, date_format, number_format, display_mode, neg_style, theme,
--        default_subsidiary_id
--    password_hash is absent by design -- do not add it (rule 5).
-- ---------------------------------------------------------------------------
CREATE TABLE users_versions (
  id                    INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id             INTEGER NOT NULL,                          -- users.id this snapshot is of
  change_id             INTEGER NOT NULL REFERENCES changes(id),
  valid_from            TEXT NOT NULL,                             -- equals changes.at (rule 5)
  op                    TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of users' business columns, DELIBERATELY EXCLUDING password_hash (rule 5):
  username              TEXT NOT NULL,
  display_name          TEXT NOT NULL,
  created_at            TEXT NOT NULL,
  disabled_at           TEXT,
  is_admin              INTEGER NOT NULL,
  txn_perm              TEXT NOT NULL,
  locale                TEXT NOT NULL,
  date_format           TEXT NOT NULL,
  number_format         TEXT NOT NULL,
  display_mode          TEXT NOT NULL,
  neg_style             TEXT NOT NULL,
  theme                 TEXT NOT NULL,
  default_subsidiary_id INTEGER
);
CREATE INDEX users_versions_entity ON users_versions(entity_id, valid_from);

-- ---------------------------------------------------------------------------
-- 3. report_groups -- code-declared reference data (D10), synced to the db at
--    startup (p06.3). NOT a versioned business table (absent from Appendix A's
--    versions list): no *_versions twin, no changes wiring. No seed here -- the
--    startup sync fills it.
-- ---------------------------------------------------------------------------
CREATE TABLE report_groups (
  name TEXT PRIMARY KEY,
  sort INTEGER NOT NULL DEFAULT 0
);

-- ---------------------------------------------------------------------------
-- 4. user_report_grants -- per-user read grants on report groups (D10). Its
--    version twin keys on a COMPOSITE entity (entity_id = user_id, snapshot
--    group_name), exactly like account_subsidiaries: membership is a set, so an
--    add is op='create' and a removal op='delete'. Grant management (writers,
--    the version-append query) lands in p13.2 -- this step only defines the
--    schema; the FK behaviour is exercised by direct SQL.
-- ---------------------------------------------------------------------------
CREATE TABLE user_report_grants (
  user_id    INTEGER NOT NULL REFERENCES users(id),
  group_name TEXT NOT NULL REFERENCES report_groups(name),
  PRIMARY KEY (user_id, group_name)
);

CREATE TABLE user_report_grants_versions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id  INTEGER NOT NULL,                                    -- user_report_grants.user_id
  change_id  INTEGER NOT NULL REFERENCES changes(id),
  valid_from TEXT NOT NULL,
  op         TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- snapshot (group_name is also part of the composite identity):
  group_name TEXT NOT NULL
);
CREATE INDEX user_report_grants_versions_entity
  ON user_report_grants_versions(entity_id, group_name, valid_from);

-- ---------------------------------------------------------------------------
-- 5. Backfill the system user's users_versions row (the p04.1-DEFERRED instance:
--    user id 1 was seeded in 00002 before users_versions existed). Without this,
--    Z3 (current row == latest version snapshot) would flag `users`.
--
--    The backfill change is id=2: the only prior changes row is id=1 (00004's
--    root-subsidiary seed); 00003/00005 add none. Fixed epoch timestamp keeps
--    the migration deterministic (rule 4).
--
--    The version row is built with INSERT...SELECT from the just-ALTERed LIVE
--    users row, so the snapshot equals the post-ALTER default values by
--    construction (it cannot drift if a default changes) -- and it selects the
--    non-secret columns ONLY (no password_hash, rule 5). valid_from == changes.at.
-- ---------------------------------------------------------------------------
INSERT INTO changes (id, actor_id, at, kind, note)
VALUES (2, 1, '1970-01-01T00:00:00Z', 'seed', 'backfill system user version');

INSERT INTO users_versions
  (entity_id, change_id, valid_from, op,
   username, display_name, created_at, disabled_at, is_admin, txn_perm,
   locale, date_format, number_format, display_mode, neg_style, theme,
   default_subsidiary_id)
SELECT u.id, 2, '1970-01-01T00:00:00Z', 'create',
       u.username, u.display_name, u.created_at, u.disabled_at, u.is_admin, u.txn_perm,
       u.locale, u.date_format, u.number_format, u.display_mode, u.neg_style, u.theme,
       u.default_subsidiary_id
FROM users u
WHERE u.id = 1;
