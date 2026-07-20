package reports

import (
	"context"
	"sort"
)

// ProgramStatementReportID is the id (URL slug + registry key) of the program statement
// report (p15.10): the DECISION-MAKER view of revenue and expense per PROGRAM (D24), and
// the source p15.11 draws 990 Part III (program service accomplishments) from.
//
// LAYOUT (p31, transposed from the p29.15 side-by-side columns): the report is a
// COLLAPSIBLE PROGRAM TREE stacked VERTICALLY as ROWS — each program in tree pre-order is a
// HEADER row (its rolled net) that SPANS its child content: its own Revenue/Expense ACCOUNT
// subtree (indented one level) and then its CHILD PROGRAMS (indented one level, recursively).
// A single money column (Amount) carries the figure for the program the block belongs to.
// This replaces the earlier one-column-per-program matrix: the owner asked for the programs
// to read as a vertical hierarchy ("layers of header rows where the next level up spans the
// child rows"), collapsible by hierarchy, rather than horizontally side by side. Because the
// program tree drives the rows, a program-scoped grant, the treetable collapse control, and
// "General spans Educacion + Food Pantry" all fall out of the row nesting.
//
// TWO HIERARCHIES, NESTED (p31):
//
//   - PROGRAM hierarchy (OUTER rows): programs in tree pre-order — General at depth 0, then
//     its child programs (Educacion, Food Pantry) as depth-1 blocks nested UNDER it. A parent
//     program's figures already ROLL UP its descendants (ProgramActivity, D24), so General's
//     account subtree is the whole org's program activity and the nested child blocks repeat
//     the descendant slices (an accepted redundancy — the nesting mirrors the rollup, and
//     collapsing General hides the children exactly as the old parent column subsumed them).
//   - ACCOUNT hierarchy (INNER rows): within each program block the rolled per-account
//     activity renders as the collapsible account tree (placeholder parents as roll-up
//     subtotals over indented leaves) — the p29.15 Statement-of-Activities machinery, now
//     offset one level deeper so it sits inside its program.
//
// DEPTH is a pure recursive descent (treetable's data-depth contract): a program block at
// depth D emits its header at D, its account sections + net at D+1, and each child program
// at D+1 (recursively). A parent's descendants are exactly the contiguous deeper rows, so
// treetable's collapse/expand (Tree: true) folds a program's whole subtree — its accounts
// AND its child programs — with no extra markup.
//
// TWO PARAMETERIZATIONS, selected by the report-specific PROGRAM param:
//
//   - COMPARATIVE (the default, no program chosen): the WHOLE program tree, General as the
//     depth-0 root whose block carries the org-wide activity, its children nested beneath.
//   - SINGLE SUBTREE (a program chosen via ?program=): that program's subtree alone, the
//     chosen program as the depth-0 root.
//
// CONVERSION (p26.54): NATIVE by DEFAULT — the statement reads in native currency, GROUPED
// BY CURRENCY (each currency is a full program-tree walk, p29.15). Native mode cannot sum a
// mixed-currency parent into one figure, so the currency dimension is the OUTERMOST grouping:
// within a single currency block every account (leaf or placeholder) is single-valued, so the
// collapsible account tree renders cleanly. An OPTIONAL target-currency selector
// (ParamsSpec.CurrencyOptional, the leading "— native —" choice) converts the whole matrix to
// one currency at the period-end CLOSING rate (D12): the Currency column drops and there is a
// SINGLE program-tree walk (one converted figure per (account, program)). A converted cell
// sums across an account's native currencies, so (like the trial balance / income statement)
// it is not drillable; the native default keeps the per-currency drills.
//
// DRILL-DOWN (p15.3d): each account LEAF amount cell drills (DrillPeriod) to its
// contributing splits — filtered by program + account + period + native currency. A ROLLUP
// cell (a program WITH descendants, e.g. General) drills across the program SUBTREE via the
// program-SET drill (Drill.ProgramIDs = the program's descendants incl. self), unioning the
// per-program split sets so the drilled native sum reconciles to the rolled figure; a LEAF
// program's cell uses the single ProgramID. Account-PLACEHOLDER subtotal rows and program
// HEADER rows are NOT drillable (a rollup spans many leaves), matching SoA.
//
// PERIOD (from/to), revenue+expense accounts only (only R/E splits carry a program, D24).
const ProgramStatementReportID = "program_statement"

// registerProgramStatement registers the program statement (p15.10) into reg under the
// "programs" group. It offers the period (from/to) and the report-specific PROGRAM
// selector; the shared web params form renders both from the ParamsSpec.
func registerProgramStatement(reg *Registry) {
	reg.Register(Report{
		ID:         ProgramStatementReportID,
		TitleKey:   "reports.program_statement.title",
		Group:      "programs",
		ParamsSpec: ParamsSpec{Period: true, Program: true, CurrencyOptional: true},
		Run:        runProgramStatement,
		// p31: BOTH hierarchies are collapsible rows now — the OUTER program tree and the
		// INNER account tree share one pre-order data-depth sequence, enhanced by the shared
		// treetable control (Tree: true). A program header folds its whole subtree (its
		// accounts + its child programs); an account placeholder folds its leaves.
		Tree: true,
		// p27.4: rows are keyed by program, so a program-scoped grant filters them to the
		// granted subtree (resolveParams -> Params.ProgramScope, honored below).
		ProgramDimensioned: true,
	})
}

// progNode is one program in the report's tree: its id, display name (a stored proper noun
// surfaced verbatim), its DESCENDANT set (self + children, D24) a rollup cell's drill spans,
// and its child program nodes (pre-order). When the program is a LEAF (descendants ==
// {self}), a cell drills with the single ProgramID; otherwise with the program SET.
type progNode struct {
	id          ProgramID
	name        string
	descendants []ProgramID // self + all descendants (for the rollup-cell drill)
	children    []*progNode // child programs (pre-order), nested one depth deeper
}

// runProgramStatement computes the program statement Table (p15.10, p31). It reads the
// rolled per-(program, account) native activity, builds the program tree (the whole tree for
// the comparative view, or the chosen program's subtree), and — per currency in native mode,
// or once in converted mode — walks the program tree in pre-order: each program a HEADER row
// spanning its rolled Revenue/Expense account tree and its nested child programs.
func runProgramStatement(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	scope := Scope{Sub: p.Scope}

	// p26.54: NATIVE by default (per-currency blocks, the documented default) OR CONVERTED
	// to a chosen target currency (one block, no currency column) when the optional currency
	// param is set. converted keys off a non-empty target.
	converted := p.TargetCurrency != ""

	// Rolled per-(program, account) activity: a parent program's cells fold in its
	// descendants (ProgramActivity does the tree rollup, D24). Native (RateNone) => per
	// currency; converted (RateClosing at the period end, D12) => one target-currency CurAmt
	// per (program, account), summed across the account's native currencies.
	opts := ConvertOpts{Mode: RateNone}
	if converted {
		opts = ConvertOpts{To: p.TargetCurrency, Mode: RateClosing}
	}
	act, err := tk.ProgramActivity(ctx, scope, p.From, p.To, opts)
	if err != nil {
		return Table{}, err
	}

	roots, err := programTree(ctx, tk, p)
	if err != nil {
		return Table{}, err
	}

	// Account name (resolved for lang, D5), type, and tree order.
	storeTree, err := tk.Store().Tree(ctx, p.LangOr(), nil)
	if err != nil {
		return Table{}, err
	}
	tree := toTreeNodes(storeTree)

	b := &psBuilder{tk: tk, p: p, tree: tree, converted: converted}
	// Index the account tree ONCE (constant for the run); section()/sectionSum() reuse it
	// instead of rebuilding per program × currency (p-perf).
	b.children, b.roots, b.isPlaceholder, b.name, b.depth, b.typeOf = indexTree(tree)
	b.columns()

	// The set of currency blocks: every currency that appears anywhere in the rolled
	// activity (native), or a single synthetic key (converted) so the whole matrix is one
	// block. Determined ONCE over the whole activity so a program with no activity in a
	// currency still nests correctly under one that does.
	for _, ccy := range b.currencies(act) {
		for _, root := range roots {
			b.programBlock(ccy, act, root, 0)
		}
	}

	return b.table(), nil
}

// programTree resolves the report's program tree: for the COMPARATIVE view (no program
// chosen) the whole tree rooted at the seeded root; for the SINGLE view (a program chosen)
// the subtree rooted at that program. Each node carries its descendant set (self + subtree)
// for the rollup-cell drill (D24), and its child nodes in pre-order.
func programTree(ctx context.Context, tk *Toolkit, p Params) ([]*progNode, error) {
	rows, err := tk.Store().ProgramTree(ctx)
	if err != nil {
		return nil, err
	}
	// Descendants (self + subtree) per program, for the rollup-cell drill.
	descOf := func(id ProgramID) ([]ProgramID, error) {
		drows, derr := tk.Store().ProgramDescendants(ctx, id)
		if derr != nil {
			return nil, derr
		}
		ids := make([]ProgramID, 0, len(drows))
		for _, r := range drows {
			ids = append(ids, r.ID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		return ids, nil
	}

	// Build a node per program, then wire children by ParentID. ProgramTree is pre-order,
	// so appending children preserves the chart's sort order.
	byID := make(map[ProgramID]*progNode, len(rows))
	for _, r := range rows {
		desc, derr := descOf(r.ID)
		if derr != nil {
			return nil, derr
		}
		byID[r.ID] = &progNode{id: r.ID, name: r.Name, descendants: desc}
	}
	var roots []*progNode
	for _, r := range rows {
		n := byID[r.ID]
		if r.ParentID.Valid && ProgramID(r.ParentID.Int64) != r.ID {
			parent := byID[ProgramID(r.ParentID.Int64)]
			parent.children = append(parent.children, n)
		} else {
			roots = append(roots, n)
		}
	}

	// SINGLE view: return just the chosen program's subtree (its node, children intact).
	if p.Program != 0 {
		if n, ok := byID[p.Program]; ok {
			return []*progNode{n}, nil
		}
		return nil, nil
	}
	return roots, nil
}

// progCol is one program in tree PRE-ORDER as a FLAT list entry: its id, display name, and
// DESCENDANT set (self + children, D24) a rollup cell's drill spans. The program statement
// itself now renders the program TREE (progNode, nested); this flat shape is retained for
// Form 990 Part III (form_990.go), which lists each program group linearly.
type progCol struct {
	id          ProgramID
	name        string
	descendants []ProgramID
}

// programColumns returns every program in tree pre-order as a flat progCol list, each
// carrying its descendant set (for the rollup-cell drill). Used by Form 990 Part III to emit
// one program group per row; the program statement uses programTree (the nested form).
func programColumns(ctx context.Context, tk *Toolkit, _ Params) ([]progCol, error) {
	tree, err := tk.Store().ProgramTree(ctx)
	if err != nil {
		return nil, err
	}
	cols := make([]progCol, 0, len(tree))
	for _, n := range tree {
		drows, derr := tk.Store().ProgramDescendants(ctx, n.ID)
		if derr != nil {
			return nil, derr
		}
		ids := make([]ProgramID, 0, len(drows))
		for _, r := range drows {
			ids = append(ids, r.ID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		cols = append(cols, progCol{id: n.ID, name: n.Name, descendants: ids})
	}
	return cols, nil
}

// psBuilder accumulates the program-statement rows. The leading columns are Program/Account,
// Currency (native only), then a single Amount column; every row emits exactly one money
// cell (for the program whose block it belongs to).
type psBuilder struct {
	tk        *Toolkit
	p         Params
	tree      []treeNode
	converted bool

	// Indexed account tree, computed ONCE from `tree` (constant for the run) and reused by
	// every section()/sectionSum() call (p-perf). indexTree rebuilds children/roots/
	// placeholder/name/depth/type maps with a per-node depth walk, so re-deriving it per
	// (program × currency × section) was an O(programs·currencies) redundant rebuild.
	children      map[AccountID][]AccountID
	roots         []AccountID
	isPlaceholder map[AccountID]bool
	name          map[AccountID]string
	depth         map[AccountID]int
	typeOf        map[AccountID]string

	tableCols []Column
	rows      []Row
}

// psConvertedKey is the synthetic currency key used for the single converted block (p26.54):
// all converted amounts share it so currencies()/programBlock() treat the whole matrix as one
// block.
const psConvertedKey = "\x00converted"

// columns builds the column set: Program/Account, Currency (native only), then a single
// Amount column.
func (b *psBuilder) columns() {
	b.tableCols = append(b.tableCols, Column{HeaderKey: "reports.program_statement.col.program_account", Align: AlignLeft})
	if !b.converted {
		// Native mode carries a Currency column (per-currency blocks); the converted view
		// (single target currency) drops it (p26.54).
		b.tableCols = append(b.tableCols, Column{HeaderKey: "reports.program_statement.col.currency", Align: AlignLeft})
	}
	b.tableCols = append(b.tableCols, Column{HeaderKey: "reports.program_statement.col.amount", Align: AlignRight})
}

// currencies returns the sorted currency-block keys: every currency appearing anywhere in
// the rolled activity (native), or the single synthetic key (converted).
func (b *psBuilder) currencies(act map[ProgramID]map[AccountID][]CurAmt) []string {
	if b.converted {
		return []string{psConvertedKey}
	}
	seen := make(map[string]bool)
	for _, byAcct := range act {
		for _, byCcy := range byAcct {
			for _, a := range byCcy {
				seen[a.Currency] = true
			}
		}
	}
	ccys := make([]string, 0, len(seen))
	for c := range seen {
		ccys = append(ccys, c)
	}
	sort.Strings(ccys)
	return ccys
}

// leafAmt buckets one program's rolled activity for one currency block into
// leafAmt[account] = raw net-debit minor. In converted mode every (account) figure lands
// under the synthetic key so the whole account is single-valued.
func (b *psBuilder) leafAmt(ccy string, act map[ProgramID]map[AccountID][]CurAmt, prog ProgramID) map[AccountID]int64 {
	la := make(map[AccountID]int64)
	for acct, byCcy := range act[prog] {
		for _, a := range byCcy {
			key := a.Currency
			if b.converted {
				key = psConvertedKey
			}
			if key == ccy {
				la[acct] += a.Minor
			}
		}
	}
	return la
}

// programBlock emits one program's block within a currency: a HEADER row at depth (its
// rolled net for the currency), then its Revenue + Expense account trees and Net line at
// depth+1, then each CHILD program at depth+1 (recursively). A program with NO activity in
// this currency AND no descendant activity contributes nothing (the block is skipped so
// empty branches drop out, matching the SoA rule).
func (b *psBuilder) programBlock(ccy string, act map[ProgramID]map[AccountID][]CurAmt, node *progNode, depth int) {
	la := b.leafAmt(ccy, act, node.id)

	// Does this program's block carry ANY visible content in this currency — its own rolled
	// activity, or a descendant program's? An all-empty branch is skipped entirely.
	if !b.hasActivity(ccy, act, node) {
		return
	}

	// Revenue + Expense sections (leaves-only raw net-debit sums returned for the net line).
	// They are pre-walked (no emission) to know the program's rolled net BEFORE the header
	// row, so the collapsed one-liner carries an informative figure.
	revNet := b.sectionSum(la, "revenue")
	expNet := b.sectionSum(la, "expense")
	net := -(revNet + expNet) // revenue positive = −revNet; net = −revNet − expNet

	// Header row: the program name at this depth, its rolled net for the currency. A program
	// header is a subtotal tier (a rollup over its subtree), not drillable.
	b.programHeader(ccy, node.name, net, depth)

	// The program's own rolled account tree, indented one level under the header.
	b.section(ccy, la, node, "revenue", "reports.program_statement.section.revenue",
		"reports.program_statement.total.revenue", -1, depth+1)
	b.section(ccy, la, node, "expense", "reports.program_statement.section.expenses",
		"reports.program_statement.total.expenses", +1, depth+1)
	b.netLine(ccy, net, depth+1)

	// Child programs nest one level deeper (recursively) — General spans Educacion + Food
	// Pantry, each its own collapsible sub-block.
	for _, child := range node.children {
		b.programBlock(ccy, act, child, depth+1)
	}
}

// hasActivity reports whether node's block would render any row in this currency: its own
// rolled activity is nonzero for the currency, OR some descendant program has activity.
func (b *psBuilder) hasActivity(ccy string, act map[ProgramID]map[AccountID][]CurAmt, node *progNode) bool {
	if len(b.leafAmt(ccy, act, node.id)) > 0 {
		return true
	}
	for _, child := range node.children {
		if b.hasActivity(ccy, act, child) {
			return true
		}
	}
	return false
}

// sectionSum returns the RAW net-debit sum (leaves of type typ only) over la — used to
// derive the program's net BEFORE emitting rows (so the header carries the net).
func (b *psBuilder) sectionSum(la map[AccountID]int64, typ string) int64 {
	var sum int64
	for acct, v := range la {
		if !b.isPlaceholder[acct] && b.typeOf[acct] == typ {
			sum += v
		}
	}
	return sum
}

// section renders one R/E section (type "revenue"|"expense") for ONE program within a
// currency block as a COLLAPSIBLE ACCOUNT TREE (p29.15/p31, offset by depthOffset so it
// nests under its program header): a section header, then the account tree walked pre-order —
// placeholder parents as SUBTOTAL rows carrying their subtree's sum (this currency only),
// leaves WITH ACTIVITY as indented data rows — then a section total row. `sign` is the display
// sign (−1 revenue, +1 expense). All rows are indented by depthOffset (the program's depth+1).
func (b *psBuilder) section(ccy string, la map[AccountID]int64, node *progNode, typ, sectionKey, totalKey string, sign int64, depthOffset int) {
	children, roots, isPlaceholder, name, depth, typeOf := b.children, b.roots, b.isPlaceholder, b.name, b.depth, b.typeOf

	// Per-node subtotal (this currency), plus whether a node's subtree carries any in-scope
	// activity of this type — an empty branch drops out entirely (SoA rule).
	colSum := make(map[AccountID]int64)
	inSection := make(map[AccountID]bool)
	var fold func(id AccountID) int64
	fold = func(id AccountID) int64 {
		var sum int64
		if !isPlaceholder[id] {
			if typeOf[id] == typ {
				if v, ok := la[id]; ok && v != 0 {
					inSection[id] = true
					sum = v
				}
			}
			colSum[id] = sum
			return sum
		}
		for _, c := range children[id] {
			sum += fold(c)
			if inSection[c] {
				inSection[id] = true
			}
		}
		colSum[id] = sum
		return sum
	}
	for _, r := range roots {
		fold(r)
	}

	// A section with no activity for this program+currency emits nothing (not even a header).
	anyInSection := false
	for _, r := range roots {
		if inSection[r] {
			anyInSection = true
			break
		}
	}
	if !anyInSection {
		return
	}

	b.headerRow(ccy, sectionKey, depthOffset)

	var sectionSum int64
	var walk func(id AccountID)
	walk = func(id AccountID) {
		if !inSection[id] {
			return
		}
		// The account tree sits at depthOffset + its own tree depth, so a root R/E
		// placeholder ("Revenue"/"Expenses") is a PEER of the section label (both at
		// depthOffset), the section label being a non-parent sibling — matching the old
		// side-by-side layout, just shifted into the program's depth band.
		d := depthOffset + depth[id]
		if isPlaceholder[id] {
			b.subtotalRow(ccy, TextCell(name[id]), sign*colSum[id], d)
			for _, c := range children[id] {
				walk(c)
			}
			return
		}
		b.leafRow(ccy, id, name[id], la[id], node, sign, d)
		sectionSum += colSum[id]
	}
	for _, r := range roots {
		walk(r)
	}

	b.totalRow(ccy, totalKey, sign*sectionSum, depthOffset)
}

// leafRow emits one account leaf's row for the current program: name, currency (native
// only), the money cell (native, this currency). A cell with activity drills (leaf program:
// single ProgramID; rollup program: the subtree set); a zero cell is left non-drillable.
func (b *psBuilder) leafRow(ccy string, acct AccountID, name string, raw int64, node *progNode, sign int64, depth int) {
	cell := MoneyCell(sign*raw, b.moneyCcy(ccy))
	// Native cells drill to their contributing splits; a CONVERTED cell sums across an
	// account's native currencies (p26.54) so it is not drillable.
	if !b.converted {
		if d := b.cellDrill(acct, ccy, node, raw); d != nil {
			cell = cell.WithDrill(d)
		}
	}
	cells := append(b.leadCells(TextCell(name), ccy), cell)
	b.rows = append(b.rows, Row{Cells: cells, Indent: depth, Kind: RowData})
}

// subtotalRow emits a placeholder-parent roll-up row: name (a stored account name), the
// money cell (its subtree sum for this currency), at the given depth. Not drillable.
func (b *psBuilder) subtotalRow(ccy string, nameCell Cell, v int64, depth int) {
	cells := append(b.leadCells(nameCell, ccy), MoneyCell(v, b.moneyCcy(ccy)))
	b.rows = append(b.rows, Row{Cells: cells, Indent: depth, Kind: RowSubtotal})
}

// programHeader emits a program's HEADER row: the program name, its rolled net for the
// currency, at the program's depth. It is the OUTER-tree parent (a subtotal tier over its
// whole subtree — accounts + child programs) and is NOT drillable (a rollup spans many
// leaves).
func (b *psBuilder) programHeader(ccy, name string, net int64, depth int) {
	cells := append(b.leadCells(TextCell(name), ccy), MoneyCell(net, b.moneyCcy(ccy)))
	b.rows = append(b.rows, Row{Cells: cells, Indent: depth, Kind: RowSubtotal})
}

// cellDrill builds the DrillPeriod filter for one program×account×currency LEAF cell. A
// ROLLUP program (descendants beyond self) drills across its subtree via the program SET
// (Drill.ProgramIDs); a LEAF program uses the single ProgramID. A cell with NO activity
// (raw == 0) is left non-drillable.
func (b *psBuilder) cellDrill(acct AccountID, ccy string, node *progNode, raw int64) *Drill {
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
	if len(node.descendants) > 1 {
		d.ProgramIDs = node.descendants // rollup: union the subtree's per-program splits
	} else {
		id := node.id
		d.ProgramID = &id // leaf: single program
	}
	return d
}

// leadCells builds a row's leading cells: the name/label cell, plus (native mode only) the
// currency-code cell. In converted mode the Currency column is dropped (p26.54).
func (b *psBuilder) leadCells(nameCell Cell, ccy string) []Cell {
	cells := make([]Cell, 0, 3)
	cells = append(cells, nameCell)
	if !b.converted {
		cells = append(cells, TextCell(ccy))
	}
	return cells
}

// moneyCcy is the currency code a money cell carries: the row's native currency in native
// mode, or the target currency in converted mode.
func (b *psBuilder) moneyCcy(ccy string) string {
	if b.converted {
		return b.p.TargetCurrency
	}
	return ccy
}

// headerRow appends a section heading row (Revenue/Expenses) at the given depth: the section
// label + a blank money cell. A data row (not a tree parent — the placeholder subtotal that
// follows is the parent).
func (b *psBuilder) headerRow(ccy, key string, depth int) {
	cells := append(b.leadCells(LabelCell(key), ccy), BlankMoneyCell())
	b.rows = append(b.rows, Row{Cells: cells, Indent: depth, Kind: RowData})
}

// totalRow appends a SECTION-total row with the section's leaf sum (signed) at the given
// depth: a RowSectionTotal (p30.10), ranked ABOVE the placeholder-parent RowSubtotal rollups
// and BELOW the program header/net, so the total tiers read distinctly.
func (b *psBuilder) totalRow(ccy, key string, v int64, depth int) {
	cells := append(b.leadCells(LabelCell(key), ccy), MoneyCell(v, b.moneyCcy(ccy)))
	b.rows = append(b.rows, Row{Cells: cells, Indent: depth, Kind: RowSectionTotal})
}

// netLine appends the net row for this program+currency at the given depth: net = revenue −
// expenses, already computed by the caller (revenue shown positive = −revNet; net = −revNet −
// expNet, the income-statement convention).
func (b *psBuilder) netLine(ccy string, net int64, depth int) {
	cells := append(b.leadCells(LabelCell("reports.program_statement.net"), ccy), MoneyCell(net, b.moneyCcy(ccy)))
	b.rows = append(b.rows, Row{Cells: cells, Indent: depth, Kind: RowTotal})
}

func (b *psBuilder) table() Table {
	return Table{Columns: b.tableCols, Rows: b.rows}
}
