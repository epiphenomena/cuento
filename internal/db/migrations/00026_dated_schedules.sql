-- +goose Up
-- p26.84: seed two DATED (explicit-date) budget schedules so less-than-monthly
-- budgeting -- quarterly and semiannual -- is available WITHOUT any recurrence-engine
-- change. The owner does NOT want >monthly recurrence KINDS added (no 'quarterly' /
-- 'semiannual' enum + expander, see the 00025 header + DECISIONS p26.79); instead the
-- model already carries a dated kind: kind='custom', a schedule whose occurrences are an
-- explicit list of dates in the budget_schedule_dates child table, expanded verbatim by
-- internal/budget.ExpandSchedule (expandCustom). This seed uses that existing kind.
-- Forward-only; never edit an applied migration; no Down (AGENTS rule 4).
--
-- Seeded (a REPRESENTATIVE 2026 year -- a dated schedule is an explicit list, so it
-- names concrete calendar days, not a rule):
--   * Quarterly (quarter-end)          custom, dates 2026-03-31 / 2026-06-30 /
--                                        2026-09-30 / 2026-12-31
--   * Semiannual (Jun 30 & Dec 31)     custom, dates 2026-06-30 / 2026-12-31
-- All four dates are 2026 weekdays; a custom schedule is never weekend-adjusted (the
-- list is authoritative as stored). These give quarterly + semiannual budgeting via the
-- dated path the model supports today, closing the gap 00025 explicitly deferred.
--
-- NAMES are stored as PLAIN English data (like every user-created schedule name and the
-- 00022/00025 seeded rows -- proper-noun treatment, AGENTS rule 9, NOT catalog keys).
-- The task asked for "bilingual" names, but the directly-comparable seeded rows are
-- single English strings and budget_schedules.name is a single (non-per-language)
-- column -- "consistent with the existing rows" is the constraint the schema supports, so
-- these follow the same English-data convention. NOT a slash/composite name (that would
-- itself be inconsistent with the existing rows). DECISIONS p26.84 records the deviation.
--
-- This mirrors 00025 (and 00022) for the PARENT rows: budget_schedules is a VERSIONED
-- business table (00016), so each seeded row gets its op='create' *_versions snapshot
-- tied to ONE `changes` row (rule 5), snapshot copied via INSERT ... SELECT from the
-- just-written LIVE row so live/version are byte-identical (Z3 clean). The child
-- explicit dates live in budget_schedule_dates, itself a SET-versioned twin
-- (budget_schedule_dates_versions, composite entity = schedule_id + occurs_on): each
-- seeded date gets a live row AND an op='create' version snapshot, or the checks.go
-- membership-parity + dangling-snapshot rules flag it.
--
-- Ids are AUTOINCREMENT-assigned (no hardcoded PKs) so this applies CLEANLY to an
-- already-populated db (dev.db) on the next migrate-on-startup. The version snapshots
-- resolve their change via a UNIQUE note (distinct from 00022's and 00025's). The child
-- date inserts resolve their PARENT id by the schedule's (locally unique) name rather
-- than last_insert_rowid() -- last_insert_rowid() no longer points at the schedule once
-- a child date is inserted, and budget_schedule_dates has no AUTOINCREMENT id.
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the 00008/00009 ASCII
-- convention and the p04.2 byte-offset quirk apply here too).

-- One changes row ties both seeded dated schedules (and their child dates). Its note is
-- UNIQUE so the snapshot INSERT ... SELECT statements below resolve exactly this change.
INSERT INTO changes (actor_id, at, kind, note)
VALUES (1, '1970-01-01T00:00:00Z', 'seed', 'seed dated budget schedules');

-- 1. Quarterly (quarter-end) ----------------------------------------------------
-- The parent schedule row (all day/anchor fields NULL -- a custom schedule carries no
-- recurrence params; its dates are the child list). Version snapshot BEFORE any child
-- date insert, while last_insert_rowid() still points at this schedule.
INSERT INTO budget_schedules (name, kind)
VALUES ('Quarterly (quarter-end)', 'custom');
INSERT INTO budget_schedules_versions
  (entity_id, change_id, valid_from, op,
   name, kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date, weekend_adjust, notes)
SELECT s.id,
       (SELECT id FROM changes WHERE note = 'seed dated budget schedules'),
       '1970-01-01T00:00:00Z', 'create',
       s.name, s.kind, s.day_of_month, s.day_of_month_2, s.ordinal, s.weekday,
       s.anchor_date, s.weekend_adjust, s.notes
FROM budget_schedules s WHERE s.id = last_insert_rowid();

-- The explicit dates (live rows), resolving the parent by its locally-unique name.
INSERT INTO budget_schedule_dates (schedule_id, occurs_on)
SELECT (SELECT id FROM budget_schedules WHERE name = 'Quarterly (quarter-end)'), d.occurs_on
FROM (SELECT '2026-03-31' AS occurs_on UNION ALL SELECT '2026-06-30'
      UNION ALL SELECT '2026-09-30' UNION ALL SELECT '2026-12-31') d;
-- One op='create' version snapshot per child date (membership set), byte-identical to
-- the live rows just written for this schedule.
INSERT INTO budget_schedule_dates_versions
  (entity_id, change_id, valid_from, op, occurs_on)
SELECT c.schedule_id,
       (SELECT id FROM changes WHERE note = 'seed dated budget schedules'),
       '1970-01-01T00:00:00Z', 'create', c.occurs_on
FROM budget_schedule_dates c
WHERE c.schedule_id = (SELECT id FROM budget_schedules WHERE name = 'Quarterly (quarter-end)');

-- 2. Semiannual (Jun 30 & Dec 31) -----------------------------------------------
INSERT INTO budget_schedules (name, kind)
VALUES ('Semiannual (Jun 30 & Dec 31)', 'custom');
INSERT INTO budget_schedules_versions
  (entity_id, change_id, valid_from, op,
   name, kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date, weekend_adjust, notes)
SELECT s.id,
       (SELECT id FROM changes WHERE note = 'seed dated budget schedules'),
       '1970-01-01T00:00:00Z', 'create',
       s.name, s.kind, s.day_of_month, s.day_of_month_2, s.ordinal, s.weekday,
       s.anchor_date, s.weekend_adjust, s.notes
FROM budget_schedules s WHERE s.id = last_insert_rowid();

INSERT INTO budget_schedule_dates (schedule_id, occurs_on)
SELECT (SELECT id FROM budget_schedules WHERE name = 'Semiannual (Jun 30 & Dec 31)'), d.occurs_on
FROM (SELECT '2026-06-30' AS occurs_on UNION ALL SELECT '2026-12-31') d;
INSERT INTO budget_schedule_dates_versions
  (entity_id, change_id, valid_from, op, occurs_on)
SELECT c.schedule_id,
       (SELECT id FROM changes WHERE note = 'seed dated budget schedules'),
       '1970-01-01T00:00:00Z', 'create', c.occurs_on
FROM budget_schedule_dates c
WHERE c.schedule_id = (SELECT id FROM budget_schedules WHERE name = 'Semiannual (Jun 30 & Dec 31)');
