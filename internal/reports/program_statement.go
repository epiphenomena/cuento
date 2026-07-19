package reports

import (
	"context"
	"sort"
)

// ProgramStatementReportID is the id (URL slug + registry key) of the program statement
// report (p15.10): the DECISION-MAKER view of revenue and expense per PROGRAM (D24), and
// the source p15.11 draws 990 Part III (program service accomplishments) from. Rows are
// natural ACCOUNTS grouped into a Revenue then an Expense section, rendered as the
// COLLAPSIBLE account tree (p29.15, reusing the Statement-of-Activities machinery): a
// placeholder parent (Revenue/Expenses, or any grouping account) is a roll-up SUBTOTAL row
// over its indented child accounts. Every program figure is ALSO a PROGRAM-TREE ROLLUP: a
// parent program's cell folds in its descendant programs' activity (ProgramActivity, D24),
// so the root program "General" carries the whole org's program activity.
//
// TWO HIERARCHIES, SUBTOTALED BOTH WAYS (p29.15):
//
//   - ACCOUNT hierarchy (report BODY): parent/placeholder accounts appear as roll-up
//     subtotal rows over their indented children, collapsible via the shared treetable
//     control (Tree: true, data-depth = account tree depth) — exactly like the Statement
//     of Activities. A subtotal cell (program column j) is Σ of its descendant leaves'
//     figures in program column j.
//   - PROGRAM hierarchy (COLUMNS): the columns are the programs in tree PRE-ORDER, each
//     parent-program column already rolling up its descendants (ProgramActivity), so the
//     column ordering reads the program tree top-down and a parent column subtotals its
//     child columns by construction.
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
// GROUPED BY CURRENCY (each currency is a full Revenue/Expense/Net block, p29.15). Native
// mode cannot sum a mixed-currency parent into one figure, so the currency dimension is the
// OUTERMOST grouping: within a single currency block every account (leaf or placeholder) is
// single-valued, so the collapsible account tree renders cleanly. An OPTIONAL
// target-currency selector (ParamsSpec.CurrencyOptional, the leading "— native —" choice)
// converts the whole matrix to one currency at the period-end CLOSING rate (D12): the
// Currency column drops and there is a SINGLE Revenue/Expense/Net block (one converted
// figure per (account, program)). A converted cell sums across an account's native
// currencies, so (like the trial balance / income statement) it is not drillable; the
// native default keeps the per-currency drills.
//
// DRILL-DOWN (p15.3d): each program×account LEAF amount cell drills (DrillPeriod) to its
// contributing splits — filtered by program + account + period + native currency. A ROLLUP
// cell (a program WITH descendants, e.g. General) drills across the program SUBTREE via the
// program-SET drill (Drill.ProgramIDs = the program's descendants incl. self), unioning the
// per-program split sets so the drilled native sum reconciles to the rolled figure; a LEAF
// program's cell uses the single ProgramID. Account-PLACEHOLDER subtotal rows are NOT
// drillable (a rollup spans many leaves), matching SoA.
//
// PERIOD (from/to), revenue+expense accounts only (only R/E splits carry a program, D24).
const ProgramStatementReportID = "program_statement"

// registerProgramStatement registers the program statement (p15.10) into reg under the
// "programs" group. It offers the period (from/to) and the report-specific PROGRAM
// selector; the shared web params form renders both from the ParamsSpec. No currency
// control — the statement is native (per-currency blocks), like p15.8/p15.9.
func registerProgramStatement(reg *Registry) {
	reg.Register(Report{
		ID:         ProgramStatementReportID,
		TitleKey:   "reports.program_statement.title",
		Group:      "programs",
		ParamsSpec: ParamsSpec{Period: true, Program: true, CurrencyOptional: true},
		Run:        runProgramStatement,
		// p29.15: the account hierarchy is a collapsible tree (placeholder parents as
		// roll-up subtotals over indented leaves), reusing the p26.25 treetable control —
		// data-depth = account tree depth, exactly like the Statement of Activities.
		Tree: true,
		// p29.11: the per-program comparative statement fans into one column per program
		// -> render full-viewport-width so none truncate/scroll.
		WideMatrix: true,
		// p27.4: rows are keyed by program, so a program-scoped grant filters them to
		// the granted subtree (resolveParams -> Params.ProgramScope, honored below).
		ProgramDimensioned: true,
	})
}

// progCol is one comparative program column: the program id, its display name (a stored
// proper noun surfaced verbatim as the column header), and the DESCENDANT set (self +
// children, D24) a rollup cell's drill spans. When the program is a LEAF (descendants ==
// {self}), a cell drills with the single ProgramID; otherwise with the program SET.
type progCol struct {
	id          ProgramID
	name        string
	descendants []ProgramID // self + all descendants (for the rollup-cell drill)
}

// runProgramStatement computes the program statement Table (p15.10, p29.15). It reads the
// rolled per-(program, account) native activity, resolves the program columns (all programs
// in tree pre-order for the comparative view, or the single chosen program's subtree), and
// renders — per currency in native mode, or once in converted mode — the Revenue then
// Expense sections as a COLLAPSIBLE ACCOUNT TREE (placeholder parents as subtotals over
// indented leaves), closing each block with a net-per-program line.
func runProgramStatement(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	scope := Scope{Sub: p.Scope}

	// p26.54: NATIVE by default (per-currency blocks, the documented default) OR CONVERTED
	// to a chosen target currency (one block, no currency column) when the optional
	// currency param is set. converted keys off a non-empty target.
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

	b := &psBuilder{tk: tk, p: p, cols: cols, tree: tree, converted: converted}
	b.columns()

	// leafAmt[currency][account][col] = raw net-debit minor for that (currency, account,
	// program-column). In converted mode there is a single synthetic currency key (the
	// target) so the whole matrix renders as ONE block.
	leafAmt, ccys := b.index(act)

	// One Revenue/Expense/Net block per currency (native), or a single block (converted).
	for _, ccy := range ccys {
		la := leafAmt[ccy]
		// Revenue section: revenue accounts, net-debit NEGATIVE (a credit), shown +inflow
		// (sign −1). Expense section: net-debit POSITIVE (a debit), shown as-is (sign +1).
		revNet := b.section(ccy, la, "revenue", "reports.program_statement.section.revenue",
			"reports.program_statement.total.revenue", -1)
		expNet := b.section(ccy, la, "expense", "reports.program_statement.section.expenses",
			"reports.program_statement.total.expenses", +1)

		// Net per program for this currency = revenue − expenses. revNet/expNet are the raw
		// net-debit column sums (leaves only): net surplus = −(revNet + expNet), mirroring
		// the income statement (revenue positive = −revNet; net = −revNet − expNet).
		b.netLine(ccy, revNet, expNet)
	}

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
	descOf := func(id ProgramID) ([]ProgramID, error) {
		rows, err := tk.Store().ProgramDescendants(ctx, id)
		if err != nil {
			return nil, err
		}
		ids := make([]ProgramID, 0, len(rows))
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
// Currency (native only); then one money column per program. It renders the account tree
// per currency block (native) or once (converted).
type psBuilder struct {
	tk        *Toolkit
	p         Params
	cols      []progCol
	tree      []treeNode
	converted bool

	tableCols []Column
	rows      []Row
}

// convCcy is the synthetic currency key used for the single converted block (p26.54): all
// converted amounts share it so index()/section() treat the whole matrix as one block.
const psConvertedKey = "\x00converted"

// columns builds the column set: Account, Currency (native only), then one per program
// (its name as a verbatim header — a stored proper noun surfaced through i18n.T's
// passthrough, the same mechanism the income statement uses for its period headers).
func (b *psBuilder) columns() {
	b.tableCols = append(b.tableCols, Column{HeaderKey: "reports.program_statement.col.account", Align: AlignLeft})
	if !b.converted {
		// Native mode carries a Currency column (per-currency blocks); the converted view
		// (single target currency) drops it (p26.54).
		b.tableCols = append(b.tableCols, Column{HeaderKey: "reports.program_statement.col.currency", Align: AlignLeft})
	}
	for _, c := range b.cols {
		b.tableCols = append(b.tableCols, Column{HeaderKey: c.name, Align: AlignRight})
	}
}

// index buckets the rolled activity into leafAmt[currency][account][col] = raw net-debit
// minor and returns the sorted list of currency keys (block order). In native mode a
// currency key is the account's native currency; in converted mode a single synthetic key
// holds every (account, col) converted figure, so the whole matrix is one block.
func (b *psBuilder) index(act map[ProgramID]map[AccountID][]CurAmt) (map[string]map[AccountID][]int64, []string) {
	leafAmt := make(map[string]map[AccountID][]int64)
	add := func(ccy string, acct AccountID, col int, minor int64) {
		if leafAmt[ccy] == nil {
			leafAmt[ccy] = make(map[AccountID][]int64)
		}
		if leafAmt[ccy][acct] == nil {
			leafAmt[ccy][acct] = make([]int64, len(b.cols))
		}
		leafAmt[ccy][acct][col] += minor
	}
	for col, c := range b.cols {
		for acct, byCcy := range act[c.id] {
			for _, a := range byCcy {
				key := a.Currency
				if b.converted {
					key = psConvertedKey
				}
				add(key, acct, col, a.Minor)
			}
		}
	}
	ccys := make([]string, 0, len(leafAmt))
	for ccy := range leafAmt {
		ccys = append(ccys, ccy)
	}
	sort.Strings(ccys)
	return leafAmt, ccys
}

// section renders one R/E section (type "revenue"|"expense") within a currency block as a
// COLLAPSIBLE ACCOUNT TREE (p29.15, mirroring the Statement of Activities): a section
// header, then the account tree walked pre-order — placeholder parents as SUBTOTAL rows
// carrying their subtree's per-column sums (this currency only), leaves WITH ACTIVITY as
// indented data rows — then a section total row. `sign` is the display sign applied to the
// raw net-debit figures (−1 revenue, +1 expense). It returns the section's RAW net-debit
// per-column sums (LEAVES only, so the caller derives the net line without double-counting).
func (b *psBuilder) section(ccy string, la map[AccountID][]int64, typ, sectionKey, totalKey string, sign int64) []int64 {
	children, roots, isPlaceholder, name, depth, typeOf := indexTree(b.tree)
	n := len(b.cols)

	// Per-node per-column subtotal (this currency), plus whether a node's subtree carries
	// any in-scope activity of this type — an empty branch drops out entirely (SoA rule).
	colSum := make(map[AccountID][]int64)
	inSection := make(map[AccountID]bool)
	nonzero := func(v []int64) bool {
		for _, x := range v {
			if x != 0 {
				return true
			}
		}
		return false
	}
	var fold func(id AccountID) []int64
	fold = func(id AccountID) []int64 {
		sums := make([]int64, n)
		if !isPlaceholder[id] {
			if typeOf[id] == typ {
				if leaf, ok := la[AccountID(id)]; ok && nonzero(leaf) {
					inSection[AccountID(id)] = true
					copy(sums, leaf)
				}
			}
			colSum[AccountID(id)] = sums
			return sums
		}
		for _, c := range children[id] {
			cs := fold(c)
			for i := range sums {
				sums[i] += cs[i]
			}
			if inSection[AccountID(c)] {
				inSection[AccountID(id)] = true
			}
		}
		colSum[AccountID(id)] = sums
		return sums
	}
	for _, r := range roots {
		fold(r)
	}

	b.headerRow(ccy, sectionKey)

	sectionSum := make([]int64, n)
	var walk func(id AccountID)
	walk = func(id AccountID) {
		if !inSection[AccountID(id)] {
			return
		}
		if isPlaceholder[id] {
			b.subtotalRow(ccy, TextCell(name[id]), applySign(colSum[AccountID(id)], sign), depth[id])
			for _, c := range children[id] {
				walk(c)
			}
			return
		}
		b.leafRow(ccy, AccountID(id), name[id], la[AccountID(id)], sign, depth[id])
		for i := range sectionSum {
			sectionSum[i] += colSum[AccountID(id)][i]
		}
	}
	for _, r := range roots {
		walk(r)
	}

	b.totalRow(ccy, totalKey, applySign(sectionSum, sign))
	return sectionSum // RAW net-debit leaf sums (caller flips once for the net line)
}

// leafRow emits one account leaf's row: name, currency (native only), one money cell per
// program column (native, this currency). A cell with activity drills (leaf: single
// ProgramID; rollup program: the subtree set); a zero cell is left non-drillable.
func (b *psBuilder) leafRow(ccy string, acct AccountID, name string, raw []int64, sign int64, depth int) {
	cells := b.leadCells(TextCell(name), ccy)
	for col, c := range b.cols {
		cell := MoneyCell(sign*raw[col], b.moneyCcy(ccy))
		// Native cells drill to their contributing splits; a CONVERTED cell sums across an
		// account's native currencies (p26.54) so it is not drillable.
		if !b.converted {
			if d := b.cellDrill(acct, ccy, c, raw[col]); d != nil {
				cell = cell.WithDrill(d)
			}
		}
		cells = append(cells, cell)
	}
	b.rows = append(b.rows, Row{Cells: cells, Indent: depth, Kind: RowData})
}

// subtotalRow emits a placeholder-parent roll-up row: name (a stored account name), one
// money cell per program column (its subtree sum for this currency), at the parent's tree
// depth. Not drillable (a rollup spans many leaves), matching SoA.
func (b *psBuilder) subtotalRow(ccy string, nameCell Cell, cols []int64, depth int) {
	cells := b.leadCells(nameCell, ccy)
	for _, v := range cols {
		cells = append(cells, MoneyCell(v, b.moneyCcy(ccy)))
	}
	b.rows = append(b.rows, Row{Cells: cells, Indent: depth, Kind: RowSubtotal})
}

// cellDrill builds the DrillPeriod filter for one program×account×currency LEAF cell. A
// ROLLUP program (descendants beyond self) drills across its subtree via the program SET
// (Drill.ProgramIDs); a LEAF program uses the single ProgramID. A cell with NO activity
// (raw == 0) is left non-drillable.
func (b *psBuilder) cellDrill(acct AccountID, ccy string, c progCol, raw int64) *Drill {
	if raw == 0 {
		return nil
	}
	d := &Drill{
		Scope:      b.p.Scope,
		AccountIDs: []AccountID{acct},
		Currency:   ccy,
		Mode:       DrillPeriod,
		From:       b.p.From,
		To:         b.p.To,
	}
	if len(c.descendants) > 1 {
		d.ProgramIDs = c.descendants // rollup: union the subtree's per-program splits
	} else {
		id := c.id
		d.ProgramID = &id // leaf: single program
	}
	return d
}

// leadCells builds a row's leading cells: the name/label cell, plus (native mode only)
// the currency-code cell. In converted mode the Currency column is dropped (p26.54).
func (b *psBuilder) leadCells(nameCell Cell, ccy string) []Cell {
	cells := make([]Cell, 0, len(b.cols)+2)
	cells = append(cells, nameCell)
	if !b.converted {
		cells = append(cells, TextCell(ccy))
	}
	return cells
}

// moneyCcy is the currency code a money cell carries: the row's native currency in
// native mode, or the target currency in converted mode.
func (b *psBuilder) moneyCcy(ccy string) string {
	if b.converted {
		return b.p.TargetCurrency
	}
	return ccy
}

// headerRow appends a section heading row: the section label + blank money cells. It is a
// data row at depth 0 (not a tree parent — the placeholder subtotal that follows is).
func (b *psBuilder) headerRow(ccy, key string) {
	cells := b.leadCells(LabelCell(key), ccy)
	for range b.cols {
		cells = append(cells, BlankMoneyCell())
	}
	b.rows = append(b.rows, Row{Cells: cells, Kind: RowData})
}

// totalRow appends a SECTION-total row with the section's per-column leaf sums (signed):
// a RowSectionTotal (p30.10), ranked ABOVE the placeholder-parent RowSubtotal rollups and
// BELOW the per-currency net (RowTotal), so the three total tiers read distinctly (as on
// the statement of activities and balance sheet).
func (b *psBuilder) totalRow(ccy, key string, cols []int64) {
	cells := b.leadCells(LabelCell(key), ccy)
	for _, v := range cols {
		cells = append(cells, MoneyCell(v, b.moneyCcy(ccy)))
	}
	b.rows = append(b.rows, Row{Cells: cells, Kind: RowSectionTotal})
}

// netLine appends the net-per-program row for this currency block: net = revenue −
// expenses per column. revNet/expNet are the raw net-debit leaf column sums; the displayed
// net surplus is −(revNet + expNet) (revenue shown positive = −revNet; net = −revNet −
// expNet), the income-statement convention.
func (b *psBuilder) netLine(ccy string, revNet, expNet []int64) {
	cells := b.leadCells(LabelCell("reports.program_statement.net"), ccy)
	for col := range b.cols {
		cells = append(cells, MoneyCell(-(revNet[col]+expNet[col]), b.moneyCcy(ccy)))
	}
	b.rows = append(b.rows, Row{Cells: cells, Kind: RowTotal})
}

func (b *psBuilder) table() Table {
	return Table{Columns: b.tableCols, Rows: b.rows}
}
