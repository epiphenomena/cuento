-- +goose Up
-- p27.2: budget-splits -- the NEW split-derived budget model (DECISIONS "Budget
-- redesign", 2026-07-17). Forward-only; never edit an applied migration; no Down
-- (AGENTS rule 4). ADDITIVE: the old budgets/budget_lines/budget_schedules model
-- (00016/00022/00025/00026) is UNTOUCHED and stays the shipped build until p27.3
-- retires it. The two models coexist here.
--
-- A budget = a set of PROJECTED, dated splits stored in a budget-scoped table --
-- NOT the real splits/transactions ledger (actuals stay clean). Each split is a
-- SINGLE-legged projected row: its balancing "cash available" counter-leg is
-- IMPLICIT and UNNAMED (the org plans in total cash, not per-cash-account). So a
-- budget-split is NOT a zero-sum transaction and is NOT subject to the ledger's
-- per-transaction / per-fund zero-sum invariants -- those are LEDGER invariants;
-- budget-splits are a separate projection plane (DECISIONS tension 1/2 resolutions).
--
-- Two versioned business tables get standard `_versions` twins (rule 5, D4,
-- snapshot-from-live), each a SINGLE-ID entity (no composite/set twin -- mirrors
-- budgets / budget_lines from 00016, NOT the composite fund_subsidiaries pattern):
--   * budget_plans   -> budget_plans_versions   (mutable header, hard-deletable)
--   * budget_splits  -> budget_splits_versions   (hard-deletable line)
--
-- The container is named `budget_plans` (NOT `budgets`) to avoid colliding with the
-- to-be-retired `budgets` table; p27.3 settles the final names when the old model
-- is dropped.
--
-- subsidiary_id lives on the PLAN, not the split (DECISIONS/p27.2 note): a plan is
-- scoped to one subsidiary, and every split's account is validated as a leaf in the
-- plan's subsidiary. This keeps the split column list exactly as the design lists it
-- (desc, date, account, fund, program, amount, currency).
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the 00016 ASCII
-- convention and the p04.2 byte-offset quirk apply here too).

-- ---------------------------------------------------------------------------
-- budget_plans: a named budget plan scoped to one subsidiary. No period bounds
-- (every split carries its own date; the report buckets by date -- there is no
-- separate horizon object). No seed rows (all user-created), so no seed version.
-- ---------------------------------------------------------------------------
CREATE TABLE budget_plans (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,                   -- D17
  name          TEXT NOT NULL,
  subsidiary_id INTEGER NOT NULL REFERENCES subsidiaries(id),
  notes         TEXT NOT NULL DEFAULT ''
);

-- budget_splits: one PROJECTED, dated split. account_id is a leaf in the plan's
-- subsidiary and is revenue/expense OR an open_item receivable/payable (store-
-- enforced). fund_id NULLABLE (NULL = unrestricted); program_id NULLABLE
-- (REQUIRED on R/E-categorized legs, FORBIDDEN on A/L -- store-enforced, resolving
-- DECISIONS tension 3). amount is INTEGER minor units (rule 3), a plain SIGNED
-- projection (no DR/CR twin -- single-legged). No functional_class (budget-splits
-- carry none, per the design).
CREATE TABLE budget_splits (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,                     -- D17
  plan_id     INTEGER NOT NULL REFERENCES budget_plans(id),
  description TEXT NOT NULL DEFAULT '',
  date        TEXT NOT NULL CHECK (date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  account_id  INTEGER NOT NULL REFERENCES accounts(id),
  fund_id     INTEGER REFERENCES funds(id),                          -- NULL = unrestricted
  program_id  INTEGER REFERENCES programs(id),                       -- NULL only on A/L legs
  amount      INTEGER NOT NULL,                                      -- minor units, signed (rule 3)
  currency    TEXT NOT NULL REFERENCES currencies(code)
);
CREATE INDEX budget_splits_plan ON budget_splits(plan_id);

-- ---------------------------------------------------------------------------
-- Version twins (rule 5, D4). Append-only; never UPDATE/DELETE. Snapshot columns
-- carry NO CHECK/FK (history, not live); the only referential FK is change_id ->
-- changes. Each InsertXVersion (queries) does INSERT ... SELECT from the just-
-- written live row on the same tx, so a version row is byte-identical to its live
-- row (Z3 can never diverge) and valid_from == changes.at BY CONSTRUCTION. These
-- mirror 00016's budgets / budget_lines twins EXACTLY.
-- ---------------------------------------------------------------------------

-- budget_plans_versions: STANDARD single-column entity (entity_id = budget_plans.id).
-- Snapshot column set which InsertBudgetPlanVersion writes EXACTLY:
--   name, subsidiary_id, notes
CREATE TABLE budget_plans_versions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id     INTEGER NOT NULL,                                    -- budget_plans.id
  change_id     INTEGER NOT NULL REFERENCES changes(id),
  valid_from    TEXT NOT NULL,                                       -- equals changes.at (rule 5)
  op            TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of budget_plans' business columns (no CHECK/FK -- history):
  name          TEXT NOT NULL,
  subsidiary_id INTEGER NOT NULL,
  notes         TEXT NOT NULL
);
CREATE INDEX budget_plans_versions_entity ON budget_plans_versions(entity_id, valid_from);

-- budget_splits_versions: STANDARD single-column entity (entity_id =
-- budget_splits.id). A split CAN be op='delete' (hard delete + delete version,
-- rule 14: soft-delete is transactions-only). Snapshot column set which
-- InsertBudgetSplitVersion writes EXACTLY:
--   plan_id, description, date, account_id, fund_id, program_id, amount, currency
CREATE TABLE budget_splits_versions (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id   INTEGER NOT NULL,                                      -- budget_splits.id
  change_id   INTEGER NOT NULL REFERENCES changes(id),
  valid_from  TEXT NOT NULL,                                         -- equals changes.at (rule 5)
  op          TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of budget_splits' business columns:
  plan_id     INTEGER NOT NULL,
  description TEXT NOT NULL,
  date        TEXT NOT NULL,
  account_id  INTEGER NOT NULL,
  fund_id     INTEGER,
  program_id  INTEGER,
  amount      INTEGER NOT NULL,
  currency    TEXT NOT NULL
);
CREATE INDEX budget_splits_versions_entity ON budget_splits_versions(entity_id, valid_from);
