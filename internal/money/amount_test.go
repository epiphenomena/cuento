package money

import (
	"errors"
	"math"
	"math/rand"
	"testing"
)

// --- Format tables: all NumberFormats × NegStyles × DisplayModes ---

func TestFormatNumberFormats(t *testing.T) {
	// exponent 2, positive value 123456 minor = 1234.56 major.
	tests := []struct {
		name string
		nf   NumberFormat
		want string
	}{
		{"US", NumberUS, "1,234.56"},
		{"EU", NumberEU, "1.234,56"},
		{"Plain", NumberPlain, "1234.56"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Format(123456, 2, FormatOpts{Number: tt.nf, Neg: Minus, Display: Signed})
			if got != tt.want {
				t.Fatalf("Format = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatNegStyles(t *testing.T) {
	tests := []struct {
		name string
		neg  NegStyle
		want string
	}{
		{"Minus", Minus, "-1,234.56"},
		{"Parens", Parens, "(1,234.56)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Format(-123456, 2, FormatOpts{Number: NumberUS, Neg: tt.neg, Display: Signed})
			if got != tt.want {
				t.Fatalf("Format = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatDisplayModes(t *testing.T) {
	// Net-debit (D2): positive = debit, negative = credit.
	tests := []struct {
		name    string
		minor   int64
		display DisplayMode
		neg     NegStyle
		want    string
	}{
		{"SignedPos", 123456, Signed, Minus, "1,234.56"},
		{"SignedNeg", -123456, Signed, Minus, "-1,234.56"},
		{"SignedNegParens", -123456, Signed, Parens, "(1,234.56)"},
		{"DebitCreditPos", 123456, DebitCredit, Minus, "1,234.56 DR"},
		{"DebitCreditNeg", -123456, DebitCredit, Minus, "1,234.56 CR"},
		// In DR/CR mode the sign is carried by the DR/CR tag, so NegStyle is
		// irrelevant: the magnitude is always rendered unsigned.
		{"DebitCreditNegParens", -123456, DebitCredit, Parens, "1,234.56 CR"},
		{"DebitCreditZero", 0, DebitCredit, Minus, "0.00 DR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Format(tt.minor, 2, FormatOpts{Number: NumberUS, Neg: tt.neg, Display: tt.display})
			if got != tt.want {
				t.Fatalf("Format = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatExponentZero(t *testing.T) {
	// JPY-style: no fractional part, no decimal separator.
	got := Format(1234567, 0, FormatOpts{Number: NumberUS, Neg: Minus, Display: Signed})
	if want := "1,234,567"; got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
	got = Format(1234567, 0, FormatOpts{Number: NumberEU, Neg: Minus, Display: Signed})
	if want := "1.234.567"; got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
}

func TestFormatSmallMagnitude(t *testing.T) {
	// Fractional-only values must be zero-padded to the exponent width.
	got := Format(5, 2, FormatOpts{Number: NumberUS, Neg: Minus, Display: Signed})
	if want := "0.05"; got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
	// exponent 3
	got = Format(7, 3, FormatOpts{Number: NumberUS, Neg: Minus, Display: Signed})
	if want := "0.007"; got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
}

// --- Parse tables ---

func TestParseNumberFormats(t *testing.T) {
	tests := []struct {
		name string
		in   string
		exp  int
		nf   NumberFormat
		want int64
	}{
		{"US", "1,234.56", 2, NumberUS, 123456},
		{"EU", "1.234,56", 2, NumberEU, 123456},
		{"Plain", "1234.56", 2, NumberPlain, 123456},
		{"USNoGroup", "1234.56", 2, NumberUS, 123456},
		{"ExpZero", "1,234", 0, NumberUS, 1234},
		{"FractionalOnly", "0.05", 2, NumberUS, 5},
		{"NoDecimalPart", "12", 2, NumberUS, 1200},
		{"ShortFraction", "1.5", 2, NumberUS, 150},
		{"Zero", "0.00", 2, NumberUS, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.in, tt.exp, tt.nf)
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("Parse(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseSignEncodings(t *testing.T) {
	// Parse is liberal in the sign encodings it accepts (minus, parens, DR/CR),
	// because Format may emit any of them depending on NegStyle/DisplayMode.
	tests := []struct {
		name string
		in   string
		want int64
	}{
		{"Minus", "-1,234.56", -123456},
		{"Parens", "(1,234.56)", -123456},
		{"DR", "1,234.56 DR", 123456},
		{"CR", "1,234.56 CR", -123456},
		{"DRnoSpace", "1,234.56DR", 123456},
		{"Plus", "+1,234.56", 123456},
		{"ZeroDR", "0.00 DR", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.in, 2, NumberUS)
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("Parse(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	tests := []struct {
		name string
		in   string
		exp  int
		nf   NumberFormat
	}{
		{"empty", "", 2, NumberUS},
		{"letters", "abc", 2, NumberUS},
		{"tooManyFractionDigits", "1.234", 2, NumberUS},
		{"twoDecimalPoints", "1.2.3", 2, NumberUS},
		{"loneSign", "-", 2, NumberUS},
		{"unbalancedParen", "(1.00", 2, NumberUS},
		{"decimalOnExpZero", "1.5", 0, NumberUS},
		{"bothSignAndParens", "-(1.00)", 2, NumberUS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse(tt.in, tt.exp, tt.nf); err == nil {
				t.Fatalf("Parse(%q) = nil error, want error", tt.in)
			}
		})
	}
}

// --- Property test: parse(format(x)) == x over random minors and formats ---

func TestParseFormatRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(0xC0FFEE)) // deterministic; no network (AGENTS rule).
	nfs := []NumberFormat{NumberUS, NumberEU, NumberPlain}
	negs := []NegStyle{Minus, Parens}
	displays := []DisplayMode{Signed, DebitCredit}
	for i := 0; i < 20000; i++ {
		minor := r.Int63n(2_000_000_000) - 1_000_000_000 // includes negatives and zero
		exp := r.Intn(5)                                 // 0..4 per currencies CHECK
		opts := FormatOpts{
			Number:  nfs[r.Intn(len(nfs))],
			Neg:     negs[r.Intn(len(negs))],
			Display: displays[r.Intn(len(displays))],
		}
		s := Format(minor, exp, opts)
		got, err := Parse(s, exp, opts.Number)
		if err != nil {
			t.Fatalf("Parse(Format(%d, exp=%d, %+v)=%q) error: %v", minor, exp, opts, s, err)
		}
		if got != minor {
			t.Fatalf("round-trip: Format(%d, exp=%d, %+v)=%q parsed back to %d", minor, exp, opts, s, got)
		}
	}
}

// --- Amount ops ---

func TestAddCurrencyMismatch(t *testing.T) {
	a := Amount{Minor: 100, Currency: "USD"}
	b := Amount{Minor: 200, Currency: "MXN"}
	if _, err := a.Add(b); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Add mismatch err = %v, want ErrCurrencyMismatch", err)
	}
	// Same currency adds exactly.
	c := Amount{Minor: 200, Currency: "USD"}
	sum, err := a.Add(c)
	if err != nil {
		t.Fatalf("Add same currency error: %v", err)
	}
	if sum != (Amount{Minor: 300, Currency: "USD"}) {
		t.Fatalf("Add = %+v, want {300 USD}", sum)
	}
}

func TestNeg(t *testing.T) {
	a := Amount{Minor: 123, Currency: "USD"}
	if got := a.Neg(); got != (Amount{Minor: -123, Currency: "USD"}) {
		t.Fatalf("Neg = %+v", got)
	}
}

// --- ConvertMinor: half-even rounding at final result ---

func TestConvertRoundsHalfEven(t *testing.T) {
	tests := []struct {
		name    string
		minor   int64
		rate    float64
		fromExp int
		toExp   int
		want    int64
	}{
		// rate 0.5, same exponent: products land exactly on .5 boundaries.
		{"1x0.5=0.5->0", 1, 0.5, 2, 2, 0},      // ties to even (0)
		{"3x0.5=1.5->2", 3, 0.5, 2, 2, 2},      // ties to even (2), rounds up
		{"5x0.5=2.5->2", 5, 0.5, 2, 2, 2},      // ties to even (2), rounds down
		{"7x0.5=3.5->4", 7, 0.5, 2, 2, 4},      // ties to even (4)
		{"-3x0.5=-1.5->-2", -3, 0.5, 2, 2, -2}, // symmetric negative tie
		{"-5x0.5=-2.5->-2", -5, 0.5, 2, 2, -2}, // symmetric negative tie
		// exact, no rounding
		{"200x1.5=300", 200, 1.5, 2, 2, 300},
		// exponent change: 1 USD-cent (exp2) -> exp0 units at rate 1.0 = 0.01 -> 0
		{"expDown", 1, 1.0, 2, 0, 0},
		// exponent change up: 1 unit exp0 -> exp2 at rate 1.0 = 100
		{"expUp", 1, 1.0, 0, 2, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConvertMinor(tt.minor, tt.rate, tt.fromExp, tt.toExp)
			if got != tt.want {
				t.Fatalf("ConvertMinor(%d, %v, %d, %d) = %d, want %d",
					tt.minor, tt.rate, tt.fromExp, tt.toExp, got, tt.want)
			}
		})
	}
}

// Guard: our tie expectations really are exact ties at the float level, so the
// test proves half-even behavior rather than accidental nearest-rounding.
func TestConvertTieInputsAreExact(t *testing.T) {
	for _, minor := range []int64{1, 3, 5, 7} {
		scaled := float64(minor) * 0.5
		if scaled != math.Trunc(scaled)+0.5 {
			t.Fatalf("tie input %d*0.5=%v is not an exact .5 tie", minor, scaled)
		}
	}
}
