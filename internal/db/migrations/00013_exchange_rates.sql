-- +goose Up
-- p14.1: exchange_rates -- daily-granularity FX rates for report-time conversion
-- (D12). Forward-only; never edit an applied migration; no Down (AGENTS rule 4).
--
-- exchange_rates is CHANGE-ID-ANCHORED reference data, NOT a versioned business
-- table. It is absent from the Appendix A versions list, so there is no
-- exchange_rates_versions twin, no trigger, and no snapshot-from-live append
-- (like currencies/report_groups/org_settings). But UNLIKE those, a rate write
-- IS an audited batch: PutRates goes through the store write funnel so every
-- batch shares ONE changes row (the audit anchor, rule 14) recorded here in
-- change_id -- answering "who loaded these rates, and when". A rates-only change
-- row is referenced by no *_versions row, which is fine: Z5 checks
-- versions -> changes (never the reverse), and no check requires a changes row to
-- have a version twin (reference-data writes bypass the funnel entirely).
--
-- rate is a REAL (float) per the PLAN spec and D12 -- exchange rates and
-- report-time conversion are the ONLY place float64 is permitted (AGENTS rule 3);
-- stored amounts stay int64 minor units. Conversion/rounding lands in p15
-- reporting, not here.
--
-- PK(rate_date, base, quote): at most one rate per (date, pair). rate_date is a
-- YYYY-MM-DD string (same date convention as transactions.date); base/quote are
-- 3-letter currency codes (FKs into currencies so a rate cannot name a currency
-- the org does not know). RateOn looks up the greatest rate_date <= a query date.

CREATE TABLE exchange_rates (
  rate_date TEXT    NOT NULL,
  base      TEXT    NOT NULL REFERENCES currencies(code),
  quote     TEXT    NOT NULL REFERENCES currencies(code),
  rate      REAL    NOT NULL,
  source    TEXT    NOT NULL,
  change_id INTEGER NOT NULL REFERENCES changes(id),
  PRIMARY KEY (rate_date, base, quote)
);

-- The RateOn lookup is (base, quote) filtered, greatest rate_date <= target: this
-- index serves that ORDER BY rate_date DESC LIMIT 1 (and its reciprocal swap)
-- without a table scan as the rate history grows.
CREATE INDEX idx_exchange_rates_pair_date ON exchange_rates (base, quote, rate_date);
