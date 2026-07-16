-- +goose Up
-- p26.63: soft-delete for saved bank-import mapping profiles. Forward-only; never
-- edit an applied migration; no Down (AGENTS rule 4). Keep this file PURE ASCII (sqlc
-- reads migrations as its schema; the p04.2 byte-offset quirk applies).
--
-- A profile cannot be HARD-deleted: import_batches.profile_id is
-- NOT NULL REFERENCES mapping_profiles(id) and every batch (including the throwaway
-- "import DATE" profile importConfirm creates per confirm) references a profile at
-- birth, so with foreign_keys=ON (db.go DSN) a DELETE of any real profile trips the
-- FK. So deletion is a SOFT deactivate: `active` flips to 0 and the profile drops out
-- of the load list, while the batch's profile_id FK (its audit of the exact mapping
-- that produced it) stays intact.
--
-- mapping_profiles is a NON-VERSIONED operational/staging table (DECISIONS p17.1) --
-- no *_versions twin -- so this is a plain column add, no version trigger. ADD COLUMN
-- with a constant DEFAULT is an in-place SQLite operation (no table rebuild): existing
-- rows read the default, so every already-saved profile stays active.
ALTER TABLE mapping_profiles
  ADD COLUMN active INTEGER NOT NULL DEFAULT 1;
