package reports

import (
	"encoding/csv"
	"fmt"
	"io"

	"cuento/internal/money"
)

// WriteCSV renders a Table as CSV to w using the stdlib encoding/csv writer, which
// quotes and escapes any field containing a comma, double-quote, or newline per RFC
// 4180 — so a text cell holding `a,b"c\nd` round-trips through a csv.Reader intact
// (the framework's escaping-correctness guarantee). The value rows are MACHINE-PLAIN,
// not display-formatted: money is emitted as its exact major-unit decimal in
// NumberPlain form (no grouping separators to fight the delimiter) followed by the
// currency code in its own column-adjacent value; dates as their ISO string; text
// verbatim. Column headers are written as a header row using localize(headerKey) —
// the caller passes a localizer (the request-language {{t}}) so the CSV headers read
// in the user's language while the framework stays i18n-agnostic. exps maps a
// currency code to its minor-unit exponent (for money formatting); an unknown
// currency falls back to exponent 0 (whole units), which is safe for a machine round
// trip. Indent, subtotal/total, and warning-row KIND are not encoded as styling in
// CSV — the data cells already carry the values — but a warning row's cells are
// emitted like any other so the D19 net is never dropped from the export.
func WriteCSV(w io.Writer, t Table, localize func(key string) string, exps map[string]int) error {
	cw := csv.NewWriter(w)

	// Header row: localized column headers.
	header := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		header[i] = columnHeader(c, localize)
	}
	if err := cw.Write(header); err != nil {
		return fmt.Errorf("reports: write csv header: %w", err)
	}

	for _, row := range t.Rows {
		rec := make([]string, len(row.Cells))
		for i, cell := range row.Cells {
			rec[i] = csvCell(cell, exps)
		}
		if err := cw.Write(rec); err != nil {
			return fmt.Errorf("reports: write csv row: %w", err)
		}
	}

	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("reports: flush csv: %w", err)
	}
	return nil
}

// csvCell renders one cell to its machine-plain CSV string. Money uses NumberPlain
// (no grouping) so the value is a clean decimal; a Blank money cell is the empty
// string. Text/date cells emit their raw string (csv.Writer handles all escaping).
func csvCell(c Cell, exps map[string]int) string {
	switch c.Kind {
	case CellMoney:
		if c.Blank {
			return ""
		}
		exp := exps[c.Currency] // 0 for an unknown currency (whole units)
		// NumberPlain + Minus + Signed: a bare, unambiguous decimal for machines.
		return money.Format(c.Minor, exp, money.FormatOpts{
			Number:  money.NumberPlain,
			Neg:     money.Minus,
			Display: money.Signed,
		})
	case CellMeasures:
		// p30.9: all three measures, ';'-separated (not a comma, so csv never has to
		// quote), machine-recoverable in fixed budgeted;actual;variance order.
		exp := exps[c.Currency]
		opts := money.FormatOpts{Number: money.NumberPlain, Neg: money.Minus, Display: money.Signed}
		f := func(m int64) string { return money.Format(m, exp, opts) }
		return f(c.Budgeted) + ";" + f(c.Actual) + ";" + f(c.Variance)
	default: // CellText, CellDate
		return c.Text
	}
}
