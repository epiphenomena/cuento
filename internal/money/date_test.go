package money

import (
	"strings"
	"testing"
	"time"
)

// A discriminating date: US (MM/DD/YYYY) and EU (DD/MM/YYYY) renderings differ
// visibly, so a swapped layout fails loudly.
var march4 = time.Date(2025, 3, 4, 0, 0, 0, 0, time.UTC)

// refNow is the fixed reference date the flexible parser uses to supply an omitted
// year (p23.3). Kept deterministic so a 2-part "M-D" form has a stable expected
// year in tests.
var refNow = time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)

// --- FormatDate table: ISO / US / EU ---

func TestFormatDate(t *testing.T) {
	tests := []struct {
		name string
		df   DateFormat
		want string
	}{
		{"ISO", DateISO, "2025-03-04"},
		{"US", DateUS, "03/04/2025"},
		{"EU", DateEU, "04/03/2025"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatDate(march4, tt.df)
			if got != tt.want {
				t.Fatalf("FormatDate(%v, %v) = %q, want %q", march4, tt.df, got, tt.want)
			}
		})
	}
}

// --- ParseDate table: each format parses its own rendering ---

func TestParseDate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		df   DateFormat
	}{
		{"ISO", "2025-03-04", DateISO},
		{"US", "03/04/2025", DateUS},
		{"EU", "04/03/2025", DateEU},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDate(tt.in, tt.df, refNow)
			if err != nil {
				t.Fatalf("ParseDate(%q, %v) error: %v", tt.in, tt.df, err)
			}
			if !got.Equal(march4) {
				t.Fatalf("ParseDate(%q, %v) = %v, want %v", tt.in, tt.df, got, march4)
			}
			// Date-only, UTC midnight.
			if got.Location() != time.UTC {
				t.Fatalf("ParseDate(%q) location = %v, want UTC", tt.in, got.Location())
			}
		})
	}
}

// ISO input is accepted regardless of the DateFormat setting (D16).
func TestParseDateISOAlwaysAccepted(t *testing.T) {
	for _, df := range []DateFormat{DateISO, DateUS, DateEU} {
		got, err := ParseDate("2025-03-04", df, refNow)
		if err != nil {
			t.Fatalf("ParseDate ISO under df=%v error: %v", df, err)
		}
		if !got.Equal(march4) {
			t.Fatalf("ParseDate ISO under df=%v = %v, want %v", df, got, march4)
		}
	}
}

// --- Rejection of malformed / impossible inputs ---

func TestParseDateRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
		df   DateFormat
	}{
		{"empty", "", DateUS},
		{"notADate", "not-a-date", DateISO},
		{"month13US", "13/40/2025", DateUS},
		{"day40EU", "13/40/2025", DateEU}, // 13 as a day, 40 as a month — both bad
		{"month13ISO", "2025-13-01", DateISO},
		{"day40ISO", "2025-01-40", DateISO},
		{"feb30ISO", "2025-02-30", DateISO},     // impossible day for the month
		{"feb29NonLeap", "2025-02-29", DateISO}, // non-leap year
		{"feb30US", "02/30/2025", DateUS},
		{"trailingGarbage", "2025-03-04x", DateISO},
		// Flexible-form rejections (p23.3): still bad even under lenient parsing.
		{"flexBadMonth", "26-13-1", DateISO},     // year 2026, month 13 -> reject
		{"flexFeb30", "2026-2-30", DateISO},      // impossible day for the month
		{"flexTooManyParts", "1/2/3/4", DateISO}, // 4 components is not a date
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDate(tt.in, tt.df, refNow)
			if err == nil {
				t.Fatalf("ParseDate(%q, %v) = %v, want error", tt.in, tt.df, got)
			}
			// Error message should be meaningful: mention the offending input.
			if tt.in != "" && !strings.Contains(err.Error(), tt.in) {
				t.Fatalf("ParseDate(%q) error %q does not mention the input", tt.in, err.Error())
			}
		})
	}
}

// TestParseDateFlexible: the p23.3 short/partial numeric forms. All parse
// big-endian (Y-M-D / M-D) regardless of df; a 2-part form takes refNow's year;
// a 2-digit year pivots (00–68 -> 2000s, 69–99 -> 1900s).
func TestParseDateFlexible(t *testing.T) {
	d := func(y, m, day int) time.Time { return time.Date(y, time.Month(m), day, 0, 0, 0, 0, time.UTC) }
	tests := []struct {
		name string
		in   string
		df   DateFormat
		want time.Time
	}{
		{"shortYMD", "26-6-1", DateISO, d(2026, 6, 1)},
		{"shortYMDzeroPad", "26-06-01", DateUS, d(2026, 6, 1)},
		{"fourDigitYearShortMD", "2026-6-1", DateEU, d(2026, 6, 1)},
		{"impliedYear", "6-1", DateISO, d(2026, 6, 1)}, // refNow year 2026
		{"slashBigEndian", "26/6/1", DateISO, d(2026, 6, 1)},
		{"slashImpliedYear", "6/1", DateEU, d(2026, 6, 1)},
		{"dotSeparators", "26.6.1", DateISO, d(2026, 6, 1)},
		{"pivotLow", "00-1-1", DateISO, d(2000, 1, 1)},
		{"pivot68", "68-1-1", DateISO, d(2068, 1, 1)},
		{"pivot69", "69-1-1", DateISO, d(1969, 1, 1)},
		{"pivot98", "98-12-31", DateISO, d(1998, 12, 31)},
		// A slash-separated full date is now accepted as big-endian even under df=ISO
		// (previously rejected; p23.3 makes entry forgiving — DECISIONS p23.3).
		{"slashFullUnderISO", "2025/03/04", DateISO, march4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDate(tt.in, tt.df, refNow)
			if err != nil {
				t.Fatalf("ParseDate(%q, %v) error: %v", tt.in, tt.df, err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("ParseDate(%q, %v) = %v, want %v", tt.in, tt.df, got, tt.want)
			}
			if got.Location() != time.UTC {
				t.Fatalf("ParseDate(%q) location = %v, want UTC", tt.in, got.Location())
			}
		})
	}
}
