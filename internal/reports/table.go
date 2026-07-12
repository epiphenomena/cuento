package reports

// Table is a report's rendered output as typed rows: the framework's single
// output shape, rendered by the generic HTML template (web) and the CSV writer
// (csv.go) alike, so p15.3–p15.11 add reports without touching either renderer.
// Cells carry RAW values (money as int64 minor + currency, dates as ISO strings,
// text as plain strings) — never pre-formatted display strings — so the HTML
// renderer can honor each user's number/date settings (rule 10) while the CSV
// renderer emits machine-plain values. A row carries an indent level (tree depth),
// subtotal/total flags (styling + emphasis), and a Kind that marks the D19
// intercompany warning row so a nonzero net renders visibly rather than being
// silently dropped.
type Table struct {
	// Columns are the localized-at-render column headers as i18n message ids (the
	// web layer runs them through {{t}}; CSV emits them raw or localized by the
	// caller). Header text is a report concern; the framework carries the keys.
	Columns []Column

	// Rows are the body rows in render order (tree pre-order for hierarchical
	// reports). Subtotal/total rows are interleaved at their tree position.
	Rows []Row
}

// Align selects a column's horizontal alignment hint for the HTML renderer. Money
// columns are right-aligned; text/date columns left. CSV ignores it.
type Align int

const (
	// AlignLeft is the default (text, dates, labels).
	AlignLeft Align = iota
	// AlignRight suits numeric/money columns.
	AlignRight
)

// Column is one table column: its header (an i18n message id) and an alignment
// hint. Keeping the header a KEY (not localized text) upholds rule 9 — the web
// renderer localizes it per request.
type Column struct {
	// HeaderKey is the i18n message id for the column header.
	HeaderKey string
	// Align is the alignment hint for the HTML renderer.
	Align Align
}

// RowKind classifies a row for rendering: an ordinary data row, an emphasized
// subtotal or grand-total row, or the D19 intercompany WARNING row (rendered
// prominently — never dropped — when a consolidated scope's flagged accounts fail
// to net to zero per currency).
type RowKind int

const (
	// RowData is an ordinary data row.
	RowData RowKind = iota
	// RowSubtotal is a subtotal row (a subtree's aggregate); emphasized.
	RowSubtotal
	// RowTotal is a grand-total row; emphasized more strongly.
	RowTotal
	// RowWarning is the D19 intercompany warning row: a nonzero net after
	// collapsing flagged accounts, surfaced as a visible warning per rule (D19).
	RowWarning
)

// Row is one table row: its cells (aligned to Columns positionally), a tree
// Indent level for hierarchical reports, and a Kind. A RowWarning row's cells
// typically hold the warning text and the offending net; the renderers style it
// by Kind, not by content.
type Row struct {
	// Cells are the row's typed cells, positionally aligned with Table.Columns.
	Cells []Cell
	// Indent is the tree depth (0 = top level) the HTML renderer uses to inset the
	// first cell; CSV ignores it (the value cells already carry the data).
	Indent int
	// Kind classifies the row for rendering (data / subtotal / total / warning).
	Kind RowKind
}

// CellKind is a cell's value type: text, money, or date. The renderers switch on
// it to format (HTML per-user, CSV machine-plain) or to emit raw.
type CellKind int

const (
	// CellText is a LITERAL string value rendered verbatim -- a stored proper noun
	// (account/subsidiary/fund name, payee) that must NOT be localized (rule 9:
	// proper nouns are stored data, not catalog entries).
	CellText CellKind = iota
	// CellLabel is an i18n message KEY the renderers localize to the request
	// language (a framework/section/subtotal label). Distinguishing CellLabel from
	// CellText at the cell level (rather than guessing from the string) is the
	// explicit contract every report author follows: emit a label as a key, a
	// proper noun as text -- so a 990-preparer report can never render a raw key or
	// wrongly translate a name.
	CellLabel
	// CellMoney is an int64 minor-unit amount in a named currency (rule 3).
	CellMoney
	// CellDate is an ISO (YYYY-MM-DD) date string.
	CellDate
)

// Cell is one typed table cell. Exactly one value field is meaningful per Kind:
// Text for CellText/CellDate (Date stored as its ISO string), Minor+Currency for
// CellMoney. Money is stored EXACT (int64 minor units + ISO currency, rule 3) so
// the renderer, not the report, decides the display format (rule 10). A CellMoney
// with Blank set renders as an empty cell (e.g. a subtotal label row's amount
// columns) without being mistaken for a zero amount.
type Cell struct {
	Kind CellKind
	// Text holds a CellText value or a CellDate's ISO string.
	Text string
	// Minor is a CellMoney amount in the currency's minor units (rule 3).
	Minor int64
	// Currency is a CellMoney's ISO currency code.
	Currency string
	// Blank, on a CellMoney cell, renders an empty cell instead of a formatted
	// zero — distinguishing "no amount here" from "amount is zero".
	Blank bool

	// Drill, when non-nil, makes this cell "click through" (p15.3d): the web layer
	// renders the cell's value as a link to /reports/{id}/drill?{Drill.Encode()},
	// which lists exactly the transactions whose signed NATIVE sum equals this
	// figure. A nil Drill = not drillable (label cells, totals a report chooses not
	// to drill). It is data-only and pure, so the CSV/text renderers ignore it (the
	// golden is unchanged) and the reconciliation invariant is unit-testable.
	Drill *Drill
}

// WithDrill returns a copy of c carrying the drill descriptor d, so a report builds
// a drillable money cell fluently: MoneyCell(m, ccy).WithDrill(&Drill{...}). Keeping
// it a method (not a MoneyCell parameter) means the many non-drillable call sites
// (labels, totals) stay unchanged and only drillable cells opt in (p15.4+ pattern).
func (c Cell) WithDrill(d *Drill) Cell {
	c.Drill = d
	return c
}

// TextCell builds a LITERAL text cell (a stored proper noun rendered verbatim,
// never localized).
func TextCell(s string) Cell { return Cell{Kind: CellText, Text: s} }

// LabelCell builds a LABEL cell from an i18n message key; the renderers localize
// it to the request language (a section/subtotal/framework label).
func LabelCell(key string) Cell { return Cell{Kind: CellLabel, Text: key} }

// DateCell builds a date cell from an ISO (YYYY-MM-DD) string. The value is stored
// as its ISO text; the HTML renderer reformats it per the user's date setting.
func DateCell(iso string) Cell { return Cell{Kind: CellDate, Text: iso} }

// MoneyCell builds a money cell from exact minor units and an ISO currency code
// (rule 3). The renderers format it (HTML per-user, CSV plain).
func MoneyCell(minor int64, currency string) Cell {
	return Cell{Kind: CellMoney, Minor: minor, Currency: currency}
}

// BlankMoneyCell builds a money cell that renders empty (not a formatted zero),
// for the amount columns of a pure label/heading row.
func BlankMoneyCell() Cell { return Cell{Kind: CellMoney, Blank: true} }
