package reports

import "cuento/internal/ids"

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
	// HeaderKey is the i18n message id for the column header. Empty when HeaderText
	// carries a verbatim proper-noun header instead (a program name, rule 9).
	HeaderKey string
	// HeaderText is a VERBATIM header string (a stored proper noun — e.g. a program
	// name), rendered as-is and NEVER localized (rule 9's carve-out for stored data).
	// A column sets HeaderKey (a catalog label) OR HeaderText (a proper noun), never
	// both; the renderers prefer HeaderText when it is non-empty.
	HeaderText string
	// Align is the alignment hint for the HTML renderer.
	Align Align
	// Group, when non-nil, places this column under a STACKED (two-row) header group
	// (p31 program statement: the program matrix's Admin | Fundraising | Program-
	// services super-columns). The web renderer emits a two-row <thead> — a group row
	// (one <th colspan> per contiguous run of columns sharing a group Key) above the
	// per-column leaf header row — and stamps each program column's data-* attributes
	// (Data) so a follow-up can wire click-to-collapse of a program subtree. The
	// text/CSV golden renderers IGNORE Group entirely (they emit the flat leaf header
	// row), so every other report's golden stays byte-identical.
	Group *ColumnGroup
}

// ColumnGroup is a stacked-header super-column a Column belongs to (p31): the group's
// localized label Key spans a contiguous run of columns that share it, and optional
// per-column Data attributes encode the program tree for the column-collapse follow-up
// (10b). It is a RENDER concern only — the golden/CSV renderers ignore it.
type ColumnGroup struct {
	// Key is the group super-header's i18n message id (e.g. the "Program services"
	// label). Every column in one contiguous group run carries the SAME Key; the web
	// renderer collapses the run into one spanning <th>. An empty Key groups columns
	// under a blank super-header cell (so a leading Total/Admin/Fundraising column can
	// sit alongside the program-services group without a redundant label).
	Key string
	// GroupID identifies the group run: adjacent columns with the same non-empty Key
	// AND GroupID form one <th colspan> super-header. It lets two distinct groups that
	// happen to share a Key stay separate, and disambiguates blank-Key runs.
	GroupID string
	// Data holds render-only data-* attribute values (attribute name -> value) the web
	// renderer stamps on this column's LEAF <th> (p31 10b hook: program id, parent
	// program id, and a group-parent marker). ASCII keys; the web layer prefixes each
	// with "data-". Ignored by the golden/CSV renderers.
	Data map[string]string
}

// RowKind classifies a row for rendering: an ordinary data row, an emphasized
// subtotal or grand-total row, or the D19 intercompany WARNING row (rendered
// prominently — never dropped — when a consolidated scope's flagged accounts fail
// to net to zero per currency).
type RowKind int

const (
	// RowData is an ordinary data row.
	RowData RowKind = iota
	// RowSubtotal is a placeholder-parent roll-up row (a subtree's aggregate);
	// emphasized — the LIGHTEST of the three total tiers.
	RowSubtotal
	// RowSectionTotal is a SECTION total row (p30.10): the definitive figure for a
	// whole section — "Total revenue"/"Total expenses" on the statement of activities,
	// "Total assets"/"Total liabilities"/"Total net assets" on the balance sheet, the
	// per-currency section totals on the program statement. It ranks BETWEEN the
	// placeholder-parent RowSubtotal (lighter) and the grand-total RowTotal (strongest,
	// double rule), so a single-parent section's "Total revenue" no longer looks
	// identical to its "Revenue" parent.
	RowSectionTotal
	// RowTotal is a grand-total row; emphasized most strongly (double rule).
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
	// CellMeasures (p30.9) is a budget-variance grid cell folding THREE co-located
	// money measures — budgeted, actual, variance — in one currency, so the web
	// renderer can emit all three (each formatted server-side, rule 10) and a client
	// module shows one at a time without a round trip. The text/CSV renderers emit all
	// three compound (B/A/V) so the golden stays reconcilable. Budgeted/Actual/Variance
	// hold the exact minor-unit amounts; Currency the ISO code; Bucket the over/under
	// magnitude class (empty = neutral, no color); ActualDrill the actual measure's
	// drill (the budgeted/variance measures never drill — a plan and a derived figure).
	CellMeasures
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

	// Budgeted / Actual / Variance are the three folded measures of a CellMeasures cell
	// (p30.9), exact minor units in Currency. Variance = Actual − Budgeted (the report's
	// net-debit convention: positive = over budget / under-collection). Unused (zero) on
	// every other cell kind.
	Budgeted int64
	Actual   int64
	Variance int64
	// Bucket is a CellMeasures cell's over/under-budget MAGNITUDE class (p30.9): "" =
	// neutral (no color, e.g. a per-month data cell or a zero variance), else one of
	// the varianceBucket* names the web layer maps to a theme-aware CSS class. Set only
	// on TOTAL cells (row-total column + grand-total rows) so color reinforces the
	// variance sign there; ignored by the text/CSV renderers (color is presentation).
	Bucket string

	// TxnID, when nonzero, links this cell to the transaction editor/history (p12.4):
	// the web layer renders the cell's value as a link to /transactions/{TxnID}/edit.
	// It is the account-ledger's (p15.6) line->txn link — a REGISTER line names one
	// split, whose transaction the reviewer clicks to open. Unlike Drill (which lists
	// the splits behind an aggregate figure), TxnID points at ONE concrete
	// transaction, so it is a distinct mechanism, not a drill. Like Drill it is
	// data-only and pure: the reports package never builds the URL (the web layer
	// does, keeping URL construction out of reports), and the CSV/text renderers
	// ignore it (the golden is unchanged). A cell carries at most one of Drill/TxnID.
	TxnID ids.TransactionID
}

// WithDrill returns a copy of c carrying the drill descriptor d, so a report builds
// a drillable money cell fluently: MoneyCell(m, ccy).WithDrill(&Drill{...}). Keeping
// it a method (not a MoneyCell parameter) means the many non-drillable call sites
// (labels, totals) stay unchanged and only drillable cells opt in (p15.4+ pattern).
func (c Cell) WithDrill(d *Drill) Cell {
	c.Drill = d
	return c
}

// WithTxn returns a copy of c linked to transaction txnID (p15.6): the web layer
// renders it as a link to the transaction editor/history (/transactions/{txnID}/edit).
// Kept a method (not a MoneyCell/TextCell parameter) so the many non-linked call
// sites stay unchanged and only a ledger LINE cell opts in.
func (c Cell) WithTxn(txnID ids.TransactionID) Cell {
	c.TxnID = txnID
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

// Over/under-budget magnitude buckets (p30.9): a variance total's |variance|/|budgeted|
// ratio deepens the color. The names are the CSS-class suffix the web layer maps to a
// theme-aware token; "" (VarianceNeutral) means no color. A zero-budget-but-nonzero
// actual variance is inexpressible as a ratio, so it classes LARGE (a wholly
// unbudgeted/uncollected figure is the strongest signal); both-zero is neutral.
const (
	VarianceNeutral  = ""
	VarianceSlight   = "slight"
	VarianceModerate = "moderate"
	VarianceLarge    = "large"
)

// VarianceBucket classifies a variance total into its over/under magnitude bucket from
// the |variance|/|budgeted| ratio (p30.9). It returns the bucket NAME only — the SIGN
// (over vs under) is the web layer's concern (positive variance = over = red ramp;
// negative = under = green ramp), so this is a pure magnitude decision, unit-testable
// at the reports level. Thresholds (DECISIONS p30.9): <10% slight, <25% moderate, else
// large; a zero budget with a nonzero variance is large (unbudgeted); both-zero neutral.
func VarianceBucket(budgeted, variance int64) string {
	if variance == 0 {
		return VarianceNeutral
	}
	if budgeted == 0 {
		return VarianceLarge // nonzero variance against no budget: the strongest signal
	}
	absV, absB := variance, budgeted
	if absV < 0 {
		absV = -absV
	}
	if absB < 0 {
		absB = -absB
	}
	switch {
	case absV*100 < absB*10: // ratio < 0.10
		return VarianceSlight
	case absV*100 < absB*25: // ratio < 0.25
		return VarianceModerate
	default:
		return VarianceLarge
	}
}

// MeasuresCell builds a budget-variance grid cell (p30.9) folding the three measures
// (budgeted, actual, variance) in one currency, with an over/under magnitude bucket
// (VarianceBucket; "" = neutral). The web layer renders all three formatted spans and a
// client module toggles which shows; the text/CSV renderers emit all three compound.
// The ACTUAL measure opts into a drill via WithDrill (only the actual is posted data).
func MeasuresCell(budgeted, actual, variance int64, currency, bucket string) Cell {
	return Cell{
		Kind: CellMeasures, Currency: currency,
		Budgeted: budgeted, Actual: actual, Variance: variance, Bucket: bucket,
	}
}
