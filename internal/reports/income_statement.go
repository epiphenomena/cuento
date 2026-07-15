package reports

import (
	"context"
	"fmt"

	"cuento/internal/store"
)

// IncomeStatementReportID is the id (URL slug + registry key) of the income-statement
// report (p15.5): the STATEMENT OF ACTIVITIES over a period -- Revenue then Expense,
// with the net surplus/deficit = Revenue - Expense at the foot. Unlike the balance
// sheet (a point-in-time position converted at the CLOSING rate), the income statement
// is a FLOW: its figures are period ACTIVITY, converted at each transaction's-date rate
// (D12 RateTxnDate), because a revenue/expense earned across a period is measured at
// the rate in force when it occurred, not at a single closing rate.
//
// MIXED R/E TREE PRESERVED: the account tree structure is rendered, not flattened --
// the Revenue and Expenses placeholder parents are SUBTOTAL rows, their children are
// indented data rows. reBuilder walks the account tree pre-order within each section,
// so a reviewer reads the same hierarchy the chart of accounts has.
//
// COMPARATIVE COLUMNS (Granularity): with month/quarter granularity the period splits
// into calendar sub-periods (via the toolkit's deterministic ByPeriod bucketing), one
// COLUMN per sub-period (Jan|Feb|...|Total, or 2025-Q1|...|Total), each column that
// sub-period's activity. The TOTAL column is the SUM of the per-period columns per row
// (NOT a fresh whole-range Activity call): under RateTxnDate the per-account converted
// figure is rounded half-even once per Activity call (compute.go), so round(Sum months)
// can differ from Sum(round month) by a minor unit. Building the total by ADDING the
// per-period converted cells makes "sum of the period columns == total column, per row"
// hold EXACTLY (int64 addition, no rounding) -- a statement that does not foot is wrong,
// so footing wins over bit-equality with a whole-range NetIncome (D-note p15.5).
//
// CONVERSION (Params.TargetCurrency, RateTxnDate): every cell is converted to the target
// (default scope base) at the transaction-date rate. GranNone still uses per-period
// bucketing internally with a single whole-range bucket, so the total is well-defined.
//
// DRILL-DOWN (p15.3d): each single-native-currency leaf activity cell drills (DrillPeriod)
// to the transactions in that column's sub-period range and native currency -- the
// converted cell drills to its NATIVE splits, reconciling to the pre-conversion native
// figure (drill.go's invariant). Subtotal/net rows and multi-currency leaves are not
// drillable (one link cannot reconcile a rolled-up or summed-across-currencies figure).
const IncomeStatementReportID = "income_statement"

// registerIncomeStatement registers the income-statement report (p15.5) into reg under
// the "financial" group. It offers the period (from/to), granularity, and target-
// currency controls; the shared web params form renders them from the ParamsSpec.
func registerIncomeStatement(reg *Registry) {
	reg.Register(Report{
		ID:         IncomeStatementReportID,
		TitleKey:   "reports.income_statement.title",
		Group:      "financial",
		ParamsSpec: ParamsSpec{Period: true, Granularity: true, Currency: true},
		Run:        runIncomeStatement,
		Tree:       true, // p26.26: the R/E tree nests placeholder parents over leaves.
	})
}

// period is one comparative column: its inclusive [from,to] sub-period bounds and a
// stable, format-independent LABEL (e.g. "2025-01" for a month, "2025-Q1" for a
// quarter, or the whole range for GranNone). The label is a period IDENTIFIER, not a
// formatted date (rule 10 governs date/number RENDERING; a period token is neither), so
// it is deterministic across locales and safe as a column header.
type period struct {
	from, to string
	label    string
}

// runIncomeStatement computes the income-statement Table (p15.5). It decomposes the
// [From,To] period into comparative sub-periods per the granularity, converts each
// account's per-period activity to the target at the transaction-date rate (D12), builds
// the TOTAL column as the per-row sum of the period columns, and renders the Revenue and
// Expense sections as a preserved account tree (parents as subtotals, children indented),
// closing with the net surplus/deficit (Revenue - Expense) per column.
func runIncomeStatement(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	target := p.TargetCurrency

	periods, err := buildPeriods(tk, p)
	if err != nil {
		return Table{}, err
	}

	// Per-period converted activity per account (target currency, txn-date rate). One
	// map per column; a leaf absent from a period simply has no entry (rendered zero).
	perAcct := make([]map[AccountID]int64, len(periods))
	// Per-period NATIVE activity per (account,currency) -- for the drill filter's native
	// currency and to detect single- vs multi-currency leaves (only single-currency
	// leaves are drillable, mirroring the balance sheet).
	nativeCcy := make([]map[AccountID]map[string]int64, len(periods))
	for i, pr := range periods {
		conv, err := tk.Activity(ctx, Scope{Sub: p.Scope}, pr.from, pr.to, ConvertOpts{To: target, Mode: RateTxnDate})
		if err != nil {
			return Table{}, err
		}
		m := make(map[AccountID]int64, len(conv))
		for acct, amts := range conv {
			for _, a := range amts {
				m[acct] += a.Minor
			}
		}
		perAcct[i] = m

		nat, err := tk.Activity(ctx, Scope{Sub: p.Scope}, pr.from, pr.to, ConvertOpts{Mode: RateNone})
		if err != nil {
			return Table{}, err
		}
		nm := make(map[AccountID]map[string]int64, len(nat))
		for acct, amts := range nat {
			cm := make(map[string]int64, len(amts))
			for _, a := range amts {
				cm[a.Currency] += a.Minor
			}
			nm[acct] = cm
		}
		nativeCcy[i] = nm
	}

	// The TOTAL column per account = SUM of the per-period converted cells (footing rule).
	total := make(map[AccountID]int64)
	for _, m := range perAcct {
		for acct, v := range m {
			total[acct] += v
		}
	}

	storeTree, err := tk.Store().Tree(ctx, p.LangOr(), nil)
	if err != nil {
		return Table{}, err
	}
	tree := toTreeNodes(storeTree)

	b := &isBuilder{
		tk: tk, p: p, target: target,
		periods:   periods,
		perAcct:   perAcct,
		nativeCcy: nativeCcy,
		total:     total,
	}
	b.columns()

	// --- Revenue section (revenue accounts, tree order preserved). Revenue activity is
	// net-debit NEGATIVE (a credit), so it is displayed with sign -1 to read as a
	// positive inflow.
	revNet := b.section(tree, "revenue", "reports.income_statement.section.revenue",
		"reports.income_statement.total.revenue", -1)

	// --- Expense section. Expense activity is net-debit POSITIVE (a debit), already the
	// way an outflow reads, so display sign +1.
	expNet := b.section(tree, "expense", "reports.income_statement.section.expenses",
		"reports.income_statement.total.expenses", +1)

	// --- Net surplus/deficit = Revenue - Expense, per column. Revenue activity is
	// net-debit NEGATIVE (a credit); expense POSITIVE. Presented the way a statement of
	// activities reads: a SURPLUS is positive. Revenue shown positive = -revNet; net
	// surplus = (-revNet) - expNet = -(revNet + expNet). revNet/expNet are the raw
	// net-debit column sums, so surplus[i] = -(revNet[i] + expNet[i]).
	surplus := make([]int64, len(periods)+1)
	for i := range surplus {
		surplus[i] = -(revNet[i] + expNet[i])
	}
	b.netLine("reports.income_statement.net", surplus)

	return b.table(), nil
}

// buildPeriods decomposes p's [From,To] into comparative columns per the granularity.
// GranNone yields a single whole-range column (so the total is still well-defined and
// the report reads as a plain period statement). Month/quarter use the toolkit's
// deterministic ByPeriod bucketing so the columns are exactly the sub-periods the
// TxnDate conversion decomposes into. Each column carries a stable period-identifier
// label (YYYY-MM / YYYY-Qn), not a locale-formatted date.
func buildPeriods(tk *Toolkit, p Params) ([]period, error) {
	from, to := p.From, p.To
	if p.Granularity == GranNone {
		return []period{{from: from, to: to, label: ""}}, nil
	}
	var out []period
	err := tk.ByPeriod(from, to, p.Granularity, func(pf, pt string) error {
		out = append(out, period{from: pf, to: pt, label: periodLabel(pf, p.Granularity)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// periodLabel builds a column's stable identifier from its start date: "YYYY-MM" for a
// month, "YYYY-Qn" for a quarter. It is a period NAME (a coarse column marker), not a
// formatted date, so it is deterministic across locales and honors rule 10 (which
// governs date/number rendering, not period identifiers).
func periodLabel(from string, g Granularity) string {
	y, m, err := yearMonth(from)
	if err != nil {
		return from
	}
	if g == GranQuarter {
		q := (m-1)/3 + 1
		return fmt.Sprintf("%04d-Q%d", y, q)
	}
	return fmt.Sprintf("%04d-%02d", y, m)
}

// isBuilder accumulates the income-statement rows. Columns are [Line, <period...>,
// Total]; every money column is the target currency (converted, txn-date). It walks the
// account tree per section, emitting placeholder parents as subtotal rows and leaves as
// indented data rows, so the R/E tree is preserved.
type isBuilder struct {
	tk      *Toolkit
	p       Params
	target  string
	periods []period

	perAcct   []map[AccountID]int64            // [col]acct -> converted minor
	nativeCcy []map[AccountID]map[string]int64 // [col]acct -> ccy -> native minor
	total     map[AccountID]int64              // acct -> converted total (sum of cols)

	cols []Column
	rows []Row
}

// columns builds the column set: Line, one per comparative period, then Total.
func (b *isBuilder) columns() {
	b.cols = append(b.cols, Column{HeaderKey: "reports.income_statement.col.line", Align: AlignLeft})
	for _, pr := range b.periods {
		key := "reports.income_statement.col.period" // GranNone: a single "Period" column
		if pr.label != "" {
			key = pr.label // a period identifier, surfaced verbatim by the localizer
		}
		b.cols = append(b.cols, Column{HeaderKey: key, Align: AlignRight})
	}
	b.cols = append(b.cols, Column{HeaderKey: "reports.income_statement.col.total", Align: AlignRight})
}

// section renders one R/E section (type "revenue"|"expense") as a preserved tree: the
// section header, then the account tree (placeholder parents as subtotal rows carrying
// their subtree's per-column sums, leaves WITH ACTIVITY as indented data rows), then the
// section total. `sign` is the DISPLAY sign applied to the raw net-debit sums: revenue is
// net-debit negative (a credit) so sign -1 shows a positive inflow; expense is net-debit
// positive (a debit) so sign +1. It returns the section's per-column RAW net-debit sums
// (index len(periods) is the Total column) so the caller derives the net surplus once.
//
// A leaf is IN the section only when it is of `typ` AND carries activity in this scope
// (present in the converted-total map) -- so a global chart-of-accounts leaf with no
// activity in the scope (e.g. a US revenue account under a leaf-MX scope) is not shown, a
// placeholder parent with no in-scope activity likewise drops out.
func (b *isBuilder) section(tree []treeNode, typ, sectionKey, totalKey string, sign int64) []int64 {
	children, roots, isPlaceholder, name, depth, typeOf := indexTree(tree)

	inSection := make(map[AccountID]bool)
	colSum := make(map[AccountID][]int64) // [col] converted sum; col len(periods) = Total
	ncols := len(b.periods) + 1

	var fold func(id AccountID) []int64
	fold = func(id AccountID) []int64 {
		sums := make([]int64, ncols)
		if !isPlaceholder[id] {
			_, hasActivity := b.total[id]
			if typeOf[id] == typ && hasActivity {
				inSection[id] = true
				for i := range b.periods {
					sums[i] = b.perAcct[i][id]
				}
				sums[len(b.periods)] = b.total[id]
			}
			colSum[id] = sums
			return sums
		}
		for _, c := range children[id] {
			cs := fold(c)
			for i := range sums {
				sums[i] += cs[i]
			}
		}
		for _, c := range children[id] {
			if inSection[c] {
				inSection[id] = true
				break
			}
		}
		colSum[id] = sums
		return sums
	}
	for _, r := range roots {
		fold(r)
	}

	b.sectionHeader(sectionKey)

	sectionSum := make([]int64, ncols)
	var walk func(id AccountID)
	walk = func(id AccountID) {
		if !inSection[id] {
			return
		}
		if isPlaceholder[id] {
			b.amountRow(LabelableName(name[id]), applySign(colSum[id], sign), depth[id], RowSubtotal)
			for _, c := range children[id] {
				walk(c)
			}
			return
		}
		b.leafRow(name[id], id, depth[id], sign)
		for i := range sectionSum {
			sectionSum[i] += colSum[id][i]
		}
	}
	for _, r := range roots {
		walk(r)
	}

	b.totalRow(totalKey, applySign(sectionSum, sign))
	return sectionSum // RAW net-debit sums (caller flips once for the net line)
}

// leafRow emits one account leaf's row: its name, one converted cell per period, and the
// converted total, each multiplied by the section display sign. A single-native-currency
// leaf's cells are DRILLABLE (DrillPeriod, that column's range + native ccy); a multi-
// currency leaf is left non-drillable (one link cannot reconcile a summed-across-
// currencies converted cell -- the balance-sheet rule).
func (b *isBuilder) leafRow(name string, id AccountID, depth int, sign int64) {
	cells := make([]Cell, 0, len(b.periods)+2)
	cells = append(cells, TextCell(name))
	for i, pr := range b.periods {
		cell := MoneyCell(sign*b.perAcct[i][id], b.target)
		if d := b.leafDrill(id, pr); d != nil {
			cell = cell.WithDrill(d)
		}
		cells = append(cells, cell)
	}
	// Total column (drills across the whole range, single-currency leaves only).
	tot := MoneyCell(sign*b.total[id], b.target)
	if d := b.leafDrill(id, period{from: b.p.From, to: b.p.To}); d != nil {
		tot = tot.WithDrill(d)
	}
	cells = append(cells, tot)
	b.rows = append(b.rows, Row{Cells: cells, Indent: depth, Kind: RowData})
}

// leafDrill builds the DrillPeriod filter for a leaf's cell over the range pr (a
// column's sub-period, or the whole [From,To] for the Total cell). Only a SINGLE-native-
// currency leaf is drillable: a multi-currency leaf's converted cell sums across
// currencies and no single currency-filtered drill reconciles it. The drill carries the
// account, that range, and the native currency; the drilled native splits' signed sum
// equals the pre-conversion native figure (drill.go reconciliation invariant).
func (b *isBuilder) leafDrill(id AccountID, pr period) *Drill {
	// Determine the leaf's native currency set across the whole range (union of columns).
	ccys := map[string]bool{}
	for i := range b.periods {
		for c := range b.nativeCcy[i][id] {
			ccys[c] = true
		}
	}
	if len(ccys) != 1 {
		return nil // multi-currency (or no) native currency: not drillable
	}
	var ccy string
	for c := range ccys {
		ccy = c
	}
	return &Drill{
		Scope:      b.p.Scope,
		AccountIDs: []int64{id},
		Currency:   ccy,
		Mode:       DrillPeriod,
		From:       pr.from,
		To:         pr.to,
	}
}

// sectionHeader appends a section heading row (a label + blank money cells).
func (b *isBuilder) sectionHeader(key string) {
	b.rows = append(b.rows, Row{Cells: b.labelRow(LabelCell(key)), Kind: RowData})
}

// amountRow appends a subtotal row for a placeholder parent: its name (a stored account
// name / proper noun rendered as TEXT), one money cell per period, and the total. Not
// drillable (a rollup spans many leaves).
func (b *isBuilder) amountRow(nameCell Cell, cols []int64, indent int, kind RowKind) {
	cells := make([]Cell, 0, len(cols)+1)
	cells = append(cells, nameCell)
	for _, v := range cols {
		cells = append(cells, MoneyCell(v, b.target))
	}
	b.rows = append(b.rows, Row{Cells: cells, Indent: indent, Kind: kind})
}

// totalRow appends a section total row from a per-column value slice (already sign-
// flipped to positive).
func (b *isBuilder) totalRow(key string, cols []int64) {
	cells := make([]Cell, 0, len(cols)+1)
	cells = append(cells, LabelCell(key))
	for _, v := range cols {
		cells = append(cells, MoneyCell(v, b.target))
	}
	b.rows = append(b.rows, Row{Cells: cells, Indent: 0, Kind: RowSubtotal})
}

// netLine appends the grand net surplus/deficit row (Revenue - Expense) per column.
func (b *isBuilder) netLine(key string, cols []int64) {
	cells := make([]Cell, 0, len(cols)+1)
	cells = append(cells, LabelCell(key))
	for _, v := range cols {
		cells = append(cells, MoneyCell(v, b.target))
	}
	b.rows = append(b.rows, Row{Cells: cells, Indent: 0, Kind: RowTotal})
}

// labelRow builds a heading row's cells: the label + one blank money cell per period +
// the blank total cell.
func (b *isBuilder) labelRow(label Cell) []Cell {
	cells := make([]Cell, 0, len(b.periods)+2)
	cells = append(cells, label)
	for range b.periods {
		cells = append(cells, BlankMoneyCell())
	}
	cells = append(cells, BlankMoneyCell())
	return cells
}

func (b *isBuilder) table() Table {
	return Table{Columns: b.cols, Rows: b.rows}
}

// --- small helpers ---------------------------------------------------------

// treeNode mirrors the fields income_statement reads from store.TreeRow, so this file
// doesn't name the generated sqlc type. Built via toTreeNodes.
type treeNode struct {
	ID       int64
	ParentID int64
	HasPar   bool
	Type     string
	Name     string
}

// applySign returns a copy of cols with every element multiplied by sign (the section's
// display sign: -1 for revenue net-debit-credit -> positive inflow, +1 for expense).
func applySign(cols []int64, sign int64) []int64 {
	out := make([]int64, len(cols))
	for i, v := range cols {
		out[i] = sign * v
	}
	return out
}

// LabelableName wraps a stored account name as a TEXT cell (a proper noun, rendered
// verbatim -- the R/E placeholder parents "Revenue"/"Expenses" are account names in
// account_names, not catalog keys).
func LabelableName(name string) Cell { return TextCell(name) }

// indexTree reduces a store account tree to the maps the section walk needs: children
// (ordered), roots, placeholder set, name, depth, and type -- keyed by account id.
func indexTree(tree []treeNode) (
	children map[int64][]int64, roots []int64, isPlaceholder map[int64]bool,
	name map[int64]string, depth map[int64]int, typeOf map[int64]string,
) {
	children = make(map[int64][]int64)
	isPlaceholder = make(map[int64]bool)
	name = make(map[int64]string)
	typeOf = make(map[int64]string)
	parentOf := make(map[int64]int64)
	for _, r := range tree {
		name[r.ID] = r.Name
		typeOf[r.ID] = r.Type
		if r.HasPar {
			children[r.ParentID] = append(children[r.ParentID], r.ID)
			parentOf[r.ID] = r.ParentID
		} else {
			roots = append(roots, r.ID)
		}
	}
	for p := range children {
		isPlaceholder[p] = true
	}
	depth = make(map[int64]int)
	for _, r := range tree {
		d := 0
		for n := r.ID; ; {
			pp, ok := parentOf[n]
			if !ok {
				break
			}
			d++
			n = pp
		}
		depth[r.ID] = d
	}
	return children, roots, isPlaceholder, name, depth, typeOf
}

// toTreeNodes reduces the store's account tree rows to the local treeNode shape (so the
// rest of this file never names the generated sqlc type). The store returns pre-order,
// which toTreeNodes preserves (children order = the chart-of-accounts sort order).
func toTreeNodes(rows []store.TreeRow) []treeNode {
	out := make([]treeNode, len(rows))
	for i, r := range rows {
		out[i] = treeNode{
			ID:     r.ID,
			Type:   r.Type,
			Name:   r.Name,
			HasPar: r.ParentID.Valid,
		}
		if r.ParentID.Valid {
			out[i].ParentID = r.ParentID.Int64
		}
	}
	return out
}
