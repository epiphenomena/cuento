package money

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// This file owns the DATE side of the small format enumerations (D16, AGENTS
// rule 10): FormatDate and ParseDate are the ONLY sanctioned entry points for
// rendering and parsing dates in the UI. Because they are the funnel, using
// time.Time.Format / time.Parse with a layout constant *here* is correct — the
// rule forbids reaching for time.Format directly in a template path, not inside
// these formatters. Dates are represented date-only as UTC midnight for now; a
// dedicated ledger.Date type arrives in phase 8 and can wrap this. The number
// enums (NumberFormat, etc.) live in amount.go and are not duplicated here.

// DateFormat selects the day/month/year rendering and the accepted entry layout.
// Kept a small enum by design (D16) — no CLDR. Matches the bare int-const style
// of amount.go's enums (no String(); display names arrive with the settings UI).
type DateFormat int

const (
	// DateISO renders 2006-01-02 (YYYY-MM-DD). Also always accepted on input
	// regardless of the active setting (D16).
	DateISO DateFormat = iota
	// DateUS renders 01/02/2006 (MM/DD/YYYY) — the conventional US ordering.
	DateUS
	// DateEU renders 02/01/2006 (DD/MM/YYYY) — the conventional European ordering.
	DateEU
)

// Reference-time layout strings (Mon Jan 2 15:04:05 MST 2006). ISO uses dashes;
// US/EU use slashes, so no input string is ambiguous between formats — which is
// exactly why ISO can always be accepted alongside the active setting.
const (
	layoutISO = "2006-01-02"
	layoutUS  = "01/02/2006"
	layoutEU  = "02/01/2006"
)

// layout returns the reference-time layout for the format (ISO for unknown).
func (df DateFormat) layout() string {
	switch df {
	case DateUS:
		return layoutUS
	case DateEU:
		return layoutEU
	default: // DateISO
		return layoutISO
	}
}

// FormatDate renders the date portion (year/month/day) of t per df. The clock
// portion of t is ignored; callers pass date-only values (UTC midnight).
func FormatDate(t time.Time, df DateFormat) string {
	return t.Format(df.layout())
}

// ParseDate parses s as a date per df, forgivingly (D16, p23.3). It tries, in
// order: (1) the active format's full layout, (2) full ISO YYYY-MM-DD — always
// accepted regardless of df, (3) a flexible SHORT/partial numeric form. It returns
// a date-only time.Time at UTC midnight.
//
// Flexible forms (p23.3) are dash/slash/dot-separated integers, most-significant
// first, BIG-ENDIAN regardless of df (DECISIONS p23.3 — the user's examples are
// Y-M-D / M-D):
//
//	26-6-1 -> 2026-06-01   (2-digit year expanded; single-digit month/day)
//	6-1    -> <now.Year>-06-01   (year omitted -> the reference year)
//
// A 2-digit year pivots 00–68 -> 2000–2068, 69–99 -> 1900–1999 (the strptime %y
// convention, DECISIONS p23.3). now supplies the implied year for the 2-part form;
// it is passed in (never read from the wall clock here) so parsing stays
// deterministic and testable.
//
// Malformed and impossible dates (month 13, day 40, Feb 30, Feb 29 in a non-leap
// year, trailing garbage) are rejected: the strict layouts validate via
// time.Parse, and the flexible path range-checks then round-trips through
// time.Date so an overflow (Feb 30 -> Mar 2) fails rather than normalizing.
func ParseDate(s string, df DateFormat, now time.Time) (time.Time, error) {
	raw := s
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("parse date %q: empty", raw)
	}

	// Strict full layouts own the unambiguous full renderings: the active format
	// first, then ISO as an always-accepted fallback (when df is ISO the two are
	// identical; the duplicate attempt is harmless). US/EU full dates are matched
	// here and are NOT reinterpreted by the big-endian flexible path below.
	if t, err := time.Parse(df.layout(), s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(layoutISO, s); err == nil {
		return t, nil
	}
	// Flexible short/partial numeric entry (p23.3).
	if t, ok := parseFlexibleDate(s, now); ok {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("parse date %q: not a valid date for the selected format", raw)
}

// parseFlexibleDate parses the p23.3 short/partial numeric forms: dash/slash/dot
// separated integers, most-significant first. Three parts are Y-M-D; two parts are
// M-D with the year taken from now. A 2-digit year is pivoted. It returns ok=false
// (never an error) so ParseDate can fall through to its single error return.
//
// Leniency (accepted, do not change): strings.FieldsFunc IGNORES empty
// separator-delimited fields, so runs of separators and leading/trailing separators
// collapse -- "2025--3-1", "2025-3-1-", "/3/1" all parse as if the empties were
// absent. This is deliberately forgiving for hand-typed input; the digits, not the
// separator placement, carry the meaning (mirrors Parse's grouping-separator stance).
func parseFlexibleDate(s string, now time.Time) (time.Time, bool) {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '/' || r == '.' })
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return time.Time{}, false // non-numeric component -> not a flexible date
		}
		nums = append(nums, n)
	}

	var y, m, d int
	switch len(nums) {
	case 3:
		y, m, d = expandYear(nums[0], parts[0]), nums[1], nums[2]
	case 2:
		y, m, d = now.Year(), nums[0], nums[1]
	default:
		return time.Time{}, false
	}
	return validDate(y, m, d)
}

// expandYear widens a written year to four digits: a 1- or 2-digit field pivots
// (00–68 -> 2000s, 69–99 -> 1900s, the strptime %y convention); a 3-/4-digit
// field is taken as written.
func expandYear(y int, field string) int {
	if len(field) <= 2 {
		if y <= 68 {
			return 2000 + y
		}
		return 1900 + y
	}
	return y
}

// validDate builds a UTC-midnight date and rejects out-of-range or overflowing
// components: month/day bounds are checked, then a time.Date round-trip catches an
// impossible day-of-month (Feb 30 would normalize to Mar 2, so the components no
// longer match — reject).
func validDate(y, m, d int) (time.Time, bool) {
	if m < 1 || m > 12 || d < 1 || d > 31 {
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	if t.Year() != y || int(t.Month()) != m || t.Day() != d {
		return time.Time{}, false
	}
	return t, true
}
