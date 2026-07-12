-- +goose Up
-- p09.4: seed the Honduran Lempira (HNL) currency for the go-live import.
-- Forward-only; never edit an applied migration; no Down (AGENTS rule 4).
--
-- The historical ledger (docs/ledger-export.md) posts in two currencies: the
-- org functional currency (USD, seeded p03.1) and one local currency, the
-- Honduran Lempira. The HNL subsidiary's base_currency FK and every HNL split
-- reference currencies(code), so HNL must exist before the import can create
-- that subsidiary or post its transactions. exponent 2 per ISO 4217 (D1).
--
-- currencies is STATIC reference data (like p03.1's seed), NOT a versioned
-- business table, so this is a plain INSERT with no versions/changes wiring.
INSERT INTO currencies (code, exponent, symbol, name) VALUES
  ('HNL', 2, 'L', 'Honduran Lempira');
