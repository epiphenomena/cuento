package reports

import (
	"strings"

	"cuento/internal/money"
)

// golden.go is the Phase-15 GOLDEN-FILE test harness (introduced by p15.3, reused by
// p15.4–p15.11). A golden is a committed, human-REVIEWABLE textual rendering of a
// report over the canonical synthetic fixture at FIXED params — the reviewer reads it
// to confirm the numbers, and a diff in CI flags any behavior change. Two artifacts
// per report live under internal/reports/testdata/<id>.{txt,csv}:
//
//   - <id>.txt — an aligned, fixed-width column dump (this file's DumpTable): the
//     readable artifact a human scans (native + converted side by side).
//   - <id>.csv — the machine export (reports.WriteCSV), proving the CSV renderer and
//     giving a delimiter-plain second view.
//
// Both are DETERMINISTIC: the golden test pins lang=en, a fixed as-of, root scope, and
// USD target, and formats money with FIXED opts (money.NumberUS / Signed, NOT
// per-user settings) so there is no clock/locale drift. `make golden` (which runs the
// golden test with -update) regenerates them; the diff is reviewed, never blind-
// committed. Because every value derives from the synthetic Appendix-D fixture, the
// goldens are safe to commit (DATA RULE 11).

// GoldenMoneyOpts is the FIXED money-format used when dumping a golden's aligned text
// table: grouped thousands, ASCII minus, always-signed. It is deliberately NOT a
// per-user FormatOpts — a golden must render identically on every machine and locale,
// so the format is pinned here rather than read from a user setting (rule 10 governs
// the LIVE render path; a golden is a test artifact).
var GoldenMoneyOpts = money.FormatOpts{
	Number:  money.NumberUS,
	Neg:     money.Minus,
	Display: money.Signed,
}

// DumpTable renders a Table to a stable, aligned, fixed-width text block for the
// golden .txt artifact: a header row, a rule, then one line per row. LABEL cells are
// localized via localize (the golden pins lang=en); TEXT cells render verbatim; MONEY
// cells use GoldenMoneyOpts with the currency code prefixed and the per-currency symbol (so native vs converted is
// unambiguous even in the text dump); a Blank money cell is empty. Subtotal/total/
// warning rows carry a leading marker column so the reviewer sees the row kind. Column
// widths are computed from the content so columns align regardless of value length.
// exps maps currency -> minor-unit exponent for money formatting.
func DumpTable(t Table, localize func(key string) string, exps map[string]int) string {
	// Build the cell text grid: a leading KIND marker column, then one column per
	// table column. Header row first.
	header := make([]string, 0, len(t.Columns)+1)
	header = append(header, "") // kind-marker column header is blank
	for _, c := range t.Columns {
		header = append(header, columnHeader(c, localize))
	}

	grid := [][]string{header}
	aligns := make([]Align, len(t.Columns)+1)
	aligns[0] = AlignLeft
	for i, c := range t.Columns {
		aligns[i+1] = c.Align
	}

	for _, row := range t.Rows {
		line := make([]string, 0, len(t.Columns)+1)
		line = append(line, kindMarker(row.Kind))
		for _, cell := range row.Cells {
			line = append(line, dumpCell(cell, localize, exps))
		}
		// Pad short rows so every grid line has the same column count.
		for len(line) < len(t.Columns)+1 {
			line = append(line, "")
		}
		grid = append(grid, line)
	}

	// Column widths: the max cell width in each column.
	widths := make([]int, len(t.Columns)+1)
	for _, line := range grid {
		for i, s := range line {
			if w := len([]rune(s)); w > widths[i] {
				widths[i] = w
			}
		}
	}

	var b strings.Builder
	writeRow := func(cells []string) {
		parts := make([]string, len(cells))
		for i, s := range cells {
			parts[i] = pad(s, widths[i], aligns[i])
		}
		b.WriteString(strings.TrimRight(strings.Join(parts, "  "), " "))
		b.WriteByte('\n')
	}

	writeRow(grid[0])
	// A rule under the header.
	rule := make([]string, len(widths))
	for i, w := range widths {
		rule[i] = strings.Repeat("-", w)
	}
	writeRow(rule)
	for _, line := range grid[1:] {
		writeRow(line)
	}
	return b.String()
}

// columnHeader returns a column's golden/CSV header text: its VERBATIM proper-noun
// HeaderText when set (a program name, rule 9), else its localized HeaderKey label.
// The golden/CSV renderers emit the flat LEAF header row only — a stacked-header
// group (Column.Group) is a web-render concern and is ignored here, so a matrix
// report's text/CSV dump stays a plain one-header-row table.
func columnHeader(c Column, localize func(key string) string) string {
	if c.HeaderText != "" {
		return c.HeaderText
	}
	return localize(c.HeaderKey)
}

// kindMarker returns the leading marker for a row kind in the text dump: blank for a
// data row, ">" for a placeholder-parent subtotal, "#" for a SECTION total (p30.10;
// the middle tier — "Total revenue"/"Total assets"), "=" for a grand total, "!" for a
// D19 warning — so a reviewer reads the row kinds (and the three total tiers) at a
// glance and a golden diff shows a kind change. Every marker is a SINGLE ASCII char so
// the leading column stays one wide and money amounts never shift.
func kindMarker(k RowKind) string {
	switch k {
	case RowSubtotal:
		return ">"
	case RowSectionTotal:
		return "#"
	case RowTotal:
		return "="
	case RowWarning:
		return "!"
	default:
		return ""
	}
}

// dumpCell renders one cell to its golden-text string.
func dumpCell(c Cell, localize func(key string) string, exps map[string]int) string {
	switch c.Kind {
	case CellMoney:
		if c.Blank {
			return ""
		}
		// Keep the ISO code prefix (native vs converted disambiguation) AND add
		// the per-currency symbol via FormatMoney, so the golden shows every cell
		// gaining its symbol while USD/MXN (both "$") stay distinguishable.
		return c.Currency + " " + money.FormatMoney(c.Minor, c.Currency, exps[c.Currency], GoldenMoneyOpts)
	case CellMeasures:
		// p30.9: all three measures compound (B / A / V), currency-prefixed once, so the
		// golden stays reconcilable (a reviewer confirms V = A − B and the totals sum).
		exp := exps[c.Currency]
		f := func(m int64) string { return money.FormatMoney(m, c.Currency, exp, GoldenMoneyOpts) }
		return c.Currency + " " + f(c.Budgeted) + " / " + f(c.Actual) + " / " + f(c.Variance)
	case CellLabel:
		return localize(c.Text)
	default: // CellText, CellDate
		return c.Text
	}
}

// pad left/right-justifies s to width w per align (money columns right-align so the
// decimal points line up in the text dump).
func pad(s string, w int, a Align) string {
	n := w - len([]rune(s))
	if n <= 0 {
		return s
	}
	if a == AlignRight {
		return strings.Repeat(" ", n) + s
	}
	return s + strings.Repeat(" ", n)
}

// SumMoneyColumn sums the non-blank money cells in column col across every DATA row of
// t (skipping subtotal/total/warning rows so it re-derives the total independently of
// the report's own total rows), returning per-currency minor-unit totals. The golden
// test uses it to re-sum the report's OWN emitted cells and assert the trial balance
// balances (each native currency nets to zero) without trusting the report's total
// rows.
func SumMoneyColumn(t Table, col int) map[string]int64 {
	out := make(map[string]int64)
	for _, row := range t.Rows {
		if row.Kind != RowData {
			continue
		}
		if col >= len(row.Cells) {
			continue
		}
		c := row.Cells[col]
		if c.Kind == CellMoney && !c.Blank {
			out[c.Currency] += c.Minor
		}
	}
	return out
}
