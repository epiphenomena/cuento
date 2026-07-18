-- +goose Up
-- p27.3b: RETIRE the schedule-based budget model (DECISIONS "Budget redesign",
-- Retirement note). Forward-only; never edit an applied migration; no Down (AGENTS
-- rule 4). The split-derived model (budget_plans/budget_splits, 00028) is now the
-- shipped budget build; the old named-schedule / budget-line model
-- (00016/00022/00025/00026) is dropped here along with its seed rows.
--
-- Rule-14 concern (dropping VERSIONED tables + their audit twins): no production
-- budget data exists pre-go-live -- budgets are entirely user-created with NO seed
-- business rows, and D26's go-live is import-only (ledger, not budgets) -- so a
-- forward-only drop forfeits no real audit history. The seed rows and their
-- `changes`/`*_versions` rows written by 00016/00022/00025/00026 go with the tables;
-- those migration FILES stay in history (rule 4). The four dropped tables + their
-- twins are removed from the Z3/Z5 version-consistency checked set in the same step
-- (internal/ledger/checks.go), or `cuento check` would flag the now-absent tables.
--
-- DROP TABLE removes each table's indexes and triggers with it (SQLite). Order:
-- children before parents is unnecessary for DROP (no FK cascade needed), but we
-- drop the twins alongside their live tables for clarity. budget_schedule_dates
-- references budget_schedules; budget_lines references budgets + budget_schedules;
-- dropping in any order is fine since we drop them all.
DROP TABLE IF EXISTS budget_lines_versions;
DROP TABLE IF EXISTS budget_lines;
DROP TABLE IF EXISTS budgets_versions;
DROP TABLE IF EXISTS budgets;
DROP TABLE IF EXISTS budget_schedule_dates_versions;
DROP TABLE IF EXISTS budget_schedule_dates;
DROP TABLE IF EXISTS budget_schedules_versions;
DROP TABLE IF EXISTS budget_schedules;
