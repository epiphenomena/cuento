package budget

import (
	"reflect"
	"testing"
)

// Schedule-expansion table tests (p19.1) -- the TESTED CORE of budgeting. Each
// case's expected dates are HAND-VERIFIED against a calendar (never computed with
// the same logic under test), so a bug in the engine cannot hide behind a circular
// expectation. Facts used (all 2025 unless noted): Mar 15 = Sat, Feb 15 = Sat,
// Nov 30 = Sun, Aug 31 = Sun, Aug 15 = Fri, Feb 28 = Fri, Nov 28 = Fri; biweekly
// anchor Dec 19 2025 = Fri, +14 -> Jan 2 / Jan 16 / Jan 30 2026 (Fri).

func TestExpandSchedule(t *testing.T) {
	cases := []struct {
		name     string
		sched    Schedule
		from, to string
		want     []string
	}{
		// --- onetime -------------------------------------------------------
		{
			name:  "onetime in horizon",
			sched: Schedule{Kind: KindOnetime, AnchorDate: "2025-06-15"},
			from:  "2025-01-01", to: "2025-12-31",
			want: []string{"2025-06-15"},
		},
		{
			name:  "onetime outside horizon yields nothing",
			sched: Schedule{Kind: KindOnetime, AnchorDate: "2024-06-15"},
			from:  "2025-01-01", to: "2025-12-31",
			want: []string{},
		},

		// --- annual --------------------------------------------------------
		{
			name:  "annual one date per year over multi-year horizon",
			sched: Schedule{Kind: KindAnnual, AnchorDate: "2025-07-04"},
			from:  "2025-01-01", to: "2027-12-31",
			want: []string{"2025-07-04", "2026-07-04", "2027-07-04"},
		},
		{
			name:  "annual Feb-29 anchor clamps to Feb-28 in a non-leap year",
			sched: Schedule{Kind: KindAnnual, AnchorDate: "2024-02-29"},
			from:  "2025-01-01", to: "2025-12-31",
			want: []string{"2025-02-28"},
		},
		{
			// Jul 4 2026 = Saturday -- annual is a fixed calendar day, NOT weekend
			// -adjusted even when a weekend policy is set.
			name:  "annual on a weekend is NOT adjusted",
			sched: Schedule{Kind: KindAnnual, AnchorDate: "2026-07-04", WeekendAdjust: WeekendPrevBiz},
			from:  "2026-01-01", to: "2026-12-31",
			want: []string{"2026-07-04"},
		},

		// --- monthly by day-of-month --------------------------------------
		{
			// Mar 15 = Sat -> prev_business_day rolls to Mar 14 (Fri); Feb 15 = Sat
			// -> Feb 14 (Fri); Jan 15 = Wed (unaffected).
			name:  "monthly day-of-month with prev_business_day weekend roll",
			sched: Schedule{Kind: KindMonthly, DayOfMonth: 15, WeekendAdjust: WeekendPrevBiz},
			from:  "2025-01-01", to: "2025-03-31",
			want: []string{"2025-01-15", "2025-02-14", "2025-03-14"},
		},
		{
			// Same days, next_business_day: Sat -> Monday.
			name:  "monthly day-of-month with next_business_day weekend roll",
			sched: Schedule{Kind: KindMonthly, DayOfMonth: 15, WeekendAdjust: WeekendNextBiz},
			from:  "2025-02-01", to: "2025-03-31",
			want: []string{"2025-02-17", "2025-03-17"},
		},
		{
			// actual policy: a weekend date is kept as-is.
			name:  "monthly day-of-month with actual keeps the weekend date",
			sched: Schedule{Kind: KindMonthly, DayOfMonth: 15, WeekendAdjust: WeekendActual},
			from:  "2025-03-01", to: "2025-03-31",
			want: []string{"2025-03-15"},
		},
		{
			// day 31 in Feb 2025 clamps to Feb 28 (Fri, no roll) -- short-month clamp.
			name:  "monthly day 31 clamps to short-month end",
			sched: Schedule{Kind: KindMonthly, DayOfMonth: 31, WeekendAdjust: WeekendPrevBiz},
			from:  "2025-02-01", to: "2025-02-28",
			want: []string{"2025-02-28"},
		},
		{
			// THE combined case: day 31 in Nov 2025 clamps to Nov 30 (Sun), which
			// THEN weekend-rolls back to Nov 28 (Fri). Clamp-before-adjust order.
			name:  "monthly day 31 clamps to Nov 30 (Sun) then rolls to Nov 28 (Fri)",
			sched: Schedule{Kind: KindMonthly, DayOfMonth: 31, WeekendAdjust: WeekendPrevBiz},
			from:  "2025-11-01", to: "2025-11-30",
			want: []string{"2025-11-28"},
		},
		{
			// month-end sentinel (-1) resolves to the actual last day each month.
			name:  "monthly month-end sentinel",
			sched: Schedule{Kind: KindMonthly, DayOfMonth: lastDayOfMonth, WeekendAdjust: WeekendActual},
			from:  "2025-01-01", to: "2025-03-31",
			want: []string{"2025-01-31", "2025-02-28", "2025-03-31"},
		},

		// --- monthly by ordinal weekday -----------------------------------
		{
			// 2nd Monday of each month; Mar 2025 -> Mar 10. Ordinal weekday is
			// weekday-anchored: NO weekend adjustment (policy ignored).
			name:  "monthly 2nd Monday",
			sched: Schedule{Kind: KindMonthly, Ordinal: 2, Weekday: 1, WeekendAdjust: WeekendPrevBiz},
			from:  "2025-03-01", to: "2025-03-31",
			want: []string{"2025-03-10"},
		},
		{
			// last Friday of Mar 2025 -> Mar 28.
			name:  "monthly last Friday",
			sched: Schedule{Kind: KindMonthly, Ordinal: ordinalLast, Weekday: 5},
			from:  "2025-03-01", to: "2025-03-31",
			want: []string{"2025-03-28"},
		},

		// --- semimonthly ---------------------------------------------------
		{
			// 15 + last-day, Aug 2025: 15 = Fri (kept); 31 = Sun -> prev -> Aug 29
			// (Fri). Two days per month.
			name:  "semimonthly 15 + month-end with weekend roll",
			sched: Schedule{Kind: KindSemimonthly, DayOfMonth: 15, DayOfMonth2: lastDayOfMonth, WeekendAdjust: WeekendPrevBiz},
			from:  "2025-08-01", to: "2025-08-31",
			want: []string{"2025-08-15", "2025-08-29"},
		},
		{
			// 30 + 31 in Feb 2025 both clamp to Feb 28 -> deduplicated to one date.
			name:  "semimonthly 30 + 31 both clamp to Feb 28 and dedupe",
			sched: Schedule{Kind: KindSemimonthly, DayOfMonth: 30, DayOfMonth2: 31, WeekendAdjust: WeekendActual},
			from:  "2025-02-01", to: "2025-02-28",
			want: []string{"2025-02-28"},
		},

		// --- biweekly ------------------------------------------------------
		{
			// Anchor Dec 19 2025 (Fri). Over Dec 2025..Feb 2026 the +14 stride gives
			// Dec 19, then Jan 2 / Jan 16 / Jan 30 (3-in-a-month), then Feb 13 / Feb
			// 27 -- CROSSING the year boundary; and Dec 5 by stepping BACKWARD from
			// the anchor. Weekday-anchored: no weekend adjustment.
			name:  "biweekly crosses year boundary with a 3-in-a-month month",
			sched: Schedule{Kind: KindBiweekly, AnchorDate: "2025-12-19"},
			from:  "2025-12-01", to: "2026-02-28",
			want: []string{
				"2025-12-05", "2025-12-19",
				"2026-01-02", "2026-01-16", "2026-01-30",
				"2026-02-13", "2026-02-27",
			},
		},
		{
			// Anchor AFTER the horizon start is still reached by stepping backward.
			name:  "biweekly with anchor after horizon start (step backward)",
			sched: Schedule{Kind: KindBiweekly, AnchorDate: "2025-02-14"},
			from:  "2025-01-01", to: "2025-02-28",
			want: []string{"2025-01-03", "2025-01-17", "2025-01-31", "2025-02-14", "2025-02-28"},
		},

		// --- weekly --------------------------------------------------------
		{
			// Mondays, anchored to Jan 6 2025 (a Monday). Weekday-anchored: no roll.
			name:  "weekly Mondays",
			sched: Schedule{Kind: KindWeekly, Weekday: 1, AnchorDate: "2025-01-06"},
			from:  "2025-01-01", to: "2025-01-31",
			want: []string{"2025-01-06", "2025-01-13", "2025-01-20", "2025-01-27"},
		},
		{
			// Anchor need not itself be the target weekday: anchor Wed Jan 1, want
			// Fridays -> first Friday Jan 3.
			name:  "weekly Fridays from a non-Friday anchor",
			sched: Schedule{Kind: KindWeekly, Weekday: 5, AnchorDate: "2025-01-01"},
			from:  "2025-01-01", to: "2025-01-31",
			want: []string{"2025-01-03", "2025-01-10", "2025-01-17", "2025-01-24", "2025-01-31"},
		},

		// --- custom --------------------------------------------------------
		{
			name: "custom explicit list filtered to horizon and sorted",
			sched: Schedule{Kind: KindCustom, CustomDates: []string{
				"2025-09-30", "2025-03-15", "2024-12-31", "2025-03-15",
			}},
			from: "2025-01-01", to: "2025-12-31",
			// 2024-12-31 out of horizon; the duplicate 2025-03-15 collapses.
			want: []string{"2025-03-15", "2025-09-30"},
		},

		// --- default weekend policy ---------------------------------------
		{
			// Empty WeekendAdjust defaults to prev_business_day: Mar 15 = Sat -> Mar
			// 14 (Fri).
			name:  "empty weekend policy defaults to prev_business_day",
			sched: Schedule{Kind: KindMonthly, DayOfMonth: 15},
			from:  "2025-03-01", to: "2025-03-31",
			want: []string{"2025-03-14"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpandSchedule(tc.sched, tc.from, tc.to)
			if err != nil {
				t.Fatalf("ExpandSchedule: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ExpandSchedule\n got  %v\n want %v", got, tc.want)
			}
		})
	}
}

// TestExpandScheduleErrors covers the reject paths (bad inputs), so a store caller
// can surface a clean error.
func TestExpandScheduleErrors(t *testing.T) {
	cases := []struct {
		name     string
		sched    Schedule
		from, to string
	}{
		{"unknown kind", Schedule{Kind: "weird"}, "2025-01-01", "2025-12-31"},
		{"bad from", Schedule{Kind: KindOnetime, AnchorDate: "2025-01-01"}, "nope", "2025-12-31"},
		{"bad to", Schedule{Kind: KindOnetime, AnchorDate: "2025-01-01"}, "2025-01-01", "nope"},
		{"reversed horizon", Schedule{Kind: KindOnetime, AnchorDate: "2025-01-01"}, "2025-12-31", "2025-01-01"},
		{"onetime bad anchor", Schedule{Kind: KindOnetime, AnchorDate: "2025-02-30"}, "2025-01-01", "2025-12-31"},
		{"monthly needs exactly one selector", Schedule{Kind: KindMonthly}, "2025-01-01", "2025-12-31"},
		{"monthly both selectors set", Schedule{Kind: KindMonthly, DayOfMonth: 15, Ordinal: 2, Weekday: 1}, "2025-01-01", "2025-12-31"},
		{"semimonthly needs both days", Schedule{Kind: KindSemimonthly, DayOfMonth: 15}, "2025-01-01", "2025-12-31"},
		{"biweekly needs anchor", Schedule{Kind: KindBiweekly}, "2025-01-01", "2025-12-31"},
		{"weekly bad weekday", Schedule{Kind: KindWeekly, Weekday: 9, AnchorDate: "2025-01-01"}, "2025-01-01", "2025-12-31"},
		{"custom empty list", Schedule{Kind: KindCustom}, "2025-01-01", "2025-12-31"},
		{"custom bad date", Schedule{Kind: KindCustom, CustomDates: []string{"2025-13-01"}}, "2025-01-01", "2025-12-31"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ExpandSchedule(tc.sched, tc.from, tc.to); err == nil {
				t.Fatalf("ExpandSchedule(%+v) = nil error, want error", tc.sched)
			}
		})
	}
}
