package reports

import (
	"context"
	"sort"
)

// FunctionalExpensesReportID is the id (URL slug + registry key) of the functional-
// expenses report (p15.7): the IRS Form 990 PART IX "Statement of Functional
// Expenses" matrix. Expense accounts are grouped under their EFFECTIVE Part IX line
// (D25 inheritance: an account's own 990 code, else the nearest ancestor's — a leaf
// that overrides its parent lands on its OWN line), subtotaled per line, and each
// line's contributing accounts are listed indented beneath it. The three functional
// classes (D21) are the COLUMNS: Program services | Management & general |
// Fundraising, plus a Total column (the per-row sum of the three). Accounts with NO
// effective Part IX code fall into an explicit UNMAPPED bucket rendered LAST (never
// dropped — the whole point of the Z19 unmapped warning), and a grand-total row at
// the foot totals the whole part.
//
// CONVERSION (Params.TargetCurrency, default scope base): a 990 is a single-currency
// form. Part IX line totals are an expense FLOW over the period, so — matching the
// income statement (p15.5), NOT the balance sheet — every cell is converted to the
// target at the TRANSACTION-DATE rate (p26.71: an expense is measured at the rate in
// force when it occurred; the closing rate is for balances). This makes the Part IX
// grand total tie the income statement's total expenses exactly, regardless of intra-
// year FX movement (before p26.71 the closing-rate "year-end rollup" left a gap when
// rates moved mid-year). Each (account,class) cell is converted+rounded ONCE (D12
// final-aggregate grain, FunctionalMatrix RateTxnDate); the line subtotals, the Total
// column, and the grand total are built by INT64 ADDITION of those converted cells (the
// footing rule, as in the income statement) so "Program + Management + Fundraising ==
// Total" holds EXACTLY per row and "Σ lines == grand total" holds exactly, with no
// second rounding.
//
// DRILL-DOWN (p15.3d): each account×class amount cell carries a DrillPeriod filter
// narrowed to {that account, that functional class, the period, the cell's native
// currency}; the drilled NATIVE splits' signed sum reconciles to the cell's
// PRE-conversion native figure (drill.go's invariant, reusing DrillSplits' class
// filter). A single-native-currency account×class cell is drillable; a cell mixing
// native currencies (e.g. Program Supplies holds USD and MXN) and the line-subtotal /
// grand-total rows are not (one currency-filtered link cannot reconcile a summed-
// across-currencies or rolled-up figure — the balance-sheet rule).
//
// PERIOD (from/to), expense accounts only (FunctionalMatrix returns exactly the
// class-tagged expense splits, D21). An empty period returns an empty Table.
const FunctionalExpensesReportID = "functional_expenses"

// functionalClasses is the fixed COLUMN order of the 990 Part IX matrix (D21): the
// three functional-expense classes, left to right, as the IRS form presents them.
var functionalClasses = []Class{"program", "management", "fundraising"}

// classHeaderKey maps a functional class to its column-header i18n key (the shared
// functional.<class> catalog labels, reused from the transaction editor).
var classHeaderKey = map[Class]string{
	"program":     "functional.program",
	"management":  "functional.management",
	"fundraising": "functional.fundraising",
}

// registerFunctionalExpenses registers the functional-expenses report (p15.7) into
// reg under the "tax" (IRS-990) group. It offers the period (from/to) and the
// target-currency control; the shared web params form renders both from the
// ParamsSpec.
func registerFunctionalExpenses(reg *Registry) {
	reg.Register(Report{
		ID:         FunctionalExpensesReportID,
		TitleKey:   "reports.functional_expenses.title",
		Group:      "tax",
		ParamsSpec: ParamsSpec{Period: true, Currency: true},
		Run:        runFunctionalExpenses,
		Tree:       true, // p26.26: each 990 Part IX line nests its expense accounts.
		// p27.4: expense splits carry a program, so this is grant-subtree filterable.
		ProgramDimensioned: true,
	})
}

// runFunctionalExpenses computes the 990 Part IX functional-expenses Table. It reads
// the per-(expense account, class) activity converted to the target at the transaction-
// date rate (and, separately, native per currency for the drill filter), groups the
// accounts by effective Part IX line (D25), and renders one line group per effective
// code (accounts indented under a per-line subtotal) in the part's report order with
// the UNMAPPED bucket last, closing with a grand-total row.
func runFunctionalExpenses(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	target := p.TargetCurrency
	scope := Scope{Sub: p.Scope}

	b := &feBuilder{tk: tk, p: p, target: target}
	b.columns()

	// Converted (target, transaction-date rate) per (account, class) — one cell each.
	// An expense flow is measured at the rate in force when it occurred (D12 RateTxnDate),
	// so this total ties the income statement's total expenses exactly (p26.71).
	conv, err := tk.FunctionalMatrix(ctx, scope, p.From, p.To, ConvertOpts{To: target, Mode: RateTxnDate})
	if err != nil {
		return Table{}, err
	}
	// Native per (account, class) per currency — for the drill filter's native currency
	// and to detect single- vs multi-currency cells (only single-currency cells drill).
	native, err := tk.FunctionalMatrix(ctx, scope, p.From, p.To, ConvertOpts{Mode: RateNone})
	if err != nil {
		return Table{}, err
	}

	// Effective Part IX code per account (D25) and the part's ordered lines + labels.
	eff, err := tk.EffectiveCodes(ctx)
	if err != nil {
		return Table{}, err
	}
	lines, err := tk.Part990Lines(ctx, "IX", "expense")
	if err != nil {
		return Table{}, err
	}

	// Account name lookup (resolved for the request language, D5) for the indented rows.
	names, err := accountNameMap(ctx, tk, p.LangOr())
	if err != nil {
		return Table{}, err
	}

	// Bucket the expense accounts by their effective code ("" = Unmapped). Every
	// account present in the functional matrix is an expense account with a class.
	byCode := make(map[string][]AccountID)
	for acct := range conv {
		byCode[eff[acct]] = append(byCode[eff[acct]], acct)
	}
	// Deterministic account order within a line: by resolved name, then id.
	for code := range byCode {
		accts := byCode[code]
		sort.Slice(accts, func(i, j int) bool {
			ni, nj := names[accts[i]], names[accts[j]]
			if ni != nj {
				return ni < nj
			}
			return accts[i] < accts[j]
		})
		byCode[code] = accts
	}

	// Render each effective line that has accounts, in the part's report order; the
	// UNMAPPED bucket ("") is appended LAST. grand accumulates the whole-part totals.
	var grand classSums
	emit := func(pl PartLine, unmapped bool) {
		accts := byCode[pl.Code]
		if len(accts) == 0 {
			return // a Part IX line with no in-scope accounts is not shown.
		}
		var lineSum classSums
		accountRows := make([]Row, 0, len(accts))
		for _, acct := range accts {
			cells := make([]Cell, 0, len(functionalClasses)+2)
			cells = append(cells, TextCell(names[acct]))
			var rowTotal int64
			for _, cl := range functionalClasses {
				amt := classMinor(conv[acct][cl], target)
				lineSum.add(cl, amt)
				grand.add(cl, amt)
				rowTotal += amt
				cell := MoneyCell(amt, target)
				if d := b.cellDrill(acct, cl, native[acct][cl]); d != nil {
					cell = cell.WithDrill(d)
				}
				cells = append(cells, cell)
			}
			cells = append(cells, MoneyCell(rowTotal, target)) // row total = Σ classes
			accountRows = append(accountRows, Row{Cells: cells, Indent: 1, Kind: RowData})
		}
		// The line grouping/subtotal row FIRST (label + the line's per-class subtotals),
		// then the contributing accounts indented beneath it.
		b.lineRow(pl, unmapped, lineSum)
		b.rows = append(b.rows, accountRows...)
	}

	for _, pl := range lines {
		emit(pl, false)
	}
	// The explicit Unmapped bucket, LAST (accounts with no effective Part IX code).
	emit(PartLine{Code: ""}, true)

	// Grand total (whole Part IX) at the foot.
	b.grandRow(grand)

	return b.table(), nil
}

// classSums accumulates a per-class running total plus the three-class sum, so a line
// subtotal / grand total is built by int64 addition of the converted cells (footing).
type classSums struct {
	program, management, fundraising int64
}

func (s *classSums) add(cl Class, v int64) {
	switch cl {
	case "program":
		s.program += v
	case "management":
		s.management += v
	case "fundraising":
		s.fundraising += v
	}
}

// total returns the three-class sum (the Total column value).
func (s classSums) total() int64 { return s.program + s.management + s.fundraising }

// get returns the per-class value in the fixed column order.
func (s classSums) get(cl Class) int64 {
	switch cl {
	case "program":
		return s.program
	case "management":
		return s.management
	default:
		return s.fundraising
	}
}

// feBuilder accumulates the functional-expenses rows. Columns are [Line, Program,
// Management & general, Fundraising, Total]; every money column is the target
// currency (converted at the transaction-date rate).
type feBuilder struct {
	tk     *Toolkit
	p      Params
	target string

	cols []Column
	rows []Row
}

// columns builds the column set: Line, the three functional-class columns, then Total.
func (b *feBuilder) columns() {
	b.cols = append(b.cols, Column{HeaderKey: "reports.functional_expenses.col.line", Align: AlignLeft})
	for _, cl := range functionalClasses {
		b.cols = append(b.cols, Column{HeaderKey: classHeaderKey[cl], Align: AlignRight})
	}
	b.cols = append(b.cols, Column{HeaderKey: "reports.functional_expenses.col.total", Align: AlignRight})
}

// lineRow appends a 990 Part IX line's grouping/subtotal row: the effective-line label
// (the IRS-seeded "line — label" for a real line, a localized "(Unmapped)" LABEL for
// the "" bucket) followed by the line's per-class subtotals and the line total. A
// rollup over many accounts, so not drillable.
func (b *feBuilder) lineRow(pl PartLine, unmapped bool, sums classSums) {
	cells := make([]Cell, 0, len(functionalClasses)+2)
	if unmapped {
		cells = append(cells, LabelCell("reports.functional_expenses.unmapped"))
	} else {
		cells = append(cells, TextCell(lineLabel(pl))) // IRS-seeded stored text (D25)
	}
	for _, cl := range functionalClasses {
		cells = append(cells, MoneyCell(sums.get(cl), b.target))
	}
	cells = append(cells, MoneyCell(sums.total(), b.target))
	b.rows = append(b.rows, Row{Cells: cells, Kind: RowSubtotal})
}

// grandRow appends the grand-total row (whole Part IX): the localized total label, the
// per-class grand totals, and the grand total (Σ classes). Not drillable.
func (b *feBuilder) grandRow(sums classSums) {
	cells := make([]Cell, 0, len(functionalClasses)+2)
	cells = append(cells, LabelCell("reports.functional_expenses.total"))
	for _, cl := range functionalClasses {
		cells = append(cells, MoneyCell(sums.get(cl), b.target))
	}
	cells = append(cells, MoneyCell(sums.total(), b.target))
	b.rows = append(b.rows, Row{Cells: cells, Kind: RowTotal})
}

// cellDrill builds the DrillPeriod filter for one account×class cell over the report
// period. Only a SINGLE-native-currency cell is drillable: a cell mixing native
// currencies (its converted value sums across currencies) has no single currency-
// filtered drill that reconciles it. The drill carries the account, the class, the
// period, and the native currency; the drilled native splits' signed sum equals the
// pre-conversion native figure (drill.go reconciliation invariant, DrillSplits' class
// filter).
func (b *feBuilder) cellDrill(acct AccountID, cl Class, native []CurAmt) *Drill {
	if len(native) != 1 {
		return nil // no activity, or multi-currency: not drillable
	}
	class := string(cl)
	return &Drill{
		Scope:      b.p.Scope,
		AccountIDs: []int64{int64(acct)},
		Currency:   native[0].Currency,
		Class:      &class,
		Mode:       DrillPeriod,
		From:       b.p.From,
		To:         b.p.To,
	}
}

func (b *feBuilder) table() Table {
	return Table{Columns: b.cols, Rows: b.rows}
}

// --- small helpers ---------------------------------------------------------

// lineLabel renders a 990 line's grouping-row text: "<line> — <label>" (e.g.
// "7 — Other salaries and wages"), the IRS-seeded reference label (D25) rendered
// verbatim as stored data (rule 9's carve-out for stored reference data).
func lineLabel(pl PartLine) string {
	if pl.Line == "" {
		return pl.Label
	}
	if pl.Label == "" {
		return pl.Line
	}
	return pl.Line + " — " + pl.Label
}

// classMinor returns the single converted minor amount for an (account,class) cell in
// the target currency, or 0 when the class is absent. FunctionalMatrix RateTxnDate
// yields exactly one target-currency CurAmt per present cell.
func classMinor(amts []CurAmt, target string) int64 {
	var m int64
	for _, a := range amts {
		if a.Currency == target {
			m += a.Minor
		}
	}
	return m
}

// accountNameMap returns accountID -> resolved name (for lang) for every account, so
// the report labels its indented account rows without a per-row store call (the chart
// is small; one tree read per run).
func accountNameMap(ctx context.Context, tk *Toolkit, lang string) (map[AccountID]string, error) {
	tree, err := tk.Store().Tree(ctx, lang, nil)
	if err != nil {
		return nil, err
	}
	m := make(map[AccountID]string, len(tree))
	for _, r := range tree {
		m[AccountID(r.ID)] = r.Name
	}
	return m, nil
}
