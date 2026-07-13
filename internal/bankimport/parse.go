// Package bankimport is the PURE, table-tested CSV parser for bank-statement
// imports (p17.2). Given raw CSV bytes plus a mapping Config, it produces a slice
// of ParsedRow{Date, AmountMinor, Payee, Memo, Raw} -- one per data row -- with a
// per-row error (never a panic) on a row that does not parse. It touches NO
// database and no document; the store (p17.2) stages the rows and computes the
// dedupe hash. Amounts land as int64 minor units (rule 3) via money.Parse; dates
// via money.ParseDate. This package deliberately owns no I/O beyond reading the
// provided byte slice.
package bankimport

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"strings"

	"cuento/internal/money"
)

// Delimiter selects the field separator. Auto sniffs comma/semicolon/tab from the
// first line; the explicit values pin it (a re-used profile records the sniffed
// choice so a later file with an ambiguous header still parses the same way).
type Delimiter string

const (
	// DelimiterAuto sniffs the delimiter from the header/first line.
	DelimiterAuto Delimiter = ""
	// DelimiterComma pins ','.
	DelimiterComma Delimiter = ","
	// DelimiterSemicolon pins ';'.
	DelimiterSemicolon Delimiter = ";"
	// DelimiterTab pins '\t'.
	DelimiterTab Delimiter = "\t"
)

// AmountMode selects how the amount is read from the row.
type AmountMode string

const (
	// AmountSingle: one signed amount column (AmountCol). A leading '-' or
	// parentheses denote a negative; SignFlip inverts the result.
	AmountSingle AmountMode = "single"
	// AmountDebitCredit: a debit column (DebitCol) and a credit column (CreditCol),
	// exactly one non-blank per row. The debit is added as-is, the credit is
	// subtracted, giving a net-debit signed amount (D2); SignFlip inverts the whole.
	AmountDebitCredit AmountMode = "debit_credit"
)

// DateLayout selects the date parse layout. It maps onto money.DateFormat so the
// app's single date parser (rule 10) is reused; ISO is always additionally
// accepted by money.ParseDate, so a config picking US/EU still tolerates an ISO
// cell.
type DateLayout string

const (
	// DateISO parses YYYY-MM-DD.
	DateISO DateLayout = "ISO"
	// DateUS parses MM/DD/YYYY.
	DateUS DateLayout = "US"
	// DateEU parses DD/MM/YYYY.
	DateEU DateLayout = "EU"
)

// Config is the column mapping for one CSV layout -- the decoded form of a
// mapping_profiles.config JSON blob (p17.2). Column references are ZERO-BASED
// indices into the row. A negative index means "unmapped" (e.g. no memo column).
type Config struct {
	Delimiter Delimiter  `json:"delimiter"`
	HasHeader bool       `json:"has_header"`
	Amount    AmountMode `json:"amount_mode"`
	SignFlip  bool       `json:"sign_flip"`
	DateFmt   DateLayout `json:"date_format"`

	DateCol   int `json:"date_col"`
	AmountCol int `json:"amount_col"` // AmountSingle
	DebitCol  int `json:"debit_col"`  // AmountDebitCredit
	CreditCol int `json:"credit_col"` // AmountDebitCredit
	PayeeCol  int `json:"payee_col"`
	MemoCol   int `json:"memo_col"` // negative == unmapped
}

// ParsedRow is one parsed data row. Raw holds the original cells (so the staging
// layer can persist raw_json). On a per-row failure Err is set and the parsed
// fields are zero -- the caller decides whether to stage or reject (p17.2 rejects
// the whole batch on any Err; the parser itself never aborts the file).
type ParsedRow struct {
	Date        string // YYYY-MM-DD (money's canonical), empty on error
	AmountMinor int64  // net-debit signed minor units (D2)
	Payee       string
	Memo        string
	Raw         []string
	Err         error
}

// ErrNoRows is returned by Parse when the input has no data rows at all (an empty
// file, or a header with nothing under it). It is a whole-file error, distinct
// from a per-row ParsedRow.Err.
var ErrNoRows = errors.New("bankimport: no data rows")

// Parse reads raw CSV bytes under cfg and returns one ParsedRow per data row. A
// malformed row (bad date, unparseable amount, too few columns) yields a ParsedRow
// with Err set rather than aborting the file or panicking. A file-level problem
// (undetectable delimiter, unreadable CSV structure, zero data rows) returns a
// non-nil error and no rows. exponent is the target account currency's minor-unit
// exponent (money.Parse needs it).
func Parse(raw []byte, cfg Config, exponent int) ([]ParsedRow, error) {
	delim, err := resolveDelimiter(raw, cfg.Delimiter)
	if err != nil {
		return nil, err
	}

	cr := csv.NewReader(bytes.NewReader(raw))
	cr.Comma = delim
	cr.FieldsPerRecord = -1 // ragged rows are a per-row error, not a file abort
	cr.TrimLeadingSpace = true

	records, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("bankimport: read csv: %w", err)
	}
	if cfg.HasHeader && len(records) > 0 {
		records = records[1:]
	}
	if len(records) == 0 {
		return nil, ErrNoRows
	}

	out := make([]ParsedRow, 0, len(records))
	for _, rec := range records {
		out = append(out, parseRow(rec, cfg, exponent))
	}
	return out, nil
}

// parseRow parses a single record. It never panics: an out-of-range column index
// or a bad cell yields a ParsedRow with Err set. Raw is always the original cells.
func parseRow(rec []string, cfg Config, exponent int) ParsedRow {
	row := ParsedRow{Raw: rec}

	dateCell, err := cell(rec, cfg.DateCol)
	if err != nil {
		row.Err = fmt.Errorf("date column: %w", err)
		return row
	}
	date, err := parseDate(dateCell, cfg.DateFmt)
	if err != nil {
		row.Err = err
		return row
	}

	amount, err := parseAmount(rec, cfg, exponent)
	if err != nil {
		row.Err = err
		return row
	}

	row.Date = date
	row.AmountMinor = amount
	row.Payee = optionalCell(rec, cfg.PayeeCol)
	row.Memo = optionalCell(rec, cfg.MemoCol)
	return row
}

// parseAmount reads the amount per the mode. In single mode it is one signed
// column; in debit/credit mode exactly one of the two columns is non-blank (a
// debit adds, a credit subtracts), yielding a net-debit signed value. SignFlip
// inverts the final result (banks disagree on whether a debit is negative).
func parseAmount(rec []string, cfg Config, exponent int) (int64, error) {
	var minor int64

	switch cfg.Amount {
	case AmountDebitCredit:
		debit, derr := cell(rec, cfg.DebitCol)
		if derr != nil {
			return 0, fmt.Errorf("debit column: %w", derr)
		}
		credit, cerr := cell(rec, cfg.CreditCol)
		if cerr != nil {
			return 0, fmt.Errorf("credit column: %w", cerr)
		}
		dblank := isBlankAmount(debit)
		cblank := isBlankAmount(credit)
		switch {
		case dblank && cblank:
			return 0, errors.New("amount: debit and credit are both blank")
		case !dblank && !cblank:
			return 0, errors.New("amount: debit and credit are both filled")
		case !dblank:
			v, err := parseMoney(debit, exponent)
			if err != nil {
				return 0, err
			}
			minor = v
		default:
			v, err := parseMoney(credit, exponent)
			if err != nil {
				return 0, err
			}
			minor = -v
		}
	default: // AmountSingle
		amtCell, err := cell(rec, cfg.AmountCol)
		if err != nil {
			return 0, fmt.Errorf("amount column: %w", err)
		}
		if isBlankAmount(amtCell) {
			return 0, errors.New("amount: blank")
		}
		v, err := parseMoney(amtCell, exponent)
		if err != nil {
			return 0, err
		}
		minor = v
	}

	if cfg.SignFlip {
		minor = -minor
	}
	return minor, nil
}

// parseMoney strips currency symbols/whitespace a bank may embed, then defers to
// money.Parse (which handles grouping, parentheses, and DR/CR but NOT symbols).
// Bank exports use the plain number format (no locale grouping ambiguity); a
// magnitude with grouping still parses because money.NumberPlain leaves any commas
// -- so we strip a leading currency symbol and surrounding spaces and pass the
// rest through. Grouping separators are removed here for robustness.
func parseMoney(s string, exponent int) (int64, error) {
	cleaned := stripCurrency(s)
	minor, err := money.Parse(cleaned, exponent, money.NumberPlain)
	if err != nil {
		return 0, fmt.Errorf("amount %q: %w", s, err)
	}
	return minor, nil
}

// stripCurrency removes leading/trailing whitespace, common currency symbols, and
// thousands-grouping commas so money.Parse (NumberPlain) sees just sign markers
// and digits with a dot decimal. Parentheses are preserved (money.Parse reads them
// as negative). A trailing/leading '$', 'L' (lempira), or non-breaking space is
// dropped.
func stripCurrency(s string) string {
	s = strings.TrimSpace(s)
	// Drop grouping commas (bank plain exports occasionally group thousands).
	s = strings.ReplaceAll(s, ",", "")
	// Drop currency symbols anywhere they appear.
	for _, sym := range []string{"$", " ", "USD", "MXN", "HNL", "L", "₡"} {
		s = strings.ReplaceAll(s, sym, "")
	}
	return strings.TrimSpace(s)
}

// isBlankAmount reports whether a cell is effectively empty for amount purposes.
func isBlankAmount(s string) bool { return strings.TrimSpace(s) == "" }

// parseDate parses a date cell under the layout, reusing money.ParseDate (rule 10)
// and re-formatting to the canonical YYYY-MM-DD the ledger stores. money.ParseDate
// always accepts ISO too, so a US/EU config still tolerates an ISO cell.
func parseDate(s string, layout DateLayout) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("date: blank")
	}
	t, err := money.ParseDate(s, dateFormatOf(layout))
	if err != nil {
		return "", fmt.Errorf("date %q: %w", s, err)
	}
	return money.FormatDate(t, money.DateISO), nil
}

// dateFormatOf maps the config's DateLayout to money.DateFormat.
func dateFormatOf(l DateLayout) money.DateFormat {
	switch l {
	case DateUS:
		return money.DateUS
	case DateEU:
		return money.DateEU
	default: // DateISO or unset
		return money.DateISO
	}
}

// cell returns the trimmed value at index i, or an error if i is out of range or
// negative (a required column must map to a real column).
func cell(rec []string, i int) (string, error) {
	if i < 0 || i >= len(rec) {
		return "", fmt.Errorf("column %d out of range (row has %d columns)", i, len(rec))
	}
	return strings.TrimSpace(rec[i]), nil
}

// optionalCell returns the trimmed value at index i, or "" if i is unmapped
// (negative) or out of range -- optional fields (payee, memo) never fail a row.
func optionalCell(rec []string, i int) string {
	if i < 0 || i >= len(rec) {
		return ""
	}
	return strings.TrimSpace(rec[i])
}

// resolveDelimiter returns the rune to split on. An explicit config value is
// honored; DelimiterAuto sniffs the first non-empty line, choosing the candidate
// (comma, semicolon, tab) with the highest count, ties broken comma>semicolon>tab.
// A line with none of them is ambiguous only when it also has no obvious single
// field -- we default to comma (a one-column file is valid under comma).
func resolveDelimiter(raw []byte, d Delimiter) (rune, error) {
	switch d {
	case DelimiterComma:
		return ',', nil
	case DelimiterSemicolon:
		return ';', nil
	case DelimiterTab:
		return '\t', nil
	}
	// Auto: sniff the first non-empty line.
	line := firstLine(raw)
	if line == "" {
		return 0, ErrNoRows
	}
	best := ','
	bestN := strings.Count(line, ",")
	if n := strings.Count(line, ";"); n > bestN {
		best, bestN = ';', n
	}
	if n := strings.Count(line, "\t"); n > bestN {
		best = '\t'
	}
	return best, nil
}

// firstLine returns the first non-empty, trimmed line of raw.
func firstLine(raw []byte) string {
	for _, ln := range strings.Split(string(raw), "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}
