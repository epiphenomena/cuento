-- +goose Up
-- p06.2: the scs session store table (D9, rule 13).
-- Forward-only; never edit an applied migration; no Down (AGENTS rule 4).
--
-- `sessions` is infrastructure OWNED by github.com/alexedwards/scs/sqlite3store,
-- NOT a versioned business table: it holds opaque, ephemeral session blobs, so
-- it has no *_versions twin, no changes wiring, and no seed. We create it in a
-- goose migration (rather than let scs auto-create it) so goose stays the single
-- source of schema truth (rule 4) and sqlc sees a consistent schema.
--
-- The schema below is the EXACT shape sqlite3store queries by raw SQL. Verified
-- against the installed library source
-- (github.com/alexedwards/scs/sqlite3store@v0.0.0-20251002162104-209de6e426de):
--   Find:   SELECT data FROM sessions WHERE token = ? AND julianday('now') < expiry
--   Commit: REPLACE INTO sessions (token, data, expiry) VALUES (?, ?, julianday(?))
--   Delete: DELETE FROM sessions WHERE token = ?
-- so `expiry` holds a julianday REAL (not a unix epoch); token is the PK; data is
-- the opaque gob blob. The library's README documents this exact DDL. Default
-- table name is "sessions" (sqlite3store.Config.TableName), which we match so
-- sqlite3store.New(db) works with no configuration.
--
-- Keep this file PURE ASCII: sqlc v1.31.1 miscounts byte offsets on multi-byte
-- UTF-8 and corrupts the whole file's generated SQL (docs/DECISIONS.md p04.2).
CREATE TABLE sessions (
  token  TEXT PRIMARY KEY,
  data   BLOB NOT NULL,
  expiry REAL NOT NULL
);

CREATE INDEX sessions_expiry_idx ON sessions (expiry);
