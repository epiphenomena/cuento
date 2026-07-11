package money

import (
	"fmt"
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

// ParseDate parses s as a date per df, but ALSO always accepts ISO YYYY-MM-DD
// regardless of df (D16) — text entry stays forgiving of the machine format. It
// returns a date-only time.Time at UTC midnight. Malformed and impossible dates
// (month 13, day 40, Feb 30, Feb 29 in a non-leap year, trailing garbage) are
// rejected: time.Parse validates ranges and day-of-month strictly (it rejects
// Feb 30 via its internal daysIn), so no lenient normalization slips through and
// no manual component re-check is needed.
func ParseDate(s string, df DateFormat) (time.Time, error) {
	raw := s
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("parse date %q: empty", raw)
	}

	// Try the active format first, then ISO as an always-accepted fallback.
	// (When df is ISO the two are identical; the duplicate attempt is harmless.)
	if t, err := time.Parse(df.layout(), s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(layoutISO, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("parse date %q: not a valid date for the selected format", raw)
}
