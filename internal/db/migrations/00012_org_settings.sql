-- +goose Up
-- p11.4: org_settings -- a simple key/value config table for org-wide settings.
-- Forward-only; never edit an applied migration; no Down (AGENTS rule 4).
--
-- org_settings is STATIC CONFIG reference data (like currencies and report_groups,
-- p03.1/p06.3): it is NOT a versioned business table -- it is ABSENT from the
-- Appendix A versions list, so there is NO org_settings_versions twin, no trigger,
-- and no changes-row wiring. Admin writes here are configuration, not audited
-- business mutations, so the store's read/write helpers are plain sqlc upserts
-- OUTSIDE the write funnel (rule 2 permits reads and reference/config upserts via
-- sqlc without an actor or a changes row).
--
-- Seeded keys:
--   org_name           the organization's display name (default '': unset until an
--                      admin fills it in; no consumer yet, p13.x wires it into
--                      chrome -- storing it here is the p11.4 scope).
--   enabled_languages  a CSV of the languages account NAMES may be entered in
--                      (default 'en,es', D14). The account create/edit form renders
--                      one name input per enabled language; adding a language here
--                      makes a new name column appear. en is the required base and
--                      the UI CHROME stays en/es regardless (D14 fallback) -- this
--                      setting drives account name columns ONLY, not i18n.Langs().
--
-- Report base currency is INTENTIONALLY NOT an org setting: it follows the scoped
-- subsidiary's base_currency (D18), so it lives on subsidiaries, not here.
CREATE TABLE org_settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

INSERT INTO org_settings (key, value) VALUES
  ('org_name', ''),
  ('enabled_languages', 'en,es');
