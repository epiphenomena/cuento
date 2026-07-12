package web

import (
	"cuento/internal/money"
	"cuento/internal/store"
)

// p11.1 begins rendering money in the UI, so the web layer needs to turn a user's
// stored settings (the raw column strings on store.CurrentUser: number_format,
// neg_style, display_mode, date_format) into the money.FormatOpts / money.DateFormat
// the p03 formatters consume (rule 10: all number/date rendering goes through the
// money formatters honoring per-user settings). This is the single mapping; p13.1
// (the settings UI) reuses it. Unknown/empty values fall through to the money
// package's zero-value defaults (US number, minus, signed, ISO date) -- which are
// also the DB column defaults -- so an untouched session renders sensibly.

// formatOptsFor builds the money.FormatOpts for a user's amount-display settings.
// A nil user (anonymous -- no money page is anonymous, but keep it total) uses the
// defaults.
func formatOptsFor(u *store.CurrentUser) money.FormatOpts {
	if u == nil {
		return money.FormatOpts{}
	}
	return money.FormatOpts{
		Number:  numberFormatOf(u.NumberFormat),
		Neg:     negStyleOf(u.NegStyle),
		Display: displayModeOf(u.DisplayMode),
	}
}

// dateFormatFor maps a user's date_format setting to money.DateFormat.
func dateFormatFor(u *store.CurrentUser) money.DateFormat {
	if u == nil {
		return money.DateISO
	}
	switch u.DateFormat {
	case "US":
		return money.DateUS
	case "EU":
		return money.DateEU
	default: // "ISO" or unknown
		return money.DateISO
	}
}

func numberFormatOf(s string) money.NumberFormat {
	switch s {
	case "EU":
		return money.NumberEU
	case "plain":
		return money.NumberPlain
	default: // "US" or unknown
		return money.NumberUS
	}
}

func negStyleOf(s string) money.NegStyle {
	if s == "parens" {
		return money.Parens
	}
	return money.Minus // "minus" or unknown
}

func displayModeOf(s string) money.DisplayMode {
	if s == "dr_cr" {
		return money.DebitCredit
	}
	return money.Signed // "signed" or unknown
}
