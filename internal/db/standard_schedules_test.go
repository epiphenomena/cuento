package db_test

import (
	"database/sql"
	"testing"

	"cuento/internal/budget"
	"cuento/internal/testutil"
)

// p26.28 seeds four STANDARD budget schedules (00022) so a fresh install has them
// out of the box. budget_schedules is a versioned business table (00016), so each
// seeded row must carry its op='create' *_versions snapshot tied to one `changes`
// row -- exactly like a store CreateSchedule write -- or Z3/version parity flags it.
// These tests exercise the migration on a freshly-migrated harness db: the four
// rows exist with the expected kind+params, each is versioned (a matching
// budget_schedules_versions 'create' row), and each param set is one the schedule
// engine (internal/budget.ExpandSchedule) accepts. Written before 00022 exists --
// each fails until the migration lands.

// wantSchedule is the expected shape of one seeded standard schedule. Nullable ints
// use *int (nil = the column is NULL); anchor uses *string.
type wantSchedule struct {
	name       string
	kind       string
	dayOfMonth *int
	dayOfMon2  *int
	ordinal    *int
	weekday    *int
	anchor     *string
	// customDates is the explicit-date list for a kind='custom' (dated) schedule
	// (p26.84), in the budget_schedule_dates child table. nil for every rule-based
	// kind (which carries no child dates).
	customDates []string
}

func ip(v int) *int       { return &v }
func sp(v string) *string { return &v }

// standardSchedules is the exact seeded set the two migrations must produce: the
// four from 00022 (p26.28) plus the fuller common set from 00025 (p26.79). The two
// blocks are kept separate so a reader sees which migration owns which row and the
// 00025 additions never duplicate an 00022 name. Quarterly / semi-annual are
// deliberately ABSENT: the recurrence model (00016 kind enum + budget.ExpandSchedule)
// has no >monthly interval kind, so those are an engine change deferred from this
// seed step (DECISIONS p26.79).
func standardSchedules() []wantSchedule {
	out := append(schedules00022(), schedules00025()...)
	return append(out, schedules00026()...)
}

// schedules00022 is the original four (p26.28); unchanged.
func schedules00022() []wantSchedule {
	return []wantSchedule{
		{name: "Monthly (1st)", kind: "monthly", dayOfMonth: ip(1)},
		{name: "Monthly (last day)", kind: "monthly", dayOfMonth: ip(-1)},
		{name: "Weekly (Friday)", kind: "weekly", weekday: ip(5), anchor: sp("2000-01-07")},
		{name: "Semimonthly (15th & last)", kind: "semimonthly", dayOfMonth: ip(15), dayOfMon2: ip(-1)},
	}
}

// schedules00025 is the fuller common set added by 00025 (p26.79). Each row is a
// recurrence the model CAN express and none duplicates an 00022 name: another
// weekly weekday (Monday), the biweekly stride kind, the complementary semimonthly
// pairing (1st & 15th), a mid-month monthly, and an annual anchor. 2000-01-03 is a
// Monday and 2000-01-07 a Friday (the biweekly stride anchor); the annual anchor's
// weekday is irrelevant (annual repeats a fixed month+day).
func schedules00025() []wantSchedule {
	return []wantSchedule{
		{name: "Weekly (Monday)", kind: "weekly", weekday: ip(1), anchor: sp("2000-01-03")},
		{name: "Biweekly", kind: "biweekly", anchor: sp("2000-01-07")},
		{name: "Semimonthly (1st & 15th)", kind: "semimonthly", dayOfMonth: ip(1), dayOfMon2: ip(15)},
		{name: "Monthly (15th)", kind: "monthly", dayOfMonth: ip(15)},
		{name: "Annual (Jan 1)", kind: "annual", anchor: sp("2000-01-01")},
	}
}

// schedules00026 is the two DATED (explicit-date) schedules added by 00026 (p26.84):
// kind='custom' rows whose occurrences are an explicit list of calendar days in the
// budget_schedule_dates child table (NOT a recurrence rule). They give quarterly and
// semiannual budgeting WITHOUT a >monthly engine kind (the owner's no-engine-change
// constraint) -- expandCustom yields exactly the stored dates. A representative 2026
// year; all four dates are 2026 weekdays. All rule-based columns are NULL.
func schedules00026() []wantSchedule {
	return []wantSchedule{
		{
			name: "Quarterly (quarter-end)", kind: "custom",
			customDates: []string{"2026-03-31", "2026-06-30", "2026-09-30", "2026-12-31"},
		},
		{
			name: "Semiannual (Jun 30 & Dec 31)", kind: "custom",
			customDates: []string{"2026-06-30", "2026-12-31"},
		},
	}
}

// datedSchedules is just the p26.84 custom rows -- the ones with an explicit child-date
// list -- so the dedicated wiring test can assert schedule row -> child dates -> engine.
func datedSchedules() []wantSchedule { return schedules00026() }

// TestStandardSchedulesSeeded proves the four standard schedules exist on a fresh
// migrated db with the expected kind + params.
func TestStandardSchedulesSeeded(t *testing.T) {
	sqldb := testutil.NewDB(t)

	for _, w := range standardSchedules() {
		var (
			kind          string
			dom, dom2     sql.NullInt64
			ordinal, wday sql.NullInt64
			anchor        sql.NullString
		)
		err := sqldb.QueryRow(
			`SELECT kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date
			   FROM budget_schedules WHERE name = ?`, w.name,
		).Scan(&kind, &dom, &dom2, &ordinal, &wday, &anchor)
		if err != nil {
			t.Fatalf("standard schedule %q not found (seed missing?): %v", w.name, err)
		}
		if kind != w.kind {
			t.Errorf("%q kind = %q, want %q", w.name, kind, w.kind)
		}
		assertNullInt(t, w.name, "day_of_month", dom, w.dayOfMonth)
		assertNullInt(t, w.name, "day_of_month_2", dom2, w.dayOfMon2)
		assertNullInt(t, w.name, "ordinal", ordinal, w.ordinal)
		assertNullInt(t, w.name, "weekday", wday, w.weekday)
		assertNullStr(t, w.name, "anchor_date", anchor, w.anchor)
	}
}

// TestStandardSchedulesVersioned proves each seeded schedule has an op='create'
// version snapshot (rule 5) whose snapshot columns match the live row and whose
// valid_from equals its changes.at (the audit-consistent-seed invariant Z3 checks).
func TestStandardSchedulesVersioned(t *testing.T) {
	sqldb := testutil.NewDB(t)

	for _, w := range standardSchedules() {
		var n int
		if err := sqldb.QueryRow(
			`SELECT count(*)
			   FROM budget_schedules s
			   JOIN budget_schedules_versions v ON v.entity_id = s.id
			   JOIN changes c ON c.id = v.change_id
			  WHERE s.name = ?
			    AND v.op = 'create'
			    AND v.valid_from = c.at
			    AND v.name IS s.name AND v.kind IS s.kind
			    AND v.day_of_month IS s.day_of_month
			    AND v.day_of_month_2 IS s.day_of_month_2
			    AND v.ordinal IS s.ordinal AND v.weekday IS s.weekday
			    AND v.anchor_date IS s.anchor_date
			    AND v.weekend_adjust IS s.weekend_adjust
			    AND v.notes IS s.notes`, w.name,
		).Scan(&n); err != nil {
			t.Fatalf("query version for %q: %v", w.name, err)
		}
		if n != 1 {
			t.Errorf("%q: found %d matching create-version rows, want 1 (live/snapshot mismatch or missing version)", w.name, n)
		}
	}
}

// TestStandardSchedulesEngineValid proves every seeded param set is one the schedule
// engine accepts: ExpandSchedule over a one-year horizon returns without error and
// yields occurrences (a valid schedule is one the editor would accept, ErrSchedule
// Invalid-clean).
func TestStandardSchedulesEngineValid(t *testing.T) {
	sqldb := testutil.NewDB(t)

	rows, err := sqldb.Query(
		`SELECT id, name, kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date, weekend_adjust
		   FROM budget_schedules ORDER BY id`)
	if err != nil {
		t.Fatalf("list seeded schedules: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var (
			id                   int64
			name, kind, weekend  string
			dom, dom2, ord, wday sql.NullInt64
			anchor               sql.NullString
		)
		if err := rows.Scan(&id, &name, &kind, &dom, &dom2, &ord, &wday, &anchor, &weekend); err != nil {
			t.Fatalf("scan schedule: %v", err)
		}
		seen++
		sched := budget.Schedule{
			Kind:          kind,
			DayOfMonth:    intOr0(dom),
			DayOfMonth2:   intOr0(dom2),
			Ordinal:       intOr0(ord),
			Weekday:       intOr0(wday),
			AnchorDate:    strOrEmpty(anchor),
			WeekendAdjust: weekend,
		}
		// A dated (kind='custom') schedule carries its occurrences in the child date
		// table, not the parent columns (p26.84). Load them so the engine can expand it;
		// without this, expandCustom sees an empty list and rejects the schedule.
		if kind == budget.KindCustom {
			sched.CustomDates = loadCustomDates(t, sqldb, id)
		}
		occ, err := budget.ExpandSchedule(sched, "2026-01-01", "2026-12-31")
		if err != nil {
			t.Errorf("seeded schedule %q rejected by engine: %v", name, err)
			continue
		}
		if len(occ) == 0 {
			t.Errorf("seeded schedule %q yielded no occurrences over 2026 (unexpectedly empty)", name)
		}
	}
	if seen != len(standardSchedules()) {
		t.Errorf("engine-validated %d seeded schedules, want %d", seen, len(standardSchedules()))
	}
}

// TestDatedSchedulesExpand proves the p26.84 dated (kind='custom') schedules are wired
// end to end: each seeded schedule row has its explicit dates in budget_schedule_dates,
// and feeding those to budget.ExpandSchedule over a covering 2026 horizon yields EXACTLY
// the stored dates (sorted) -- i.e. quarterly / semiannual budgeting works via the dated
// path, with NO recurrence-engine change. It loads the dates from the db (not the want
// literal) so it also proves the migration seeded the child rows.
func TestDatedSchedulesExpand(t *testing.T) {
	sqldb := testutil.NewDB(t)

	for _, w := range datedSchedules() {
		var id int64
		var kind string
		if err := sqldb.QueryRow(
			`SELECT id, kind FROM budget_schedules WHERE name = ?`, w.name,
		).Scan(&id, &kind); err != nil {
			t.Fatalf("dated schedule %q not found (seed missing?): %v", w.name, err)
		}
		if kind != budget.KindCustom {
			t.Errorf("%q kind = %q, want %q (a dated schedule is kind=custom)", w.name, kind, budget.KindCustom)
		}
		dates := loadCustomDates(t, sqldb, id)
		if len(dates) != len(w.customDates) {
			t.Fatalf("%q: seeded %d child dates, want %d (%v)", w.name, len(dates), len(w.customDates), dates)
		}
		occ, err := budget.ExpandSchedule(budget.Schedule{Kind: kind, CustomDates: dates}, "2026-01-01", "2026-12-31")
		if err != nil {
			t.Fatalf("%q rejected by engine: %v", w.name, err)
		}
		if !equalStrings(occ, w.customDates) {
			t.Errorf("%q expanded to %v, want the explicit dates %v", w.name, occ, w.customDates)
		}
	}
}

// TestDatedSchedulesDatesVersioned proves each seeded explicit date carries its
// op='create' membership version (rule 5) -- the set-versioned child twin, so the
// budget_schedule_dates parity check in cuento check stays clean.
func TestDatedSchedulesDatesVersioned(t *testing.T) {
	sqldb := testutil.NewDB(t)

	for _, w := range datedSchedules() {
		for _, d := range w.customDates {
			var n int
			if err := sqldb.QueryRow(
				`SELECT count(*)
				   FROM budget_schedule_dates c
				   JOIN budget_schedules s ON s.id = c.schedule_id
				   JOIN budget_schedule_dates_versions v
				     ON v.entity_id = c.schedule_id AND v.occurs_on = c.occurs_on
				   JOIN changes ch ON ch.id = v.change_id
				  WHERE s.name = ? AND c.occurs_on = ?
				    AND v.op = 'create' AND v.valid_from = ch.at`, w.name, d,
			).Scan(&n); err != nil {
				t.Fatalf("query date version for %q %s: %v", w.name, d, err)
			}
			if n != 1 {
				t.Errorf("%q date %s: found %d create-version rows, want 1", w.name, d, n)
			}
		}
	}
}

// --- helpers -----------------------------------------------------------------

// loadCustomDates returns a schedule's explicit occurrence dates from the child table,
// sorted ascending (the on-disk order the composite PK yields).
func loadCustomDates(t *testing.T, sqldb *sql.DB, scheduleID int64) []string {
	t.Helper()
	rows, err := sqldb.Query(
		`SELECT occurs_on FROM budget_schedule_dates WHERE schedule_id = ? ORDER BY occurs_on`, scheduleID)
	if err != nil {
		t.Fatalf("load custom dates for schedule %d: %v", scheduleID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatalf("scan custom date: %v", err)
		}
		out = append(out, d)
	}
	return out
}

// equalStrings reports whether two string slices are element-wise equal.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func assertNullInt(t *testing.T, name, col string, got sql.NullInt64, want *int) {
	t.Helper()
	if want == nil {
		if got.Valid {
			t.Errorf("%q %s = %d, want NULL", name, col, got.Int64)
		}
		return
	}
	if !got.Valid || got.Int64 != int64(*want) {
		t.Errorf("%q %s = %v, want %d", name, col, got, *want)
	}
}

func assertNullStr(t *testing.T, name, col string, got sql.NullString, want *string) {
	t.Helper()
	if want == nil {
		if got.Valid {
			t.Errorf("%q %s = %q, want NULL", name, col, got.String)
		}
		return
	}
	if !got.Valid || got.String != *want {
		t.Errorf("%q %s = %v, want %q", name, col, got, *want)
	}
}

func intOr0(v sql.NullInt64) int {
	if v.Valid {
		return int(v.Int64)
	}
	return 0
}

func strOrEmpty(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}
