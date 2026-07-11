-- +goose Up
-- p03.1: currencies reference table + seed (Appendix A, D1).
-- Forward-only; never edit an applied migration; no Down (AGENTS rule 4).
--
-- currencies is STATIC reference data (like form990_lines), NOT a versioned
-- business table: it is absent from the Appendix A versions list, so there is
-- no currencies_versions table, no trigger, and no changes-row wiring. Amounts
-- are stored as int64 minor units; exponent (0..4) gives the minor-unit scale
-- per currency (D1) — JPY/BHD-style exponents cost nothing to support.

CREATE TABLE currencies (
  code     TEXT PRIMARY KEY,
  exponent INTEGER NOT NULL CHECK (exponent BETWEEN 0 AND 4),
  symbol   TEXT NOT NULL,
  name     TEXT NOT NULL,
  active   INTEGER NOT NULL DEFAULT 1
);

-- Seed exactly the three currencies p03.1 checks (the fixture uses USD+MXN).
-- Deterministic literals only — no dynamic clock in a migration (rule 4).
INSERT INTO currencies (code, exponent, symbol, name) VALUES
  ('USD', 2, '$', 'US Dollar'),
  ('MXN', 2, '$', 'Mexican Peso'),
  ('EUR', 2, '€', 'Euro');
