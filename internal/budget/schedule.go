// Package budget holds the PURE, deterministic schedule-expansion engine for
// budgeting (p19.1). A budget's amounts land in FULL on DISCRETE occurrence dates
// (no pro-rata, PLAN Phase 19): a named, reusable schedule + a horizon (from,to)
// expand to a sorted, deduplicated list of "YYYY-MM-DD" occurrence dates. Reports
// (p19.2+) bucket those occurrences by their reporting period and sum.
//
// The engine is intentionally CLOCK-FREE and DB-FREE (AGENTS: never time.Now() on
// a computed path; the horizon is a parameter): it is exhaustively table-testable
// and reproducible. It uses stdlib time only for calendar arithmetic (all in UTC),
// never for "now". No new dependency (AGENTS rule 1).
package budget

import (
	"fmt"
	"sort"
	"time"
)

// dateLayout is the canonical wire/storage date shape used everywhere in cuento
// (transactions.date, funds.start_date, ...): a plain YYYY-MM-DD calendar day with
// no time-of-day and no zone. Expansion produces and consumes exactly this.
const dateLayout = "2006-01-02"

// Schedule kinds (the migration's CHECK enum, mirrored here as typed constants so
// the pure engine never depends on the sqlc row).
const (
	KindOnetime     = "onetime"
	KindAnnual      = "annual"
	KindMonthly     = "monthly"
	KindSemimonthly = "semimonthly"
	KindBiweekly    = "biweekly"
	KindWeekly      = "weekly"
	KindCustom      = "custom"
)

// Weekend-adjust policies (the migration's CHECK enum). Applied ONLY to
// day-of-month kinds (monthly-by-DoM, semimonthly) -- ordinal-weekday, biweekly,
// and weekly are weekday-anchored by construction and never need adjusting.
const (
	WeekendActual  = "actual"            // leave a Sat/Sun date as-is
	WeekendPrevBiz = "prev_business_day" // roll back to the preceding Friday (DEFAULT)
	WeekendNextBiz = "next_business_day" // roll forward to the following Monday
	DefaultWeekend = WeekendPrevBiz
	lastDayOfMonth = -1 // sentinel DayOfMonth meaning "the month's last day"
	ordinalLast    = -1 // Ordinal sentinel meaning "the last <weekday> of the month"
)

// Schedule is the pure, DB-free description of a recurrence. It mirrors the
// budget_schedules business columns, but as plain Go values so ExpandSchedule
// stays testable without sqlc. Only the fields relevant to Kind are read.
//
//   - Onetime/Annual: AnchorDate (Annual repeats its month+day each year; a
//     Feb-29 anchor clamps to Feb-28 in a non-leap year).
//   - Monthly: EITHER DayOfMonth (1..31, or lastDayOfMonth=-1 for month-end;
//     clamps down for short months) OR an ordinal weekday (Ordinal 1..4 or
//     ordinalLast=-1, Weekday 0=Sun..6=Sat).
//   - Semimonthly: DayOfMonth + DayOfMonth2 (each 1..31 or -1=month-end; both
//     clamp; a collision after clamping is deduplicated).
//   - Biweekly: every 14 days from AnchorDate (both directions from the anchor).
//   - Weekly: the Weekday recurring weekly, aligned to AnchorDate's week.
//   - Custom: an explicit CustomDates list (imported), each "YYYY-MM-DD".
type Schedule struct {
	Kind          string
	DayOfMonth    int    // 1..31 or -1 (month-end); 0 = unset
	DayOfMonth2   int    // second semimonthly day; 0 = unset
	Ordinal       int    // 1..4 or -1 (last); 0 = unset (monthly-by-DoM instead)
	Weekday       int    // 0..6 (Sun..Sat)
	AnchorDate    string // "YYYY-MM-DD"; "" = unset
	WeekendAdjust string // one of the Weekend* policies; "" defaults to prev_business_day
	CustomDates   []string
}

// ExpandSchedule returns the schedule's occurrence dates within the INCLUSIVE
// horizon [from, to], as sorted, de-duplicated "YYYY-MM-DD" strings. It is pure:
// same inputs -> same output, no clock, no DB. from/to are "YYYY-MM-DD"; an
// invalid date, an out-of-order horizon, or a malformed schedule field returns an
// error (validated up front so a store caller can reject cleanly).
//
// Semantics, spelled out so the table tests can be hand-verified:
//   - the horizon is INCLUSIVE of both ends;
//   - clamping happens BEFORE weekend adjustment (day 31 -> Feb 28, and only THEN
//     does a weekend roll apply);
//   - weekend adjustment applies to monthly-by-day-of-month and semimonthly ONLY;
//   - a weekend roll may move a date OUT of [from,to] (it is then dropped) or a
//     near-edge date INTO it -- membership is decided on the ADJUSTED date;
//   - results are always sorted ascending and deduplicated.
func ExpandSchedule(s Schedule, from, to string) ([]string, error) {
	fromT, err := parseDate(from)
	if err != nil {
		return nil, fmt.Errorf("expand schedule: from: %w", err)
	}
	toT, err := parseDate(to)
	if err != nil {
		return nil, fmt.Errorf("expand schedule: to: %w", err)
	}
	if toT.Before(fromT) {
		return nil, fmt.Errorf("expand schedule: horizon end %s before start %s", to, from)
	}
	policy := s.WeekendAdjust
	if policy == "" {
		policy = DefaultWeekend
	}

	var dates []time.Time
	switch s.Kind {
	case KindOnetime:
		dates, err = expandOnetime(s)
	case KindAnnual:
		dates, err = expandAnnual(s, fromT, toT)
	case KindMonthly:
		dates, err = expandMonthly(s, fromT, toT, policy)
	case KindSemimonthly:
		dates, err = expandSemimonthly(s, fromT, toT, policy)
	case KindBiweekly:
		dates, err = expandStride(s, fromT, toT, 14)
	case KindWeekly:
		dates, err = expandWeekly(s, fromT, toT)
	case KindCustom:
		dates, err = expandCustom(s)
	default:
		return nil, fmt.Errorf("expand schedule: unknown kind %q", s.Kind)
	}
	if err != nil {
		return nil, err
	}

	return filterSortUnique(dates, fromT, toT), nil
}

// --- per-kind expanders ------------------------------------------------------

// expandOnetime yields the single anchor date (kept only if it falls in-horizon,
// handled by the shared filter). No weekend adjustment (not a day-of-month kind).
func expandOnetime(s Schedule) ([]time.Time, error) {
	d, err := parseDate(s.AnchorDate)
	if err != nil {
		return nil, fmt.Errorf("onetime anchor: %w", err)
	}
	return []time.Time{d}, nil
}

// expandAnnual yields one date per calendar year in [from,to] at the anchor's
// (month, day). A Feb-29 anchor clamps to Feb-28 in a non-leap year. Weekday-
// unaware: NO weekend adjustment (an annual date is a fixed calendar day).
func expandAnnual(s Schedule, from, to time.Time) ([]time.Time, error) {
	anchor, err := parseDate(s.AnchorDate)
	if err != nil {
		return nil, fmt.Errorf("annual anchor: %w", err)
	}
	m, d := anchor.Month(), anchor.Day()
	var out []time.Time
	for y := from.Year() - 1; y <= to.Year()+1; y++ {
		out = append(out, clampDay(y, m, d))
	}
	return out, nil
}

// expandMonthly yields one date per month in the horizon, either by day-of-month
// (clamped for short months, THEN weekend-adjusted) or by ordinal weekday (e.g.
// 2nd Monday, or the LAST Friday -- weekday-anchored, never weekend-adjusted).
func expandMonthly(s Schedule, from, to time.Time, policy string) ([]time.Time, error) {
	byDoM := s.DayOfMonth != 0
	byOrdinal := s.Ordinal != 0
	if byDoM == byOrdinal {
		return nil, fmt.Errorf("monthly: need exactly one of day_of_month or ordinal+weekday")
	}
	var out []time.Time
	for _, ym := range monthsSpanning(from, to) {
		y, m := ym.y, ym.m
		if byDoM {
			d := clampDay(y, m, dayOrEnd(s.DayOfMonth, y, m))
			out = append(out, weekendAdjust(d, policy))
			continue
		}
		d, err := ordinalWeekday(y, m, s.Ordinal, s.Weekday)
		if err != nil {
			return nil, err
		}
		out = append(out, d) // ordinal weekday: no weekend adjustment
	}
	return out, nil
}

// expandSemimonthly yields TWO day-of-month dates per month (each clamped then
// weekend-adjusted). Two days that clamp to the same day (e.g. 30 + 31 in Feb)
// collapse via the shared dedupe.
func expandSemimonthly(s Schedule, from, to time.Time, policy string) ([]time.Time, error) {
	if s.DayOfMonth == 0 || s.DayOfMonth2 == 0 {
		return nil, fmt.Errorf("semimonthly: need day_of_month and day_of_month_2")
	}
	var out []time.Time
	for _, ym := range monthsSpanning(from, to) {
		y, m := ym.y, ym.m
		for _, dom := range []int{s.DayOfMonth, s.DayOfMonth2} {
			d := clampDay(y, m, dayOrEnd(dom, y, m))
			out = append(out, weekendAdjust(d, policy))
		}
	}
	return out, nil
}

// expandStride yields every stride-th day from the anchor (biweekly = 14). It
// walks BOTH directions from the anchor so an anchor before or after `from` is
// handled without a negative-modulo bug. Weekday-anchored: no weekend adjustment.
// This naturally produces 3-in-a-month months and crosses year boundaries.
func expandStride(s Schedule, from, to time.Time, stride int) ([]time.Time, error) {
	anchor, err := parseDate(s.AnchorDate)
	if err != nil {
		return nil, fmt.Errorf("%s anchor: %w", s.Kind, err)
	}
	var out []time.Time
	// Step forward from the anchor to `to`.
	for d := anchor; !d.After(to); d = d.AddDate(0, 0, stride) {
		if !d.Before(from) {
			out = append(out, d)
		}
	}
	// Step backward from the anchor to `from` (excluding the anchor itself, already
	// covered above).
	for d := anchor.AddDate(0, 0, -stride); !d.Before(from); d = d.AddDate(0, 0, -stride) {
		if !d.After(to) {
			out = append(out, d)
		}
	}
	return out, nil
}

// expandWeekly yields the schedule's Weekday every week, aligned to the anchor's
// week. It reuses the 7-day stride from the first on-or-after-anchor occurrence of
// Weekday. Weekday-anchored: no weekend adjustment.
func expandWeekly(s Schedule, from, to time.Time) ([]time.Time, error) {
	anchor, err := parseDate(s.AnchorDate)
	if err != nil {
		return nil, fmt.Errorf("weekly anchor: %w", err)
	}
	if s.Weekday < 0 || s.Weekday > 6 {
		return nil, fmt.Errorf("weekly: weekday %d out of range 0..6", s.Weekday)
	}
	// First occurrence of Weekday on or after the anchor.
	delta := (s.Weekday - int(anchor.Weekday()) + 7) % 7
	start := anchor.AddDate(0, 0, delta)
	return expandStride(Schedule{Kind: KindWeekly, AnchorDate: start.Format(dateLayout)}, from, to, 7)
}

// expandCustom yields the explicit imported date list (each validated). No
// weekend adjustment (the list is authoritative, as imported).
func expandCustom(s Schedule) ([]time.Time, error) {
	if len(s.CustomDates) == 0 {
		return nil, fmt.Errorf("custom: no dates supplied")
	}
	out := make([]time.Time, 0, len(s.CustomDates))
	for _, ds := range s.CustomDates {
		d, err := parseDate(ds)
		if err != nil {
			return nil, fmt.Errorf("custom date %q: %w", ds, err)
		}
		out = append(out, d)
	}
	return out, nil
}

// --- calendar helpers --------------------------------------------------------

// parseDate parses a strict "YYYY-MM-DD" in UTC. time.Parse rejects an impossible
// day (e.g. 2025-02-30), giving free field validation.
func parseDate(s string) (time.Time, error) {
	t, err := time.ParseInLocation(dateLayout, s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse date %q: %w", s, err)
	}
	return t, nil
}

// dayOrEnd resolves a day-of-month value to a concrete day number, translating the
// month-end sentinel (-1) to the actual last day of (y, m). A positive value is
// returned as-is (clampDay handles a value past month-end).
func dayOrEnd(dom, y int, m time.Month) int {
	if dom == lastDayOfMonth {
		return daysInMonth(y, m)
	}
	return dom
}

// clampDay builds the date (y, m, day), CLAMPING day down to the month's last day
// for short months (day 31 in Feb -> Feb 28/29). Clamping to month-end is the sane
// default (DECISIONS p19.1) -- a budget occurrence is never silently skipped.
func clampDay(y int, m time.Month, day int) time.Time {
	last := daysInMonth(y, m)
	if day > last {
		day = last
	}
	if day < 1 {
		day = 1
	}
	return time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
}

// daysInMonth returns the number of days in (y, m) via the day-0-of-next-month
// idiom (NOT overflow normalization), correct across leap years.
func daysInMonth(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// ordinalWeekday returns the ordinal-th `weekday` of (y, m): ordinal 1..4 counts
// from the month start; ordinalLast (-1) is the LAST such weekday. It is
// weekday-anchored, so the result never needs weekend adjustment.
func ordinalWeekday(y int, m time.Month, ordinal, weekday int) (time.Time, error) {
	if weekday < 0 || weekday > 6 {
		return time.Time{}, fmt.Errorf("monthly ordinal: weekday %d out of range 0..6", weekday)
	}
	if ordinal == ordinalLast {
		last := time.Date(y, m, daysInMonth(y, m), 0, 0, 0, 0, time.UTC)
		back := (int(last.Weekday()) - weekday + 7) % 7
		return last.AddDate(0, 0, -back), nil
	}
	if ordinal < 1 || ordinal > 4 {
		return time.Time{}, fmt.Errorf("monthly ordinal %d out of range 1..4 or -1(last)", ordinal)
	}
	first := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	fwd := (weekday - int(first.Weekday()) + 7) % 7
	day := 1 + fwd + (ordinal-1)*7
	if day > daysInMonth(y, m) {
		return time.Time{}, fmt.Errorf("monthly ordinal %d weekday %d does not occur in %04d-%02d", ordinal, weekday, y, int(m))
	}
	return time.Date(y, m, day, 0, 0, 0, 0, time.UTC), nil
}

// weekendAdjust applies the weekend policy to a date that lands on Sat/Sun. It is
// called ONLY for day-of-month kinds. A roll crosses the month/year boundary
// (payroll semantics: prev_business_day on Sunday-the-1st goes to the previous
// month's Friday; next_business_day on a Saturday month-end goes to the next
// month's Monday).
func weekendAdjust(d time.Time, policy string) time.Time {
	switch policy {
	case WeekendActual:
		return d
	case WeekendNextBiz:
		for isWeekend(d) {
			d = d.AddDate(0, 0, 1)
		}
		return d
	default: // WeekendPrevBiz (and the empty default)
		for isWeekend(d) {
			d = d.AddDate(0, 0, -1)
		}
		return d
	}
}

// isWeekend reports whether d falls on Saturday or Sunday (v1 has NO holiday
// calendar -- weekends only, PLAN Phase 19).
func isWeekend(d time.Time) bool {
	wd := d.Weekday()
	return wd == time.Saturday || wd == time.Sunday
}

// monthsSpanning returns the (year, month) pairs the horizon touches, inclusive of
// the months holding `from` and `to`, so a day-of-month occurrence in the first or
// last partial month is not missed (the shared filter drops any that fall outside
// the exact [from,to] day bounds).
func monthsSpanning(from, to time.Time) []struct {
	y int
	m time.Month
} {
	var out []struct {
		y int
		m time.Month
	}
	cur := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(to.Year(), to.Month(), 1, 0, 0, 0, 0, time.UTC)
	for !cur.After(end) {
		out = append(out, struct {
			y int
			m time.Month
		}{cur.Year(), cur.Month()})
		cur = cur.AddDate(0, 1, 0)
	}
	return out
}

// filterSortUnique keeps only dates within the INCLUSIVE [from,to] horizon, sorts
// ascending, and removes duplicates (two clamped semimonthly days, a weekend roll
// landing on an existing date, etc.). Returns "YYYY-MM-DD" strings.
func filterSortUnique(dates []time.Time, from, to time.Time) []string {
	inHorizon := make([]time.Time, 0, len(dates))
	for _, d := range dates {
		if d.Before(from) || d.After(to) {
			continue
		}
		inHorizon = append(inHorizon, d)
	}
	sort.Slice(inHorizon, func(i, j int) bool { return inHorizon[i].Before(inHorizon[j]) })

	out := make([]string, 0, len(inHorizon))
	var prev string
	for _, d := range inHorizon {
		s := d.Format(dateLayout)
		if s == prev {
			continue
		}
		out = append(out, s)
		prev = s
	}
	return out
}
