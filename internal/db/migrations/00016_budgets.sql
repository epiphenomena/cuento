-- +goose Up
-- p19.1: named schedules + budget model (Phase 19 budgeting). Forward-only; never
-- edit an applied migration; no Down (AGENTS rule 4).
--
-- A budget projects revenue/expense (R/E) flows onto DISCRETE occurrence DATES: a
-- named, reusable SCHEDULE generates the dates, and each budget LINE carries an
-- amount-per-occurrence keyed by (subsidiary, R/E account, fund, program). The
-- amount lands in FULL on each date -- NO pro-rata (PLAN Phase 19); reports (p19.2+)
-- bucket occurrences by their period and sum. This step is SCHEMA + versioned store
-- CRUD + the pure expansion engine (internal/budget) only -- no toolkit, no web.
--
-- Four versioned business tables get standard `_versions` twins (rule 5, D4,
-- snapshot-from-live), mirroring the funds pattern (00009) EXACTLY so the Z3/Z5
-- version-consistency checks stay clean:
--   * budget_schedules        -> budget_schedules_versions        (single-id twin)
--   * budget_schedule_dates   -> budget_schedule_dates_versions   (COMPOSITE twin,
--       entity_id = schedule_id, snapshot col = occurs_on -- the custom/imported
--       date list, versioned as a set like fund_subsidiaries: add='create',
--       remove='delete')
--   * budgets                 -> budgets_versions                 (single-id twin)
--   * budget_lines            -> budget_lines_versions            (single-id twin;
--       DeleteBudgetLine hard-deletes with an op='delete' version, rule-14-clean:
--       soft-delete is reserved for transactions, everything else audits its delete)
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the 00008/00009
-- ASCII convention and the p04.2 byte-offset quirk apply here too).

-- ---------------------------------------------------------------------------
-- budget_schedules: a NAMED, reusable recurrence. `kind` is a fixed enum; the
-- kind-specific fields (day_of_month, day_of_month_2, ordinal, weekday,
-- anchor_date) are nullable and validated PER KIND in the store (p19.1), NOT via
-- cross-field SQL CHECKs (the pure expansion engine is the single source of the
-- date math). weekend_adjust applies only to day-of-month kinds (monthly-by-DoM,
-- semimonthly) and defaults to prev_business_day. anchor_date matches the
-- YYYY-MM-DD shape used across the schema (loose GLOB, NULL allowed).
--
-- day_of_month / day_of_month_2 accept 1..31 or -1 (month-end sentinel) -- the
-- store/engine clamp short months; ordinal is 1..4 or -1 (last); weekday 0..6.
-- ---------------------------------------------------------------------------
CREATE TABLE budget_schedules (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,                 -- D17
  name           TEXT NOT NULL,
  kind           TEXT NOT NULL CHECK (kind IN ('onetime','annual','monthly','semimonthly','biweekly','weekly','custom')),
  day_of_month   INTEGER,                                           -- 1..31 or -1 (month-end); NULL if unused
  day_of_month_2 INTEGER,                                           -- semimonthly second day; NULL if unused
  ordinal        INTEGER,                                           -- 1..4 or -1 (last); NULL if unused
  weekday        INTEGER,                                           -- 0..6 (Sun..Sat); NULL if unused
  anchor_date    TEXT CHECK (anchor_date IS NULL OR anchor_date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  weekend_adjust TEXT NOT NULL DEFAULT 'prev_business_day'
                   CHECK (weekend_adjust IN ('actual','prev_business_day','next_business_day')),
  notes          TEXT NOT NULL DEFAULT ''
);

-- budget_schedule_dates: the custom/imported explicit date list (child of a
-- schedule; only meaningful for kind='custom'). Composite PK (schedule_id,
-- occurs_on) forbids duplicates. Versioned as a SET (see the twin below), like
-- fund_subsidiaries.
CREATE TABLE budget_schedule_dates (
  schedule_id INTEGER NOT NULL REFERENCES budget_schedules(id),
  occurs_on   TEXT NOT NULL CHECK (occurs_on GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  PRIMARY KEY (schedule_id, occurs_on)
);

-- budgets: a named budgeting period. period_start/period_end bound the horizon
-- (v1 caps it at ~1 fiscal year, store-checked in p19.2+; the schema only shape-
-- validates the dates). No seed rows (all user-created), so NO seed change/version.
CREATE TABLE budgets (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,                   -- D17
  name         TEXT NOT NULL,
  period_start TEXT NOT NULL CHECK (period_start GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  period_end   TEXT NOT NULL CHECK (period_end GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  notes        TEXT NOT NULL DEFAULT ''
);

-- budget_lines: one budgeted flow. Keyed by (subsidiary, account [R/E], fund,
-- program) + amount-per-occurrence + a schedule. account_id must be a revenue or
-- expense account (store-enforced, p19.1 -- a budget is of R/E flows, not balance
-- -sheet positions). fund_id is NULLABLE (NULL = unrestricted); program_id is
-- NOT NULL (the ledger requires a program on every R/E split, Z15, and p19.2
-- matches actuals per (sub,account,fund,program) -- so a budget line always names
-- a program). amount is INTEGER minor units (rule 3), magnitude per occurrence
-- (store-checked > 0; sign/direction comes from the account type at report time).
CREATE TABLE budget_lines (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,                  -- D17
  budget_id     INTEGER NOT NULL REFERENCES budgets(id),
  subsidiary_id INTEGER NOT NULL REFERENCES subsidiaries(id),
  account_id    INTEGER NOT NULL REFERENCES accounts(id),          -- must be R/E (store-enforced)
  fund_id       INTEGER REFERENCES funds(id),                      -- NULL = unrestricted
  program_id    INTEGER NOT NULL REFERENCES programs(id),
  amount        INTEGER NOT NULL,                                  -- minor units, per occurrence (rule 3)
  currency      TEXT NOT NULL REFERENCES currencies(code),
  schedule_id   INTEGER NOT NULL REFERENCES budget_schedules(id)
);
CREATE INDEX budget_lines_budget ON budget_lines(budget_id);

-- ---------------------------------------------------------------------------
-- Version twins (rule 5, D4). Append-only; never UPDATE/DELETE. Snapshot columns
-- carry NO CHECK/FK (history, not live -- an old snapshot of a since-invalidated
-- value must still be storable); the only referential FK is change_id -> changes.
-- No seed rows this step (all user-created), so no seed-version wiring. Each
-- InsertXVersion (queries) does INSERT ... SELECT from the just-written live row on
-- the same tx, so a version row is byte-identical to its live row (Z3 can never
-- diverge) and valid_from == changes.at BY CONSTRUCTION. These mirror 00009 EXACTLY.
-- ---------------------------------------------------------------------------

-- budget_schedules_versions: STANDARD single-column entity (entity_id =
-- budget_schedules.id). Snapshot column set (budget_schedules business columns, id
-- excluded), which InsertBudgetScheduleVersion writes EXACTLY:
--   name, kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date,
--   weekend_adjust, notes
CREATE TABLE budget_schedules_versions (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id      INTEGER NOT NULL,                                  -- budget_schedules.id
  change_id      INTEGER NOT NULL REFERENCES changes(id),
  valid_from     TEXT NOT NULL,                                     -- equals changes.at (rule 5)
  op             TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of budget_schedules' business columns (no CHECK/FK -- history):
  name           TEXT NOT NULL,
  kind           TEXT NOT NULL,
  day_of_month   INTEGER,
  day_of_month_2 INTEGER,
  ordinal        INTEGER,
  weekday        INTEGER,
  anchor_date    TEXT,
  weekend_adjust TEXT NOT NULL,
  notes          TEXT NOT NULL
);
CREATE INDEX budget_schedules_versions_entity ON budget_schedules_versions(entity_id, valid_from);

-- budget_schedule_dates_versions: COMPOSITE entity (schedule_id, occurs_on),
-- exactly like fund_subsidiaries_versions (00009). entity_id = schedule_id;
-- occurs_on is BOTH the snapshot column AND part of the composite entity identity.
-- The custom date list is a SET: importing a date appends op 'create', removing it
-- appends op 'delete' (there is NO 'update' for a pure list row). Its as-of /
-- AssertVersioned filter on (entity_id, occurs_on). The index carries occurs_on so
-- per-(schedule, date) as-of lookups stay cheap.
CREATE TABLE budget_schedule_dates_versions (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id   INTEGER NOT NULL,                                     -- budget_schedule_dates.schedule_id
  change_id   INTEGER NOT NULL REFERENCES changes(id),
  valid_from  TEXT NOT NULL,                                        -- equals changes.at (rule 5)
  op          TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- snapshot (occurs_on is also part of the composite identity):
  occurs_on   TEXT NOT NULL
);
CREATE INDEX budget_schedule_dates_versions_entity
  ON budget_schedule_dates_versions(entity_id, occurs_on, valid_from);

-- budgets_versions: STANDARD single-column entity (entity_id = budgets.id).
-- Snapshot column set which InsertBudgetVersion writes EXACTLY:
--   name, period_start, period_end, notes
CREATE TABLE budgets_versions (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id    INTEGER NOT NULL,                                    -- budgets.id
  change_id    INTEGER NOT NULL REFERENCES changes(id),
  valid_from   TEXT NOT NULL,                                       -- equals changes.at (rule 5)
  op           TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of budgets' business columns:
  name         TEXT NOT NULL,
  period_start TEXT NOT NULL,
  period_end   TEXT NOT NULL,
  notes        TEXT NOT NULL
);
CREATE INDEX budgets_versions_entity ON budgets_versions(entity_id, valid_from);

-- budget_lines_versions: STANDARD single-column entity (entity_id =
-- budget_lines.id). A budget line CAN be op='delete' (hard delete + delete
-- version, rule 14: soft-delete is transactions-only, everything else audits its
-- delete). Snapshot column set which InsertBudgetLineVersion writes EXACTLY:
--   budget_id, subsidiary_id, account_id, fund_id, program_id, amount, currency,
--   schedule_id
CREATE TABLE budget_lines_versions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id     INTEGER NOT NULL,                                   -- budget_lines.id
  change_id     INTEGER NOT NULL REFERENCES changes(id),
  valid_from    TEXT NOT NULL,                                      -- equals changes.at (rule 5)
  op            TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of budget_lines' business columns:
  budget_id     INTEGER NOT NULL,
  subsidiary_id INTEGER NOT NULL,
  account_id    INTEGER NOT NULL,
  fund_id       INTEGER,
  program_id    INTEGER NOT NULL,
  amount        INTEGER NOT NULL,
  currency      TEXT NOT NULL,
  schedule_id   INTEGER NOT NULL
);
CREATE INDEX budget_lines_versions_entity ON budget_lines_versions(entity_id, valid_from);
