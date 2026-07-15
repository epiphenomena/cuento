// Package money is the single entry point for exact monetary arithmetic and for
// all number rendering/parsing in cuento (AGENTS rule 10, D16). Stored amounts
// are always int64 minor units (D1); float64 appears only in exchange rates and
// in report-time conversion (AGENTS rule 3, D12) — here that is confined to
// ConvertMinor. This file owns the Amount type and the amount-display enums
// (NumberFormat, NegStyle, DisplayMode). Date formatting lives in a sibling file
// owned by p03.3 (DateFormat is defined there, not here).
package money

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// NumberFormat selects the grouping and decimal separators for rendering and
// parsing amounts. Kept a small enum by design (D16) — no CLDR.
type NumberFormat int

const (
	// NumberUS renders 1,234.56 — comma groups, dot decimal.
	NumberUS NumberFormat = iota
	// NumberEU renders 1.234,56 — dot groups, comma decimal.
	NumberEU
	// NumberPlain renders 1234.56 — no grouping, dot decimal. Useful for CSV
	// and machine-ish contexts where thousands separators get in the way.
	NumberPlain
)

// separators returns the grouping and decimal runes for the format.
func (nf NumberFormat) separators() (group, decimal rune, grouped bool) {
	switch nf {
	case NumberEU:
		return '.', ',', true
	case NumberPlain:
		return 0, '.', false
	default: // NumberUS
		return ',', '.', true
	}
}

// NegStyle selects how a negative amount is marked in Signed display mode.
type NegStyle int

const (
	// Minus renders negatives with a leading '-'.
	Minus NegStyle = iota
	// Parens renders negatives wrapped in parentheses, e.g. (1,234.56).
	Parens
)

// DisplayMode selects whether the sign is shown as +/- (net-debit, D2) or as a
// trailing DR/CR tag. The underlying stored amount is identical either way; this
// is purely a per-user display choice.
type DisplayMode int

const (
	// Signed shows the amount with its net-debit sign per NegStyle.
	Signed DisplayMode = iota
	// DebitCredit shows the magnitude with a trailing DR (positive = debit) or
	// CR (negative = credit) tag; NegStyle is irrelevant in this mode.
	DebitCredit
)

// FormatOpts bundles the three amount-display choices for Format.
type FormatOpts struct {
	Number  NumberFormat
	Neg     NegStyle
	Display DisplayMode
}

// Amount is a stored monetary value: an exact int64 count of the currency's
// minor units (D1) plus the ISO currency code. The exponent is NOT stored here —
// it is a property of the currency (currencies table) and is supplied by callers
// to Parse/Format/ConvertMinor. Amount is a value type; ops return new values.
type Amount struct {
	Minor    int64
	Currency string
}

// ErrCurrencyMismatch is returned when an operation combines two Amounts of
// different currencies. Callers branch on it via errors.Is (AGENTS style).
var ErrCurrencyMismatch = errors.New("money: currency mismatch")

// Add returns the sum of a and b, or ErrCurrencyMismatch if their currencies
// differ. Arithmetic is exact int64 (no float, per AGENTS rule 3).
func (a Amount) Add(b Amount) (Amount, error) {
	if a.Currency != b.Currency {
		return Amount{}, fmt.Errorf("add %s + %s: %w", a.Currency, b.Currency, ErrCurrencyMismatch)
	}
	return Amount{Minor: a.Minor + b.Minor, Currency: a.Currency}, nil
}

// Neg returns a with its sign flipped. Under net-debit (D2) this converts a
// debit to the equivalent credit and vice versa.
func (a Amount) Neg() Amount {
	return Amount{Minor: -a.Minor, Currency: a.Currency}
}

// pow10 returns 10^n for small non-negative n as int64. Used for exact
// minor<->major scaling; n is a currency exponent (0..4).
func pow10(n int) int64 {
	p := int64(1)
	for range n {
		p *= 10
	}
	return p
}

// Format renders minor units to a display string honoring all of FormatOpts.
// The value is unitless (no currency symbol) — this is the bare grouped-number
// renderer that pairs with Parse for round-tripping editable inputs. Display
// paths that want a per-currency symbol call FormatMoney instead (rule 10). In
// DebitCredit mode the sign is carried by a trailing DR/CR tag and the magnitude
// is always unsigned.
func Format(minor int64, exponent int, opts FormatOpts) string {
	return format(minor, exponent, opts, "")
}

// FormatMoney is the symbol-aware display renderer (AGENTS rule 10): it composes
// the per-currency symbol from currencySymbol onto the magnitude, then applies
// the sign per FormatOpts — so a negative USD reads "-$1,234.56" / "($1,234.56)"
// and a negative HNL "-1,234.56L" / "(1,234.56L)", with the symbol always hugging
// the digits and the sign wrapping the whole. It is display-only: FormatMoney's
// output is never fed back to Parse (editable inputs use the bare Format), so the
// symbol need not be re-parseable.
func FormatMoney(minor int64, currency string, exponent int, opts FormatOpts) string {
	sym, suffix := currencySymbol(currency)
	if suffix {
		return format(minor, exponent, opts, "\x00"+sym)
	}
	return format(minor, exponent, opts, sym+"\x00")
}

// format is the shared renderer. affix, when non-empty, carries the currency
// symbol with a single NUL byte marking where the magnitude goes: "$\x00" is a
// prefix symbol, "\x00L" a suffix symbol. The symbol composes onto the magnitude
// before the sign marker (minus/parens) or DR/CR tag is applied around the whole.
func format(minor int64, exponent int, opts FormatOpts, affix string) string {
	group, decimal, grouped := opts.Number.separators()

	// Work on the magnitude; the sign is applied by the display mode below.
	neg := minor < 0
	mag := minor
	if neg {
		mag = -mag
	}

	// Split into integer and fractional parts by the exponent scale.
	scale := pow10(exponent)
	intPart := mag / scale
	fracPart := mag % scale

	// Build the integer part with grouping (thousands) as configured.
	intStr := groupDigits(intPart, group, grouped)

	var b strings.Builder
	b.WriteString(intStr)
	if exponent > 0 {
		b.WriteRune(decimal)
		// Zero-pad the fractional part to the exponent width.
		fmt.Fprintf(&b, "%0*d", exponent, fracPart)
	}
	body := b.String()
	if affix != "" {
		// Splice the magnitude into the affix at its NUL placeholder so the
		// symbol hugs the digits (prefix or suffix) before any sign wrapping.
		body = strings.Replace(affix, "\x00", body, 1)
	}

	if opts.Display == DebitCredit {
		if neg {
			return body + " CR"
		}
		return body + " DR"
	}

	// Signed mode.
	if !neg {
		return body
	}
	if opts.Neg == Parens {
		return "(" + body + ")"
	}
	return "-" + body
}

// currencySymbol returns the display symbol for an ISO currency code and whether
// it is placed as a suffix (true) rather than a prefix. Symbols mirror the
// currencies-table seed (migrations 00003/00011); position is NOT a DB column
// (see DECISIONS "Currency symbol placement"), so it is owned here as a small
// deterministic map. USD/MXN/EUR prefix; HNL suffixes "L". Any unmapped currency
// falls back to its ISO code as a prefix (today's "CODE " convention, minus the
// space) — deterministic and crash-free for a currency added via the admin page.
func currencySymbol(code string) (sym string, suffix bool) {
	switch code {
	case "USD", "MXN":
		return "$", false
	case "EUR":
		return "€", false
	case "HNL":
		return "L", true
	default:
		return code, false
	}
}

// groupDigits renders a non-negative integer with optional thousands grouping.
func groupDigits(n int64, group rune, grouped bool) string {
	digits := fmt.Sprintf("%d", n)
	if !grouped || len(digits) <= 3 {
		return digits
	}
	// Insert the group separator every three digits from the right.
	var b strings.Builder
	lead := len(digits) % 3
	if lead > 0 {
		b.WriteString(digits[:lead])
	}
	for i := lead; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteRune(group)
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

// Parse converts a user-entered amount string into int64 minor units given the
// currency exponent and number format. It is deliberately liberal in the sign
// encodings it accepts — leading '-'/'+', surrounding parentheses, and a
// trailing DR/CR tag (net-debit: DR = +, CR = −) — because Format may emit any
// of them depending on NegStyle/DisplayMode; this is what makes
// parse(format(x)) == x hold across all display combinations. Malformed input
// (letters, too many fractional digits, conflicting signs) is rejected with a
// contextual error rather than silently coerced (money is exact, D1).
func Parse(s string, exponent int, nf NumberFormat) (int64, error) {
	raw := s
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("parse amount %q: empty", raw)
	}

	neg := false

	// Trailing DR/CR tag (case-insensitive). DR = debit (+), CR = credit (−).
	upper := strings.ToUpper(s)
	switch {
	case strings.HasSuffix(upper, "DR"):
		s = strings.TrimSpace(s[:len(s)-2])
	case strings.HasSuffix(upper, "CR"):
		neg = true
		s = strings.TrimSpace(s[:len(s)-2])
	}

	// Parentheses denote a negative. They must not co-exist with a sign char.
	if strings.HasPrefix(s, "(") || strings.HasSuffix(s, ")") {
		if !strings.HasPrefix(s, "(") || !strings.HasSuffix(s, ")") {
			return 0, fmt.Errorf("parse amount %q: unbalanced parentheses", raw)
		}
		neg = true
		s = s[1 : len(s)-1]
	}

	// Leading explicit sign.
	switch {
	case strings.HasPrefix(s, "-"):
		if neg { // e.g. "-(1.00)" or "(...)... DR/CR" already negated then '-'
			return 0, fmt.Errorf("parse amount %q: conflicting negative markers", raw)
		}
		neg = true
		s = s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}

	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("parse amount %q: no digits", raw)
	}

	group, decimal, grouped := nf.separators()

	// Strip grouping separators. We do not validate their placement — user input
	// is forgiving on grouping; correctness comes from the digits themselves.
	if grouped {
		s = strings.ReplaceAll(s, string(group), "")
	}

	// Split integer / fractional parts on the decimal rune.
	intPart, fracPart := s, ""
	if idx := strings.IndexRune(s, decimal); idx >= 0 {
		intPart = s[:idx]
		fracPart = s[idx+1:]
		if strings.ContainsRune(fracPart, decimal) {
			return 0, fmt.Errorf("parse amount %q: multiple decimal separators", raw)
		}
		if exponent == 0 {
			return 0, fmt.Errorf("parse amount %q: currency has no fractional digits", raw)
		}
	}

	if len(fracPart) > exponent {
		return 0, fmt.Errorf("parse amount %q: more than %d fractional digit(s)", raw, exponent)
	}

	// intPart may be empty for inputs like ".05"; treat as zero.
	intVal, err := parseDigits(intPart, true)
	if err != nil {
		return 0, fmt.Errorf("parse amount %q: %w", raw, err)
	}
	fracVal, err := parseDigits(fracPart, true)
	if err != nil {
		return 0, fmt.Errorf("parse amount %q: %w", raw, err)
	}
	if intPart == "" && fracPart == "" {
		return 0, fmt.Errorf("parse amount %q: no digits", raw)
	}

	// Right-pad the fractional part to the exponent width, then combine.
	scale := pow10(exponent)
	// fracVal currently represents fracPart as an integer of len(fracPart)
	// digits; shift it up to exponent width.
	fracVal *= pow10(exponent - len(fracPart))

	minor := intVal*scale + fracVal
	if neg {
		minor = -minor
	}
	return minor, nil
}

// parseDigits converts a run of ASCII digits to int64. An empty string yields 0
// when allowEmpty is set. Any non-digit rune is an error.
func parseDigits(s string, allowEmpty bool) (int64, error) {
	if s == "" {
		if allowEmpty {
			return 0, nil
		}
		return 0, errors.New("no digits")
	}
	var v int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid digit %q", string(r))
		}
		v = v*10 + int64(r-'0')
	}
	return v, nil
}

// ConvertMinor converts an amount expressed in fromExp minor units by an
// exchange rate into toExp minor units, rounding half-to-even at the final
// result (D12). This is the ONLY place ledger math touches float64 (AGENTS rule
// 3): the rate is float64 and the single scaled product is rounded once with
// math.RoundToEven, which is deterministic and symmetric about zero. No
// intermediate ledger sum is ever a float.
func ConvertMinor(minor int64, rate float64, fromExp, toExp int) int64 {
	// value_in_target_minor = minor * rate * 10^(toExp - fromExp).
	scaled := float64(minor) * rate * math.Pow(10, float64(toExp-fromExp))
	return int64(math.RoundToEven(scaled))
}
