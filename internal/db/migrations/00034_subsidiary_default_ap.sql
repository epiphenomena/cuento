-- +goose Up
-- Subsidiary default AP (accounts payable) account. Forward-only; never edit an
-- applied migration; no Down (AGENTS rule 4).
--
-- A per-subsidiary preference: the liability (payable) account an expense report's
-- main split posts to by default (used by a later task). It is a NULLABLE FK to
-- accounts -- nil = unset, so a subsidiary may have none.
--
-- This ALTERs subsidiaries + subsidiaries_versions to add default_ap_account_id,
-- following default_program_id (00019) EXACTLY: a nullable INTEGER on the live
-- table WITH the FK REFERENCES, and a plain INTEGER (no FK) on the versions twin
-- (an audit snapshot is not a live reference). Every existing live row and every
-- existing version row takes NULL by construction, so NULL == NULL keeps the
-- version-vs-live integrity check (Z3) clean with no backfill row.
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the byte-offset
-- quirk in docs/DECISIONS.md p04.2 applies here too).
ALTER TABLE subsidiaries ADD COLUMN default_ap_account_id INTEGER REFERENCES accounts(id);  -- nullable, no default
ALTER TABLE subsidiaries_versions ADD COLUMN default_ap_account_id INTEGER;                  -- audited (rule 5); plain INTEGER, no FK, no default -> NULL keeps existing snapshots Z3-clean
