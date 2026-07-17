-- +goose Up
-- p26.79: seed the fuller set of COMMON budget schedules, on top of the four
-- STANDARD ones 00022 (p26.28) already seeded. Forward-only; never edit an applied
-- migration; no Down (AGENTS rule 4).
--
-- 00022 seeded: Monthly (1st), Monthly (last day), Weekly (Friday),
-- Semimonthly (15th & last). This migration adds the remaining common real-world
-- recurrences the model CAN express, WITHOUT duplicating any of those names:
--   * Weekly (Monday)          weekly,      weekday=1 (0=Sun..6=Sat), anchor 2000-01-03
--                                (a Monday -- the engine aligns the recurrence to the
--                                anchor's week; the value only needs to parse valid)
--   * Biweekly                 biweekly,    anchor 2000-01-07 (every 14 days from the
--                                anchor, expandStride)
--   * Semimonthly (1st & 15th) semimonthly, day_of_month=1, day_of_month_2=15
--   * Monthly (15th)           monthly,     day_of_month=15 (mid-month; DoM XOR ordinal)
--   * Annual (Jan 1)           annual,      anchor 2000-01-01 (repeats month+day yearly;
--                                the anchor's weekday is irrelevant for an annual date)
--
-- DELIBERATELY OMITTED: quarterly and semi-annual. The recurrence model (the 00016
-- `kind` CHECK enum + the pure engine internal/budget.ExpandSchedule) has NO
-- >monthly interval kind and no "every N months" stride, so a 'quarterly' /
-- 'semiannual' row would fail the 00016 CHECK on insert and, if forced, ExpandSchedule's
-- unknown-kind error. Adding them is an ENGINE change (a new enum + expander, and a
-- table recreate for the CHECK), deferred from this pure seed step (DECISIONS p26.79).
--
-- This mirrors 00022 EXACTLY (read that file's header for the full rationale):
-- budget_schedules is a VERSIONED business table (00016), so each seeded row gets its
-- op='create' *_versions snapshot tied to ONE `changes` row (rule 5), snapshot copied
-- via INSERT ... SELECT from the just-written LIVE row so live/version are byte-
-- identical BY CONSTRUCTION (Z3 can never diverge). Ids are AUTOINCREMENT-assigned (no
-- hardcoded PKs) so this applies CLEANLY to an already-populated db (dev.db) on the
-- next migrate-on-startup; the version snapshot is tied to its change via a UNIQUE
-- note (distinct from 00022's, so the WHERE note = ... resolves exactly one change).
-- last_insert_rowid() is connection-scoped and stable across goose's statements.
--
-- Schedule NAMES are stored as PLAIN English data (like every user-created schedule
-- name and like the 00022 rows), NOT i18n catalog keys (AGENTS rule 9 -- proper-noun
-- treatment); budget_schedules.name has no UNIQUE constraint, so these never collide
-- with a user's own schedules and a bilingual org can rename any freely.
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the 00008/00009
-- ASCII convention and the p04.2 byte-offset quirk apply here too).

-- One changes row ties all five seeded schedules. Its note is UNIQUE (distinct from
-- 00022's 'seed standard budget schedules') so the snapshot INSERT ... SELECT below
-- resolves exactly this change.
INSERT INTO changes (actor_id, at, kind, note)
VALUES (1, '1970-01-01T00:00:00Z', 'seed', 'seed common budget schedules');

-- 1. Weekly (Monday) ------------------------------------------------------------
INSERT INTO budget_schedules (name, kind, weekday, anchor_date)
VALUES ('Weekly (Monday)', 'weekly', 1, '2000-01-03');
INSERT INTO budget_schedules_versions
  (entity_id, change_id, valid_from, op,
   name, kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date, weekend_adjust, notes)
SELECT s.id,
       (SELECT id FROM changes WHERE note = 'seed common budget schedules'),
       '1970-01-01T00:00:00Z', 'create',
       s.name, s.kind, s.day_of_month, s.day_of_month_2, s.ordinal, s.weekday,
       s.anchor_date, s.weekend_adjust, s.notes
FROM budget_schedules s WHERE s.id = last_insert_rowid();

-- 2. Biweekly -------------------------------------------------------------------
INSERT INTO budget_schedules (name, kind, anchor_date)
VALUES ('Biweekly', 'biweekly', '2000-01-07');
INSERT INTO budget_schedules_versions
  (entity_id, change_id, valid_from, op,
   name, kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date, weekend_adjust, notes)
SELECT s.id,
       (SELECT id FROM changes WHERE note = 'seed common budget schedules'),
       '1970-01-01T00:00:00Z', 'create',
       s.name, s.kind, s.day_of_month, s.day_of_month_2, s.ordinal, s.weekday,
       s.anchor_date, s.weekend_adjust, s.notes
FROM budget_schedules s WHERE s.id = last_insert_rowid();

-- 3. Semimonthly (1st & 15th) ---------------------------------------------------
INSERT INTO budget_schedules (name, kind, day_of_month, day_of_month_2)
VALUES ('Semimonthly (1st & 15th)', 'semimonthly', 1, 15);
INSERT INTO budget_schedules_versions
  (entity_id, change_id, valid_from, op,
   name, kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date, weekend_adjust, notes)
SELECT s.id,
       (SELECT id FROM changes WHERE note = 'seed common budget schedules'),
       '1970-01-01T00:00:00Z', 'create',
       s.name, s.kind, s.day_of_month, s.day_of_month_2, s.ordinal, s.weekday,
       s.anchor_date, s.weekend_adjust, s.notes
FROM budget_schedules s WHERE s.id = last_insert_rowid();

-- 4. Monthly (15th) -------------------------------------------------------------
INSERT INTO budget_schedules (name, kind, day_of_month)
VALUES ('Monthly (15th)', 'monthly', 15);
INSERT INTO budget_schedules_versions
  (entity_id, change_id, valid_from, op,
   name, kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date, weekend_adjust, notes)
SELECT s.id,
       (SELECT id FROM changes WHERE note = 'seed common budget schedules'),
       '1970-01-01T00:00:00Z', 'create',
       s.name, s.kind, s.day_of_month, s.day_of_month_2, s.ordinal, s.weekday,
       s.anchor_date, s.weekend_adjust, s.notes
FROM budget_schedules s WHERE s.id = last_insert_rowid();

-- 5. Annual (Jan 1) -------------------------------------------------------------
INSERT INTO budget_schedules (name, kind, anchor_date)
VALUES ('Annual (Jan 1)', 'annual', '2000-01-01');
INSERT INTO budget_schedules_versions
  (entity_id, change_id, valid_from, op,
   name, kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date, weekend_adjust, notes)
SELECT s.id,
       (SELECT id FROM changes WHERE note = 'seed common budget schedules'),
       '1970-01-01T00:00:00Z', 'create',
       s.name, s.kind, s.day_of_month, s.day_of_month_2, s.ordinal, s.weekday,
       s.anchor_date, s.weekend_adjust, s.notes
FROM budget_schedules s WHERE s.id = last_insert_rowid();
