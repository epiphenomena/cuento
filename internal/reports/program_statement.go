package reports

import (
	"context"
	"sort"
)

// ProgramStatementReportID is the id (URL slug + registry key) of the program statement
// report (p15.10, redesigned p31-10a): the DECISION-MAKER view of revenue and expense as
// a MATRIX of account ROWS by functional-class / program COLUMNS.
//
// ROWS = revenue and expense ACCOUNTS in their chart HIERARCHY, grouped into a Revenue
// section and an Expenses section (like the income statement, p15.5): a placeholder parent
// account is a roll-up SUBTOTAL over its indented leaves, each section closes with a total,
// and a grand Net line foots the whole statement. Balance-sheet accounts (asset/liability/
// equity) carry no program or functional class, so they never appear. The report registers
// Tree: true, so the account rows are collapsible via the shared treetable row-collapse.
//
// COLUMNS = a two-level classification presented as STACKED header rows:
//
//		Total | Admin | Fundraising | Program services → [ program tree ]
//
//	  - Total (leading data column) = Admin + Fundraising + Σ(ROOT program columns). The
//	    program columns are a ROLLUP tree (a parent program's column already folds in its
//	    descendants), so the Total sums only the DISJOINT root programs — summing every
//	    program column would double-count the rolled-up descendants.
//	  - Admin = expense splits whose functional_class == management, aggregated across ALL
//	    programs (one column). Fundraising = functional_class == fundraising, one column.
//	  - Program services = the PROGRAM TREE (programs.parent_id), each column the splits
//	    tagged with that program (or a descendant, rolled up): expense splits with
//	    functional_class == program by program, PLUS ALL revenue by program (revenue has no
//	    functional class, D24, so every revenue cell is a program-services cell — the Admin
//	    and Fundraising cells of a revenue row are BLANK). The program column headers carry
//	    data-* attributes encoding the tree (program id, parent program id, a group-parent
//	    marker) so a follow-up (10b) can wire click-to-collapse of a program COLUMN group
//	    without restructuring; this task does NOT implement that JS.
//
// SINGLE CURRENCY: the whole matrix is converted to the report's target currency at the
// transaction-date rate (D12 RateTxnDate, ProgramMatrix), matching functional_expenses /
// the income statement — an expense/revenue FLOW is measured at the rate in force when it
// occurred, so the Admin/Fundraising/Program columns tie the functional-expenses report and
// the income statement exactly. There is NO per-currency column (the old native mode is
// dropped): a program matrix spanning multi-currency subsidiaries only reads coherently
// converted. Every cell is converted+rounded ONCE (ProgramMatrix); the row Total, the
// section subtotals, and the grand Net are footed by INT64 ADDITION of those cells.
//
// SIGN: revenue shown POSITIVE (net-debit credit ×−1), expense shown POSITIVE, Net =
// revenue − expenses — the statement-of-activities convention (matches income_statement).
//
// PARAMS: subsidiary scope, period (from/to), and target currency (mandatory now — the
// matrix is single-currency). The report-specific PROGRAM param no longer selects one
// program's block; it SCOPES the visible program COLUMNS to that program's subtree (via
// ProgramScope / programSelectionScope). Because management/fundraising splits also carry a
// program, that scope narrows the Admin/Fundraising columns too — REQUIRED so the
// Total = Admin + Fundraising + Σ(roots) identity stays consistent under scoping.
const ProgramStatementReportID = "program_statement"

// registerProgramStatement registers the program statement (p15.10, p31-10a) into reg
// under the "programs" group. It offers the period (from/to), the mandatory target
// currency, and the report-specific PROGRAM column-scope selector.
func registerProgramStatement(reg *Registry) {
	reg.Register(Report{
		ID:         ProgramStatementReportID,
		TitleKey:   "reports.program_statement.title",
		Group:      "programs",
		ParamsSpec: ParamsSpec{Period: true, Currency: true, Program: true},
		Run:        runProgramStatement,
		// p31-10a: account ROWS are the collapsible tree (Revenue/Expenses sections with
		// placeholder-parent roll-ups), enhanced by the shared treetable control.
		Tree: true,
		// p27.4: expense/revenue splits carry a program, so this is grant-subtree filterable.
		ProgramDimensioned: true,
	})
}

// psProgCol is one PROGRAM column of the matrix: the program id, its display name (a stored
// proper noun), its parent program id (0 = a root), and whether it has children (the 10b
// group-parent data-attr marker). Emitted in program tree pre-order so a parent column sits
// immediately left of its children.
type psProgCol struct {
	id      ProgramID
	parent  ProgramID // 0 = root program
	name    string
	hasKids bool // a group-parent header (data attr marker for 10b)
}

// runProgramStatement computes the program statement matrix Table (p31-10a). It reads the
// converted per-(account, column) activity (ProgramMatrix), builds the program-column tree
// (scoped to the chosen program's subtree when picked), then walks the account tree in
// pre-order emitting a Revenue section and an Expenses section: placeholder parents as
// roll-up subtotals over their indented leaves, each section a total, and a grand Net line.
func runProgramStatement(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	scope := Scope{Sub: p.Scope}

	// A program PICK scopes the visible program COLUMNS to that program's subtree — resolve
	// it to the subtree ids (intersected with any grant, p27.4) and put the result on BOTH
	// the local p and the toolkit's live Params, so ProgramMatrix's InProgramScope filter
	// narrows every column (program-services AND Admin/Fundraising) to that subtree. This
	// keeps the Total = Admin + Fundraising + Σ(roots) identity consistent under scoping.
	// No pick + no grant leaves ProgramScope empty (org-wide, unchanged). Mirrors the income
	// statement's program-filter resolution (p15.5).
	if p.Program != 0 {
		sub, err := tk.programSelectionScope(ctx, p.Program, p.ProgramScope)
		if err != nil {
			return Table{}, err
		}
		p.ProgramScope = sub
		tk.Params.ProgramScope = sub
	} else if len(p.ProgramScope) != 0 {
		tk.Params.ProgramScope = p.ProgramScope
	}

	// Converted (target, transaction-date rate) per (account, column). Admin/Fundraising
	// aggregate across programs; program columns are rolled up the program tree.
	mat, err := tk.ProgramMatrix(ctx, scope, p.From, p.To, p.TargetCurrency)
	if err != nil {
		return Table{}, err
	}

	progCols, err := programColumnTree(ctx, tk, p)
	if err != nil {
		return Table{}, err
	}

	storeTree, err := tk.Store().Tree(ctx, p.LangOr(), nil)
	if err != nil {
		return Table{}, err
	}
	tree := toTreeNodes(storeTree)

	b := &psBuilder{tk: tk, p: p, target: p.TargetCurrency, mat: mat, progCols: progCols}
	b.children, b.roots, b.isPlaceholder, b.name, b.depth, b.typeOf = indexTree(tree)
	b.pruneEmptyProgramColumns()
	b.columns()

	// Revenue then Expenses section, each the account tree walked pre-order (placeholder
	// parents as subtotals over their leaves), closing with a Net line.
	b.section("revenue", "reports.program_statement.section.revenue", "reports.program_statement.total.revenue", -1)
	b.section("expense", "reports.program_statement.section.expenses", "reports.program_statement.total.expenses", +1)
	b.netLine()

	return b.table(), nil
}

// programColumnTree resolves the program-services columns in tree pre-order: the whole
// program tree by default, or (when a program is picked / a grant scopes) only the picked
// program's subtree — the picked program as the leftmost program column, its descendants
// after it. Each column carries its descendant set (rollup-cell drill), its parent id and a
// has-children marker (the 10b column-collapse data attributes).
func programColumnTree(ctx context.Context, tk *Toolkit, p Params) ([]psProgCol, error) {
	rows, err := tk.Store().ProgramTree(ctx)
	if err != nil {
		return nil, err
	}
	kids := make(map[ProgramID]bool, len(rows))
	for _, r := range rows {
		if r.ParentID.Valid {
			kids[ProgramID(r.ParentID.Int64)] = true
		}
	}
	// The visible subtree: runProgramStatement has already resolved a program PICK (and any
	// grant) into p.ProgramScope (the subtree ids). A non-empty ProgramScope restricts the
	// visible program columns to it; empty means every program (org-wide, unscoped).
	var visible map[ProgramID]bool
	if len(p.ProgramScope) != 0 {
		visible = make(map[ProgramID]bool, len(p.ProgramScope))
		for _, id := range p.ProgramScope {
			visible[id] = true
		}
	}

	cols := make([]psProgCol, 0, len(rows))
	for _, r := range rows { // ProgramTree is pre-order
		if visible != nil && !visible[r.ID] {
			continue
		}
		var parent ProgramID
		if r.ParentID.Valid {
			parent = ProgramID(r.ParentID.Int64)
		}
		cols = append(cols, psProgCol{id: r.ID, parent: parent, name: r.Name, hasKids: kids[r.ID]})
	}
	return cols, nil
}

// psBuilder accumulates the program-statement matrix. Columns are: Account (left), Total,
// Admin, Fundraising, then one column per program (program-services). Every money cell is
// the target currency; a cell absent from the matrix renders BLANK (not a formatted zero).
type psBuilder struct {
	tk       *Toolkit
	p        Params
	target   string
	mat      map[AccountID]map[ProgramMatrixCol]int64
	progCols []psProgCol

	// Indexed account tree (income statement's machinery).
	children      map[AccountID][]AccountID
	roots         []AccountID
	isPlaceholder map[AccountID]bool
	name          map[AccountID]string
	depth         map[AccountID]int
	typeOf        map[AccountID]string

	cols []Column
	rows []Row
}

// classCols is the fixed functional-column order left of the program tree: the leading
// Total, then Admin (management) and Fundraising.
var psFunctionalCols = []ProgramMatrixCol{
	{Class: "management"},
	{Class: "fundraising"},
}

// pruneEmptyProgramColumns drops program columns whose whole subtree has no activity in the
// matrix (an all-blank column), so an unused program does not widen the statement. Because a
// parent program's column ROLLS UP its descendants, a kept child implies its parent's column
// is non-empty too — so pruning never orphans a kept child (the 10b column tree stays whole).
func (b *psBuilder) pruneEmptyProgramColumns() {
	active := func(prog ProgramID) bool {
		col := ProgramMatrixCol{Program: prog}
		for _, byCol := range b.mat {
			if byCol[col] != 0 {
				return true
			}
		}
		return false
	}
	kept := b.progCols[:0]
	for _, c := range b.progCols {
		if active(c.id) {
			kept = append(kept, c)
		}
	}
	b.progCols = kept
}

// columns builds the column set with the STACKED header groups: Account (no group), then
// Total / Admin / Fundraising (each its own single-column group so it stacks level with the
// program tree), then the program tree under the "Program services" group. Each program
// column's leaf header carries its name (verbatim) plus the 10b data attributes.
func (b *psBuilder) columns() {
	b.cols = append(b.cols, Column{HeaderKey: "reports.program_statement.col.account", Align: AlignLeft})
	// Total, Admin, Fundraising — each a single-column group so the two-row header aligns.
	b.cols = append(
		b.cols,
		Column{HeaderKey: "reports.program_statement.col.total", Align: AlignRight, Group: &ColumnGroup{GroupID: "total"}},
		Column{HeaderKey: "reports.program_statement.col.admin", Align: AlignRight, Group: &ColumnGroup{GroupID: "admin"}},
		Column{HeaderKey: "functional.fundraising", Align: AlignRight, Group: &ColumnGroup{GroupID: "fundraising"}},
	)
	// Program services group: one column per visible program, in tree pre-order.
	for _, pc := range b.progCols {
		data := map[string]string{"program": itoa(int64(pc.id))}
		if pc.parent != 0 {
			data["program-parent"] = itoa(int64(pc.parent))
		}
		if pc.hasKids {
			data["program-group"] = "1" // group-parent marker (10b column-collapse)
		}
		b.cols = append(b.cols, Column{
			HeaderText: pc.name, // stored proper noun, rendered verbatim (rule 9)
			Align:      AlignRight,
			Group: &ColumnGroup{
				Key:     "reports.program_statement.group.program_services",
				GroupID: "program_services",
				Data:    data,
			},
		})
	}
}

// numDataCols returns the count of money columns (Total, Admin, Fundraising, program cols).
func (b *psBuilder) numDataCols() int { return len(psFunctionalCols) + 1 + len(b.progCols) }

// rowSums holds one row's per-column footed totals (target currency minor units) plus a
// per-column "present" flag, so a cell with no activity renders BLANK not zero. Column order:
// [Total, Admin, Fundraising, program cols...] — Total is derived, never stored directly.
type rowSums struct {
	admin, fundraising int64
	adminHas, frHas    bool
	prog               []int64 // per program column (aligned to b.progCols)
	progHas            []bool
}

// leafSums reads one leaf account's row from the matrix into a rowSums (present flags track
// which cells to render). Only the account's OWN cells (no rollup — placeholder subtotals
// fold leaves separately).
func (b *psBuilder) leafSums(acct AccountID) rowSums {
	rs := rowSums{prog: make([]int64, len(b.progCols)), progHas: make([]bool, len(b.progCols))}
	byCol := b.mat[acct]
	if byCol == nil {
		return rs
	}
	if v, ok := byCol[ProgramMatrixCol{Class: "management"}]; ok {
		rs.admin, rs.adminHas = v, true
	}
	if v, ok := byCol[ProgramMatrixCol{Class: "fundraising"}]; ok {
		rs.fundraising, rs.frHas = v, true
	}
	for i, pc := range b.progCols {
		if v, ok := byCol[ProgramMatrixCol{Program: pc.id}]; ok {
			rs.prog[i], rs.progHas[i] = v, true
		}
	}
	return rs
}

// add folds src into rs (a placeholder-parent subtotal accumulating its leaves).
func (rs *rowSums) add(src rowSums) {
	if src.adminHas {
		rs.admin, rs.adminHas = rs.admin+src.admin, true
	}
	if src.frHas {
		rs.fundraising, rs.frHas = rs.fundraising+src.fundraising, true
	}
	for i := range src.prog {
		if src.progHas[i] {
			rs.prog[i], rs.progHas[i] = rs.prog[i]+src.prog[i], true
		}
	}
}

// total returns the row Total = Admin + Fundraising + Σ(ROOT program columns). Summing only
// root programs avoids double-counting the rolled-up descendants (a parent column already
// folds its children). A root program column is one whose parent is 0 within the VISIBLE set
// (a scoped view's picked root has parent != 0 but is the visible root — so "root" means: no
// visible ancestor). rootIdx precomputes those indices.
func (rs rowSums) total(rootIdx []int) int64 {
	t := rs.admin + rs.fundraising
	for _, i := range rootIdx {
		t += rs.prog[i]
	}
	return t
}

// rootProgIdx returns the indices of the visible-root program columns (no visible ancestor),
// so the row Total sums only disjoint rollup roots.
func (b *psBuilder) rootProgIdx() []int {
	visible := make(map[ProgramID]bool, len(b.progCols))
	for _, pc := range b.progCols {
		visible[pc.id] = true
	}
	var idx []int
	for i, pc := range b.progCols {
		if pc.parent == 0 || !visible[pc.parent] {
			idx = append(idx, i)
		}
	}
	return idx
}

// section renders one R/E section (type "revenue"|"expense"): a section header row, then the
// account tree walked pre-order — placeholder parents as SUBTOTAL rows carrying their
// subtree's per-column sums, leaves WITH ACTIVITY as data rows — then a section total.
// `sign` is the display sign (−1 revenue shown positive, +1 expense). An empty section (no
// in-scope activity of this type) emits nothing.
func (b *psBuilder) section(typ, sectionKey, totalKey string, sign int64) {
	// Per-node folded sums + whether a node's subtree carries any activity of this type.
	sums := make(map[AccountID]rowSums)
	inSection := make(map[AccountID]bool)
	var fold func(id AccountID) rowSums
	fold = func(id AccountID) rowSums {
		if !b.isPlaceholder[id] {
			rs := rowSums{prog: make([]int64, len(b.progCols)), progHas: make([]bool, len(b.progCols))}
			if b.typeOf[id] == typ {
				if leaf := b.leafSums(id); b.anyPresent(leaf) {
					rs = leaf
					inSection[id] = true
				}
			}
			sums[id] = rs
			return rs
		}
		rs := rowSums{prog: make([]int64, len(b.progCols)), progHas: make([]bool, len(b.progCols))}
		for _, c := range b.children[id] {
			rs.add(fold(c))
			if inSection[c] {
				inSection[id] = true
			}
		}
		sums[id] = rs
		return rs
	}
	for _, r := range b.roots {
		fold(r)
	}

	any := false
	for _, r := range b.roots {
		if inSection[r] {
			any = true
			break
		}
	}
	if !any {
		return
	}

	b.headerRow(sectionKey)

	rootIdx := b.rootProgIdx()
	var sectionSum rowSums
	sectionSum.prog = make([]int64, len(b.progCols))
	sectionSum.progHas = make([]bool, len(b.progCols))
	var walk func(id AccountID)
	walk = func(id AccountID) {
		if !inSection[id] {
			return
		}
		d := b.depth[id]
		if b.isPlaceholder[id] {
			b.dataRow(TextCell(b.name[id]), sums[id], sign, rootIdx, d, RowSubtotal)
			for _, c := range b.children[id] {
				walk(c)
			}
			return
		}
		b.dataRow(TextCell(b.name[id]), sums[id], sign, rootIdx, d, RowData)
		sectionSum.add(sums[id])
	}
	for _, r := range b.roots {
		walk(r)
	}

	b.totalRow(totalKey, sectionSum, sign, rootIdx)
}

// anyPresent reports whether a leaf's rowSums has any activity cell (so a zero/absent leaf
// row drops out of the section, matching the SoA rule).
func (b *psBuilder) anyPresent(rs rowSums) bool {
	if rs.adminHas || rs.frHas {
		return true
	}
	for _, h := range rs.progHas {
		if h {
			return true
		}
	}
	return false
}

// dataRow emits one account row (leaf or placeholder subtotal): the name cell, then Total,
// Admin, Fundraising, and each program column. A cell with no activity renders BLANK. Cells
// are NOT drillable: a converted figure sums across native currencies, so — like the
// functional-expenses / trial-balance rule — no single currency-filtered drill reconciles it.
func (b *psBuilder) dataRow(nameCell Cell, rs rowSums, sign int64, rootIdx []int, depth int, kind RowKind) {
	cells := make([]Cell, 0, b.numDataCols()+1)
	cells = append(cells, nameCell)

	// Total (derived; blank only if the row is wholly empty — which section() already
	// excludes, so a rendered row always has a Total).
	cells = append(cells, MoneyCell(sign*rs.total(rootIdx), b.target))
	cells = append(cells, b.moneyOrBlank(rs.admin, rs.adminHas, sign))
	cells = append(cells, b.moneyOrBlank(rs.fundraising, rs.frHas, sign))
	for i := range b.progCols {
		cells = append(cells, b.moneyOrBlank(rs.prog[i], rs.progHas[i], sign))
	}

	b.rows = append(b.rows, Row{Cells: cells, Indent: depth, Kind: kind})
}

// moneyOrBlank returns a signed money cell for present activity, or a BLANK money cell when
// there is none (distinguishing "no amount here" from a formatted zero).
func (b *psBuilder) moneyOrBlank(v int64, present bool, sign int64) Cell {
	if !present {
		return BlankMoneyCell()
	}
	return MoneyCell(sign*v, b.target)
}

// headerRow appends a section heading row (Revenue/Expenses): the section label, then blank
// money cells across every data column.
func (b *psBuilder) headerRow(key string) {
	cells := make([]Cell, 0, b.numDataCols()+1)
	cells = append(cells, LabelCell(key))
	for i := 0; i < b.numDataCols(); i++ {
		cells = append(cells, BlankMoneyCell())
	}
	b.rows = append(b.rows, Row{Cells: cells, Indent: 0, Kind: RowData})
}

// totalRow appends a SECTION-total row (Total revenue / Total expenses): the label, then the
// footed per-column sums (with a blank column left blank).
func (b *psBuilder) totalRow(key string, rs rowSums, sign int64, rootIdx []int) {
	b.labeledSumRow(LabelCell(key), rs, sign, rootIdx, RowSectionTotal)
}

// netLine appends the grand Net row (revenue − expenses) per column: the Net label, then each
// column's net figure. It folds every R/E leaf's RAW net-debit-credit value (revenue is a
// credit = negative, expense a debit = positive) and negates the sum: Net = −Σraw = −(raw
// expenses − |raw revenue|)... i.e. revenue (shown +) − expenses (shown +). It foots the same
// rounded ProgramMatrix cells (no re-conversion). Net is the strongest tier (RowTotal).
func (b *psBuilder) netLine() {
	rootIdx := b.rootProgIdx()
	var net rowSums
	net.prog = make([]int64, len(b.progCols))
	net.progHas = make([]bool, len(b.progCols))
	for acct := range b.mat {
		leaf := b.leafSums(acct)
		if !b.anyPresent(leaf) {
			continue
		}
		if t := b.typeOf[acct]; t != "revenue" && t != "expense" {
			continue // defensive: only R/E accounts carry program/class activity
		}
		net.add(leaf) // RAW: revenue negative, expense positive
	}
	// −Σraw: revenue (−raw → +) minus expenses (+raw → −) = revenue − expenses shown positive.
	b.labeledSumRow(LabelCell("reports.program_statement.net"), net, -1, rootIdx, RowTotal)
}

// labeledSumRow emits a label row carrying a rowSums across every data column (Total, Admin,
// Fundraising, program cols), a blank column staying blank. `sign` scales the stored sums.
func (b *psBuilder) labeledSumRow(nameCell Cell, rs rowSums, sign int64, rootIdx []int, kind RowKind) {
	cells := make([]Cell, 0, b.numDataCols()+1)
	cells = append(cells, nameCell)
	cells = append(cells, MoneyCell(sign*rs.total(rootIdx), b.target))
	cells = append(cells, b.moneyOrBlank(rs.admin, rs.adminHas, sign))
	cells = append(cells, b.moneyOrBlank(rs.fundraising, rs.frHas, sign))
	for i := range b.progCols {
		cells = append(cells, b.moneyOrBlank(rs.prog[i], rs.progHas[i], sign))
	}
	b.rows = append(b.rows, Row{Cells: cells, Indent: 0, Kind: kind})
}

func (b *psBuilder) table() Table {
	return Table{Columns: b.cols, Rows: b.rows}
}

// progCol is one program in tree PRE-ORDER as a FLAT list entry: its id, display name, and
// DESCENDANT set (self + children, D24) a rollup cell's drill spans. Retained for Form 990
// Part III (form_990.go), which lists each program group linearly.
type progCol struct {
	id          ProgramID
	name        string
	descendants []ProgramID
}

// programColumns returns every program in tree pre-order as a flat progCol list, each
// carrying its descendant set (for the rollup-cell drill). Used by Form 990 Part III to emit
// one program group per row.
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

// itoa renders an int64 id as its decimal string (for a data-* attribute value). A tiny
// local helper so the builder never reaches for strconv in a value path.
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
