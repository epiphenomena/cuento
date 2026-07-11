-- +goose Up
-- p02.1: minimal users table + the changes audit spine.
-- Forward-only; never edit an applied migration; no Down (AGENTS rule 4).
--
-- This is the MINIMAL users table. password_hash, is_admin, txn_perm, and the
-- per-user settings columns are added in p06.1; users_versions is created there
-- too (its snapshot must exclude password_hash, which does not exist yet).
-- changes is the audit spine and is itself never versioned.

CREATE TABLE users (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,   -- D17
  username     TEXT NOT NULL UNIQUE,
  display_name TEXT NOT NULL,
  created_at   TEXT NOT NULL,                        -- RFC3339
  disabled_at  TEXT                                  -- NULL = active
);

CREATE TABLE changes (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,        -- D17
  actor_id INTEGER NOT NULL REFERENCES users(id),
  at       TEXT NOT NULL,                            -- RFC3339
  kind     TEXT NOT NULL,
  note     TEXT                                      -- nullable (Appendix A)
);

-- System actor (id 1): the actor for machine-originated changes (seeds,
-- migrations, imports). Fixed literal timestamp keeps the migration
-- deterministic — no dynamic clock in a migration (AGENTS rule 4).
INSERT INTO users (id, username, display_name, created_at)
VALUES (1, 'system', 'System', '1970-01-01T00:00:00Z');
