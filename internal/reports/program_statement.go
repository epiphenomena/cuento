package reports

import (
	"context"
	"sort"
)

// ProgramStatementReportID is the id (URL slug + registry key) of the program statement
// report (p15.10): the DECISION-MAKER view of revenue and expense per PROGRAM (D24), and
// the source p15.11 draws 990 Part III (program service accomplishments) from. Rows are
// natural ACCOUNTS grouped into a Revenue then an Expense section (per currency), with a
// net-per-program line at the foot. Every program figure is a PROGRAM-TREE ROLLUP: a
// parent program's cell folds in its descendant programs' activity (ProgramActivity,
// D24), so the root program "General" carries the whole org's program activity.
//
// TWO PARAMETERIZATIONS, selected by the report-specific PROGRAM param:
//
//   - COMPARATIVE (the default, no program chosen): one COLUMN per program in the tree
//     (pre-order) — General, then its children — so a decision-maker compares programs
//     side by side. Columns: Account | Currency | <program...>. The ROOT (General) column
//     IS the organization-wide total by construction (D24 single seeded root); no separate
//     Total column is emitted (it would duplicate the General column byte for byte).
//
//   - SINGLE SUBTREE (a program chosen via ?program=): that program (and its descendants
//     rolled up) alone — Account | Currency | <program name>, Revenue then Expense sections,
//     net at the foot. The degenerate one-column case of the comparative view (the single
//     money column's header is the chosen program's name, like every comparative column).
//
// CONVERSION (p26.54): NATIVE by DEFAULT — the statement reads in native currency,
// per-currency rows (like the sibling activity reports p15.8/p15.9). A program is a
// management/mission dimension read in the money each grant/spend occurred in, and the
// 990 preparer wants the native figures, so native stays the default (Documented in
// DECISIONS). An OPTIONAL target-currency selector (ParamsSpec.CurrencyOptional, the
// leading "— native —" choice) converts the whole matrix to one currency at the
// period-end CLOSING rate (D12): the Currency column drops and each account collapses to
// ONE converted row per program column. A converted cell sums across an account's native
// currencies, so (like the trial balance / income statement) it is not drillable; the
// native default keeps the per-currency drills.
//
// DRILL-DOWN (p15.3d): each program×account amount cell drills (DrillPeriod) to its
// contributing splits — filtered by program + account + period + native currency. A ROLLUP
// cell (a program WITH descendants, e.g. General) drills across the program SUBTREE via the
// program-SET drill (Drill.ProgramIDs = the program's descendants incl. self), unioning the
// per-program split sets so the drilled native sum reconciles to the rolled figure; a LEAF
// program's cell uses the single ProgramID. A single-native-currency cell is drillable; the
// net rows and blank cells are not.
//
// PERIOD (from/to), revenue+expense accounts only (only R/E splits carry a program, D24).
const ProgramStatementReportID = "program_statement"

// registerProgramStatement registers the program statement (p15.10) into reg under the
// "programs" group. It offers the period (from/to) and the report-specific PROGRAM
// selector; the shared web params form renders both from the ParamsSpec. No currency
// control — the statement is native (per-currency rows), like p15.8/p15.9.
func registerProgramStatement(reg *Registry) {
	reg.Register(Report{
		ID:         ProgramStatementReportID,
		TitleKey:   "reports.program_statement.title",
		Group:      "programs",
		ParamsSpec: ParamsSpec{Period: true, Program: true, CurrencyOptional: true},
		Run:        runProgramStatement,
	})
}

// progCol is one comparative program column: the program id, its display name (a stored
// proper noun surfaced verbatim as the column header), and the DESCENDANT set (self +
// children, D24) a rollup cell's drill spans. When the program is a LEAF (descendants ==
// {self}), a cell drills with the single ProgramID; otherwise with the program SET.
type progCol struct {
	id          ProgramID
	name        string
	descendants []int64 // self + all descendants (for the rollup-cell drill)
}

// runProgramStatement computes the program statement Table (p15.10). It reads the rolled
// per-(program, account) native activity, resolves the program columns (all programs in
// tree pre-order for the comparative view, or the single chosen program's subtree), and
// renders the Revenue then Expense sections (rows = natural account × currency), closing
// with a net-per-program line per currency.
func runProgramStatement(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	scope := Scope{Sub: p.Scope}

	// p26.54: NATIVE by default (per-currency rows, the documented default) OR CONVERTED
	// to a chosen target currency (one row per account, no currency column) when the
	// optional currency param is set. converted keys off a non-empty target.
	converted := p.TargetCurrency != ""

	// Rolled per-(program, account) activity: a parent program's cells fold in its
	// descendants (ProgramActivity does the tree rollup, D24). Native (RateNone) => per
	// currency, exactly the figures the per-currency rows show and the drills reconcile
	// to. Converted (RateClosing at the period end, D12) => one target-currency CurAmt
	// per (program, account), summed across the account's native currencies.
	opts := ConvertOpts{Mode: RateNone}
	if converted {
		opts = ConvertOpts{To: p.TargetCurrency, Mode: RateClosing}
	}
	act, err := tk.ProgramActivity(ctx, scope, p.From, p.To, opts)
	if err != nil {
		return Table{}, err
	}

	cols, err := programColumns(ctx, tk, p)
	if err != nil {
		return Table{}, err
	}

	// Account name (resolved for lang, D5), type, and tree order.
	storeTree, err := tk.Store().Tree(ctx, p.LangOr(), nil)
	if err != nil {
		return Table{}, err
	}
	tree := toTreeNodes(storeTree)

	b := &psBuilder{tk: tk, p: p, cols: cols, act: act, converted: converted}
	b.columns()

	// Revenue section: revenue accounts, net-debit NEGATIVE (a credit), shown +inflow
	// (sign −1). Expense section: net-debit POSITIVE (a debit), shown as-is (sign +1).
	revNet := b.section(tree, "revenue", "reports.program_statement.section.revenue",
		"reports.program_statement.total.revenue", -1)
	expNet := b.section(tree, "expense", "reports.program_statement.section.expenses",
		"reports.program_statement.total.expenses", +1)

	// Net per program, per currency = revenue − expenses. revNet/expNet are the raw
	// net-debit column sums per currency: net surplus = −(revNet + expNet), mirroring the
	// income statement (revenue positive = −revNet; net = −revNet − expNet).
	b.netLines(revNet, expNet)

	return b.table(), nil
}

// programColumns resolves the report's program columns: for the COMPARATIVE view (no
// program chosen) every program in tree pre-order, each carrying its descendant set for the
// rollup drill; for the SINGLE view (a program chosen) just that program (its subtree rolled
// up). A program's descendants (self + children) come from ProgramDescendants (D24).
func programColumns(ctx context.Context, tk *Toolkit, p Params) ([]progCol, error) {
	tree, err := tk.Store().ProgramTree(ctx)
	if err != nil {
		return nil, err
	}
	// Descendants (self + subtree) per program, for the rollup-cell drill.
	descOf := func(id int64) ([]int64, error) {
		rows, err := tk.Store().ProgramDescendants(ctx, id)
		if err != nil {
			return nil, err
		}
		ids := make([]int64, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		return ids, nil
	}

	if p.Program != 0 {
		// Single-program subtree: one column, the chosen program rolled up.
		var name string
		for _, n := range tree {
			if n.ID == p.Program {
				name = n.Name
				break
			}
		}
		desc, err := descOf(p.Program)
		if err != nil {
			return nil, err
		}
		return []progCol{{id: p.Program, name: name, descendants: desc}}, nil
	}

	// Comparative: every program in tree pre-order (ProgramTree is pre-order).
	cols := make([]progCol, 0, len(tree))
	for _, n := range tree {
		desc, err := descOf(n.ID)
		if err != nil {
			return nil, err
		}
		cols = append(cols, progCol{id: n.ID, name: n.Name, descendants: desc})
	}
	return cols, nil
}

// psBuilder accumulates the program-statement rows. The leading columns are Account,
// Currency; then one money column per program (native, per-currency rows).
type psBuilder struct {
	tk   *Toolkit
	p    Params
	cols []progCol
	act  map[ProgramID]map[AccountID][]CurAmt
	// converted omits the Currency column and renders one target-currency row per
	// account (p26.54); false keeps the native per-currency rows.
	converted bool

	tableCols []Column
	rows      []Row
}

// columns builds the column set: Account, Currency, then one per program (its name as a
// verbatim header — a stored proper noun surfaced through i18n.T's passthrough, the same
// mechanism the income statement uses for its period-identifier headers).
func (b *psBuilder) columns() {
	b.tableCols = append(b.tableCols, Column{HeaderKey: "reports.program_statement.col.account", Align: AlignLeft})
	if !b.converted {
		// Native mode carries a Currency column (per-currency rows); the converted view
		// (single target currency) drops it (p26.54).
		b.tableCols = append(b.tableCols, Column{HeaderKey: "reports.program_statement.col.currency", Align: AlignLeft})
	}
	for _, c := range b.cols {
		b.tableCols = append(b.tableCols, Column{HeaderKey: c.name, Align: AlignRight})
	}
}

// section renders one R/E section (type "revenue"|"expense") as: a section header, one row
// per (account, currency) present in ANY program column (accounts in tree pre-order,
// currencies sorted), then a section total row. `sign` is the display sign applied to the
// raw net-debit figures (−1 revenue, +1 expense). It returns the section's raw net-debit
// column sums per (program-column-index, currency), so the caller derives the net line once.
func (b *psBuilder) section(tree []treeNode, typ, sectionKey, totalKey string, sign int64) map[string][]int64 {
	_, roots, isPlaceholder, name, _, typeOf := indexTree(tree)

	// Pre-order account ids of this type (leaves only carry activity; placeholders are the
	// R/E parents, skipped as data rows here — this report lists natural leaf accounts).
	order := preorderAccounts(tree, roots)

	// Collect the (account, currency) rows that have any activity in any program column,
	// and the per-column native amount for each. amt[acct][ccy][col] = raw net-debit minor.
	amt := make(map[AccountID]map[string][]int64)
	present := func(acct AccountID, ccy string, col int, minor int64) {
		if amt[acct] == nil {
			amt[acct] = make(map[string][]int64)
		}
		if amt[acct][ccy] == nil {
			amt[acct][ccy] = make([]int64, len(b.cols))
		}
		amt[acct][ccy][col] += minor
	}
	for col, c := range b.cols {
		for acct, byCcy := range b.act[c.id] {
			if typeOf[acct] != typ || isPlaceholder[acct] {
				continue
			}
			for _, a := range byCcy {
				present(acct, a.Currency, col, a.Minor)
			}
		}
	}

	b.headerRow(sectionKey)

	// Section totals per (column, currency), and the per-column net sums per currency.
	sectionSum := make(map[string][]int64)
	addSum := func(sums map[string][]int64, ccy string, col int, v int64) {
		if sums[ccy] == nil {
			sums[ccy] = make([]int64, len(b.cols))
		}
		sums[ccy][col] += v
	}

	for _, acct := range order {
		byCcy, ok := amt[acct]
		if !ok {
			continue
		}
		ccys := make([]string, 0, len(byCcy))
		for ccy := range byCcy {
			ccys = append(ccys, ccy)
		}
		sort.Strings(ccys)
		for _, ccy := range ccys {
			cells := b.leadCells(TextCell(name[acct]), ccy)
			for col, c := range b.cols {
				raw := byCcy[ccy][col]
				addSum(sectionSum, ccy, col, raw)
				cell := MoneyCell(sign*raw, b.moneyCcy(ccy))
				// Native cells drill to their contributing splits; a CONVERTED cell sums
				// across an account's native currencies (p26.54) so it is not drillable
				// (the trial-balance / income-statement rule for converted figures).
				if !b.converted {
					if d := b.cellDrill(acct, ccy, c, raw); d != nil {
						cell = cell.WithDrill(d)
					}
				}
				cells = append(cells, cell)
			}
			b.rows = append(b.rows, Row{Cells: cells, Indent: 1, Kind: RowData})
		}
	}

	// Section total rows (one per currency), signed for display.
	b.totalRows(totalKey, sectionSum, sign)
	return sectionSum
}

// cellDrill builds the DrillPeriod filter for one program×account×currency cell. A ROLLUP
// program (descendants beyond self) drills across its subtree via the program SET
// (Drill.ProgramIDs); a LEAF program uses the single ProgramID. The drilled native splits'
// signed sum equals the cell's pre-display net-debit figure (drill.go's invariant). A cell
// with NO activity (raw == 0, e.g. a program that never touched this account) is left
// non-drillable — there is nothing to list — matching the functional-expenses precedent.
func (b *psBuilder) cellDrill(acct AccountID, ccy string, c progCol, raw int64) *Drill {
	if raw == 0 {
		return nil
	}
	d := &Drill{
		Scope:      b.p.Scope,
		AccountIDs: []int64{int64(acct)},
		Currency:   ccy,
		Mode:       DrillPeriod,
		From:       b.p.From,
		To:         b.p.To,
	}
	if len(c.descendants) > 1 {
		d.ProgramIDs = c.descendants // rollup: union the subtree's per-program splits
	} else {
		id := int64(c.id)
		d.ProgramID = &id // leaf: single program
	}
	return d
}

// leadCells builds a row's leading cells: the name/label cell, plus (native mode only)
// the currency-code cell. In converted mode the Currency column is dropped (p26.54), so
// only the name cell leads.
func (b *psBuilder) leadCells(nameCell Cell, ccy string) []Cell {
	cells := make([]Cell, 0, len(b.cols)+2)
	cells = append(cells, nameCell)
	if !b.converted {
		cells = append(cells, TextCell(ccy))
	}
	return cells
}

// moneyCcy is the currency code a money cell carries: the row's native currency in
// native mode, or the target currency in converted mode (every converted amount is
// already in the target, so it labels/formats as the target).
func (b *psBuilder) moneyCcy(ccy string) string {
	if b.converted {
		return b.p.TargetCurrency
	}
	return ccy
}

// headerRow appends a section heading row: the section label + blank money cells.
func (b *psBuilder) headerRow(key string) {
	cells := b.leadCells(LabelCell(key), "")
	for range b.cols {
		cells = append(cells, BlankMoneyCell())
	}
	b.rows = append(b.rows, Row{Cells: cells, Kind: RowData})
}

// totalRows appends one section-total row per currency (sorted), each with the section's
// per-column sums signed for display.
func (b *psBuilder) totalRows(key string, sums map[string][]int64, sign int64) {
	ccys := make([]string, 0, len(sums))
	for ccy := range sums {
		ccys = append(ccys, ccy)
	}
	sort.Strings(ccys)
	for _, ccy := range ccys {
		cells := b.leadCells(LabelCell(key), ccy)
		for col := range b.cols {
			cells = append(cells, MoneyCell(sign*sums[ccy][col], b.moneyCcy(ccy)))
		}
		b.rows = append(b.rows, Row{Cells: cells, Kind: RowSubtotal})
	}
}

// netLines appends the net-per-program rows (one per currency): net = revenue − expenses
// per column. revNet/expNet are the raw net-debit column sums per currency; the displayed
// net surplus is −(revNet + expNet) (revenue shown positive = −revNet; net = −revNet −
// expNet), the income-statement convention.
func (b *psBuilder) netLines(revNet, expNet map[string][]int64) {
	ccySet := map[string]bool{}
	for ccy := range revNet {
		ccySet[ccy] = true
	}
	for ccy := range expNet {
		ccySet[ccy] = true
	}
	ccys := make([]string, 0, len(ccySet))
	for ccy := range ccySet {
		ccys = append(ccys, ccy)
	}
	sort.Strings(ccys)

	for _, ccy := range ccys {
		cells := b.leadCells(LabelCell("reports.program_statement.net"), ccy)
		for col := range b.cols {
			var rev, exp int64
			if revNet[ccy] != nil {
				rev = revNet[ccy][col]
			}
			if expNet[ccy] != nil {
				exp = expNet[ccy][col]
			}
			cells = append(cells, MoneyCell(-(rev+exp), b.moneyCcy(ccy)))
		}
		b.rows = append(b.rows, Row{Cells: cells, Kind: RowTotal})
	}
}

func (b *psBuilder) table() Table {
	return Table{Columns: b.tableCols, Rows: b.rows}
}

// preorderAccounts returns leaf-account ids in tree pre-order (children in the store's sort
// order), for a stable row order matching the chart of accounts. Placeholders are included
// in the walk but the caller filters them out per row.
func preorderAccounts(tree []treeNode, roots []int64) []AccountID {
	children := make(map[int64][]int64)
	for _, r := range tree {
		if r.HasPar {
			children[r.ParentID] = append(children[r.ParentID], r.ID)
		}
	}
	var out []AccountID
	var walk func(id int64)
	walk = func(id int64) {
		out = append(out, AccountID(id))
		for _, c := range children[id] {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return out
}
