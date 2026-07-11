package money

import (
	"testing"
	"time"
)

// A discriminating date: US (MM/DD/YYYY) and EU (DD/MM/YYYY) renderings differ
// visibly, so a swapped layout fails loudly.
var march4 = time.Date(2025, 3, 4, 0, 0, 0, 0, time.UTC)

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
			got, err := ParseDate(tt.in, tt.df)
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
		got, err := ParseDate("2025-03-04", df)
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
		{"wrongSeparators", "2025/03/04", DateISO}, // ISO wants dashes
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDate(tt.in, tt.df)
			if err == nil {
				t.Fatalf("ParseDate(%q, %v) = %v, want error", tt.in, tt.df, got)
			}
			// Error message should be meaningful: mention the offending input.
			if !containsInput(err.Error(), tt.in) && tt.in != "" {
				t.Fatalf("ParseDate(%q) error %q does not mention the input", tt.in, err.Error())
			}
		})
	}
}

func containsInput(msg, in string) bool {
	return in != "" && stringsContains(msg, in)
}

func stringsContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
