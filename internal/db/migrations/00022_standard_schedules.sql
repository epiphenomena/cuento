-- +goose Up
-- p26.28: pre-create the four STANDARD budget schedules (p19.x) so a fresh install
-- and the already-built dev/sample db both have them out of the box. Forward-only;
-- never edit an applied migration; no Down (AGENTS rule 4).
--
-- budget_schedules is a VERSIONED business table (00016): every seeded row MUST also
-- get its op='create' *_versions snapshot tied to one `changes` row, exactly like a
-- normal store CreateSchedule write, or Z3/Z-version parity flags it. This mirrors
-- the audit-consistent seed template (00004/00008): a changes row + live rows + the
-- matching version rows, all deterministic (fixed epoch, rule 4).
--
-- CRITICAL difference from 00004/00008: those ran on a PROVABLY EMPTY db and could
-- hardcode changes.id / entity ids (1..3). This migration must ALSO apply cleanly to
-- an already-populated db (dev.db has changes up to ~49k from ledgerimport) on the
-- next migrate-on-startup, so hardcoding ids would collide on the changes PK. We let
-- AUTOINCREMENT assign ids, tie the version snapshot to the change via a unique note,
-- and INSERT ... SELECT the snapshot from the just-written LIVE row (like 00006's
-- backfill) so the snapshot is byte-identical to live BY CONSTRUCTION (Z3 can never
-- diverge, whatever id AUTOINCREMENT picks). last_insert_rowid() is connection-scoped
-- and stable across goose's in-transaction statements.
--
-- The four schedule NAMES are stored as PLAIN English names, like every other
-- user-created schedule name (budget_schedules.name is stored data, not an i18n
-- catalog key -- same treatment as subsidiary/fund/payee proper nouns, AGENTS rule
-- 9). budget_schedules.name has NO UNIQUE constraint, so these never collide with a
-- user's own schedules; a bilingual org can rename any of them freely.
--
-- Each param set is one the schedule engine (internal/budget.ExpandSchedule) accepts
-- (validated in the store's CreateSchedule path via validateScheduleFields):
--   * Monthly (1st)          monthly,     day_of_month=1                (DoM XOR ordinal)
--   * Monthly (last day)     monthly,     day_of_month=-1 (month-end)
--   * Weekly (Friday)        weekly,      weekday=5 (0=Sun..6=Sat, matches Go's
--                              time.Weekday), anchor_date=2000-01-07 (a Friday --
--                              the engine aligns the recurrence to the anchor's week;
--                              the value only needs to parse to a valid date)
--   * Semimonthly (15th & last) semimonthly, day_of_month=15, day_of_month_2=-1
--                              (-1=month-end so February is correct, not literal 30)
-- weekend_adjust is left at the column DEFAULT (prev_business_day): it is ignored by
-- the weekly kind and is a sane payroll default for the DoM/semimonthly kinds. The
-- INSERT ... SELECT snapshot copies whatever the live default resolves to, so live
-- and version stay identical regardless.
--
-- Keep this file PURE ASCII (sqlc reads migrations as its schema; the 00008/00009
-- ASCII convention and the p04.2 byte-offset quirk apply here too).

-- One changes row ties all four seeded schedules (mirroring CreateSchedule, which
-- ties a schedule + its dates under a single change). AUTOINCREMENT assigns the id
-- (4 on a fresh db, a high free id on a populated one).
INSERT INTO changes (actor_id, at, kind, note)
VALUES (1, '1970-01-01T00:00:00Z', 'seed', 'seed standard budget schedules');

-- 1. Monthly (1st) --------------------------------------------------------------
INSERT INTO budget_schedules (name, kind, day_of_month)
VALUES ('Monthly (1st)', 'monthly', 1);
INSERT INTO budget_schedules_versions
  (entity_id, change_id, valid_from, op,
   name, kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date, weekend_adjust, notes)
SELECT s.id,
       (SELECT id FROM changes WHERE note = 'seed standard budget schedules'),
       '1970-01-01T00:00:00Z', 'create',
       s.name, s.kind, s.day_of_month, s.day_of_month_2, s.ordinal, s.weekday,
       s.anchor_date, s.weekend_adjust, s.notes
FROM budget_schedules s WHERE s.id = last_insert_rowid();

-- 2. Monthly (last day) ---------------------------------------------------------
INSERT INTO budget_schedules (name, kind, day_of_month)
VALUES ('Monthly (last day)', 'monthly', -1);
INSERT INTO budget_schedules_versions
  (entity_id, change_id, valid_from, op,
   name, kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date, weekend_adjust, notes)
SELECT s.id,
       (SELECT id FROM changes WHERE note = 'seed standard budget schedules'),
       '1970-01-01T00:00:00Z', 'create',
       s.name, s.kind, s.day_of_month, s.day_of_month_2, s.ordinal, s.weekday,
       s.anchor_date, s.weekend_adjust, s.notes
FROM budget_schedules s WHERE s.id = last_insert_rowid();

-- 3. Weekly (Friday) ------------------------------------------------------------
INSERT INTO budget_schedules (name, kind, weekday, anchor_date)
VALUES ('Weekly (Friday)', 'weekly', 5, '2000-01-07');
INSERT INTO budget_schedules_versions
  (entity_id, change_id, valid_from, op,
   name, kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date, weekend_adjust, notes)
SELECT s.id,
       (SELECT id FROM changes WHERE note = 'seed standard budget schedules'),
       '1970-01-01T00:00:00Z', 'create',
       s.name, s.kind, s.day_of_month, s.day_of_month_2, s.ordinal, s.weekday,
       s.anchor_date, s.weekend_adjust, s.notes
FROM budget_schedules s WHERE s.id = last_insert_rowid();

-- 4. Semimonthly (15th & last) --------------------------------------------------
INSERT INTO budget_schedules (name, kind, day_of_month, day_of_month_2)
VALUES ('Semimonthly (15th & last)', 'semimonthly', 15, -1);
INSERT INTO budget_schedules_versions
  (entity_id, change_id, valid_from, op,
   name, kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date, weekend_adjust, notes)
SELECT s.id,
       (SELECT id FROM changes WHERE note = 'seed standard budget schedules'),
       '1970-01-01T00:00:00Z', 'create',
       s.name, s.kind, s.day_of_month, s.day_of_month_2, s.ordinal, s.weekday,
       s.anchor_date, s.weekend_adjust, s.notes
FROM budget_schedules s WHERE s.id = last_insert_rowid();
