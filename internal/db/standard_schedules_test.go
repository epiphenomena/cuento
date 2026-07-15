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
}

func ip(v int) *int       { return &v }
func sp(v string) *string { return &v }

// standardSchedules is the exact seeded set the migration must produce.
func standardSchedules() []wantSchedule {
	return []wantSchedule{
		{name: "Monthly (1st)", kind: "monthly", dayOfMonth: ip(1)},
		{name: "Monthly (last day)", kind: "monthly", dayOfMonth: ip(-1)},
		{name: "Weekly (Friday)", kind: "weekly", weekday: ip(5), anchor: sp("2000-01-07")},
		{name: "Semimonthly (15th & last)", kind: "semimonthly", dayOfMonth: ip(15), dayOfMon2: ip(-1)},
	}
}

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
		`SELECT name, kind, day_of_month, day_of_month_2, ordinal, weekday, anchor_date, weekend_adjust
		   FROM budget_schedules ORDER BY id`)
	if err != nil {
		t.Fatalf("list seeded schedules: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var (
			name, kind, weekend  string
			dom, dom2, ord, wday sql.NullInt64
			anchor               sql.NullString
		)
		if err := rows.Scan(&name, &kind, &dom, &dom2, &ord, &wday, &anchor, &weekend); err != nil {
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

// --- helpers -----------------------------------------------------------------

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
