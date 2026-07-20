package reports_test

// p15.10 program statement report tests. The DECISION-MAKER view of revenue and expense
// per PROGRAM (D24), the source p15.11 draws 990 Part III from. Every number is
// HAND-DERIVED from the synthetic fixture (PLAN Appendix D, internal/testutil/fixture) —
// f.Expected.Program is the RAW (per-program, non-rolled) oracle; the report rolls each
// program's figures UP the program tree (a parent folds in its descendants), and these
// tests assert both the RAW partition and the ROLLED figures.
//
// GROUP programs. LAYOUT (p31): a COLLAPSIBLE PROGRAM TREE stacked as ROWS — each program
// in tree pre-order is a HEADER row (its rolled net) spanning its own Revenue/Expense
// account tree AND its nested child programs, in one Amount column per currency block.
// Columns: Program / Account | Currency (native only) | Amount. NATIVE currency, one
// currency block after another. Revenue displayed POSITIVE (net-debit credit ×−1), Expense
// POSITIVE, net = revenue − expenses.
//
// HAND-DERIVED (root scope, 2025-01-01..2026-06-30), ROLLED per-program figures:
//
//	General (root, folds in Educación + Food Pantry):
//	  Revenue:  Contributions USD 5,275,000 · Event Income USD 300,000 ·
//	            Government Grants USD 200,000 / MXN 10,000,000 · Program Fees USD 120,000
//	  Expense:  Salaries USD 1,650,000 · Program Supplies USD 210,000 (60k+150k Educ) /
//	            MXN 500,000 · Food Purchases MXN 360,000 (210k+150k FP) ·
//	            Occupancy USD 305,000 · Insurance USD 60,000 · Bank Fees USD 2,500 ·
//	            Event Costs USD 100,000
//	  Net: USD 3,567,500 · MXN 9,140,000   (== p15.9 chTotal — the org's R/E activity)
//	Educación (leaf): Rev GovGrants USD 200,000 / MXN 10,000,000 · ProgramFees USD 120,000
//	                  Exp ProgramSupplies USD 150,000 / MXN 500,000
//	                  Net: USD 170,000 (320,000 − 150,000) · MXN 9,500,000 (10,000,000 − 500,000)
//	Food Pantry (leaf): Exp Food Purchases MXN 150,000 ; Net MXN −150,000

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/ids"
	"cuento/internal/reports"
	"cuento/internal/store"
	"cuento/internal/testutil/fixture"
)

// psReport fetches the registered program statement from Default().
func psReport(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.ProgramStatementReportID)
	if !ok {
		t.Fatalf("program statement report %q not registered in Default()", reports.ProgramStatementReportID)
	}
	return rep
}

// psParams runs the statement over the whole fixture span, root scope, lang en,
// comparative view (no program chosen).
func psParams(f *fixture.Fixture) reports.Params {
	return reports.Params{
		Scope: reports.SubsidiaryID(f.IDs.Root),
		From:  f.Expected.ActivityFrom, // 2025-01-01
		To:    f.Expected.AsOf,         // 2026-06-30
		Lang:  "en",
	}
}

// psMoneyCol is the single money-column index: native mode carries Program/Account +
// Currency lead columns (so Amount is index 2); converted mode drops Currency (Amount is
// index 1).
func psMoneyCol(tbl reports.Table) int { return len(tbl.Columns) - 1 }

// psProgHeader returns the index of program prog's HEADER row in currency block ccy (a
// RowSubtotal whose first cell is the program name) — the row that opens the program's
// block, or -1 if the program has no block in that currency.
func psProgHeader(tbl reports.Table, prog, ccy string) int {
	for i, row := range tbl.Rows {
		if row.Kind != reports.RowSubtotal || len(row.Cells) < 2 {
			continue
		}
		if row.Cells[0].Text == prog && row.Cells[1].Text == ccy {
			return i
		}
	}
	return -1
}

// psProgOwnRow returns the money-cell minor + Drill of the account/label row whose first
// cell is name and currency is ccy, WITHIN program prog's OWN content in currency block ccy.
// A program's own rows run from its header to its Net line (RowTotal) — the terminator emitted
// after the sections and BEFORE any nested child program (netLine is unconditional). So a
// nested child program's identically-named rows are never matched.
func psProgOwnRow(t *testing.T, tbl reports.Table, prog, name, ccy string) (int64, *reports.Drill, bool) {
	t.Helper()
	col := psMoneyCol(tbl)
	h := psProgHeader(tbl, prog, ccy)
	if h < 0 {
		return 0, nil, false
	}
	for i := h + 1; i < len(tbl.Rows); i++ {
		row := tbl.Rows[i]
		if row.Kind == reports.RowTotal {
			break // Net line: end of this program's OWN content
		}
		if len(row.Cells) <= col {
			continue
		}
		if row.Cells[0].Text == name && row.Cells[1].Text == ccy {
			return row.Cells[col].Minor, row.Cells[col].Drill, true
		}
	}
	return 0, nil, false
}

// psProgNet returns program prog's Net (RowTotal) figure in currency block ccy — the first
// RowTotal after prog's header (a program's own Net precedes any nested child program).
func psProgNet(t *testing.T, tbl reports.Table, prog, ccy string) int64 {
	t.Helper()
	col := psMoneyCol(tbl)
	h := psProgHeader(tbl, prog, ccy)
	if h < 0 {
		t.Fatalf("program %q header not found in %s block", prog, ccy)
	}
	for i := h + 1; i < len(tbl.Rows); i++ {
		if tbl.Rows[i].Kind == reports.RowTotal {
			return tbl.Rows[i].Cells[col].Minor
		}
	}
	t.Fatalf("program %q net row not found in %s block", prog, ccy)
	return 0
}

// TestProgramStatementConverted exercises the p26.54 OPTIONAL currency conversion: setting a
// target currency converts the whole matrix to one currency at the period-end closing rate,
// DROPS the Currency column, and collapses each account to ONE figure per program (a
// multi-currency account no longer emits a row per native currency). Native (empty target) is
// the default and is covered by every other test + the golden; this is the converted path.
func TestProgramStatementConverted(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := psReport(t)

	// Native (default): the leading columns are Program/Account, Currency, then Amount.
	natP := psParams(f)
	natT, err := rep.Run(ctx, reports.NewToolkit(f.Store, natP), natP)
	if err != nil {
		t.Fatalf("run native: %v", err)
	}
	if natT.Columns[0].HeaderKey != "reports.program_statement.col.program_account" {
		t.Fatalf("native column 0 = %q, want the program/account column", natT.Columns[0].HeaderKey)
	}
	if natT.Columns[1].HeaderKey != "reports.program_statement.col.currency" {
		t.Fatalf("native column 1 = %q, want the currency column", natT.Columns[1].HeaderKey)
	}
	if natT.Columns[2].HeaderKey != "reports.program_statement.col.amount" {
		t.Fatalf("native column 2 = %q, want the amount column", natT.Columns[2].HeaderKey)
	}

	// Converted to USD: the Currency column is GONE (column 1 is the amount).
	convP := psParams(f)
	convP.TargetCurrency = "USD"
	convT, err := rep.Run(ctx, reports.NewToolkit(f.Store, convP), convP)
	if err != nil {
		t.Fatalf("run converted: %v", err)
	}
	if len(convT.Columns) != 2 {
		t.Fatalf("converted view has %d columns, want 2 (Program/Account, Amount)", len(convT.Columns))
	}
	if convT.Columns[1].HeaderKey != "reports.program_statement.col.amount" {
		t.Errorf("converted column 1 = %q, want the amount column", convT.Columns[1].HeaderKey)
	}
	// Converting collapses per-currency blocks -> the converted table has FEWER rows than
	// the native two-currency table.
	if len(convT.Rows) >= len(natT.Rows) {
		t.Errorf("converted rows (%d) not fewer than native rows (%d) -- per-currency blocks did not collapse",
			len(convT.Rows), len(natT.Rows))
	}
	// Every money cell in the converted table is labelled in the target currency (USD), and no
	// converted cell is drillable (a converted figure sums across native currencies, so it is
	// not drillable -- the trial-balance rule).
	for _, row := range convT.Rows {
		for _, c := range row.Cells {
			if c.Kind == reports.CellMoney && !c.Blank {
				if c.Currency != "USD" {
					t.Errorf("converted money cell currency = %q, want USD", c.Currency)
				}
				if c.Drill != nil {
					t.Errorf("converted cell is drillable; converted figures must not drill")
				}
			}
		}
	}
}

// TestProgramStatementGolden runs the comparative statement and asserts the per-program
// per-account ROLLED figures, the tree rollup discriminators (General folds in its
// descendants), the program-tree ROW structure (depth + RowKind), and the net-per-program
// line BY HAND, then compares the rendered text + CSV to committed goldens.
func TestProgramStatementGolden(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := psReport(t)

	p := psParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run program statement: %v", err)
	}

	// --- Per-program per-account ROLLED figures (revenue shown +, expense shown +) -------
	type cellWant struct {
		prog, acct, ccy string
		want            int64
	}
	for _, tc := range []cellWant{
		// General block — revenue, incl. the rolled-in children.
		{"General", "Contributions", "USD", 5_275_000},
		{"General", "Government Grants", "MXN", 10_000_000},
		{"General", "Government Grants", "USD", 200_000},
		{"General", "Program Service Fees", "USD", 120_000},
		// General block — expense, ROLLUP discriminators (fold in Educación / Food Pantry).
		{"General", "Program Supplies", "USD", 210_000}, // 60,000 General + 150,000 Educación
		{"General", "Program Supplies", "MXN", 500_000}, // only Educación
		{"General", "Food Purchases", "MXN", 360_000},   // 210,000 General + 150,000 Food Pantry
		{"General", "Salaries", "USD", 1_650_000},
		// Educación (leaf) block.
		{"Educacion", "Program Service Fees", "USD", 120_000},
		{"Educacion", "Program Supplies", "USD", 150_000},
		{"Educacion", "Program Supplies", "MXN", 500_000},
		{"Educacion", "Government Grants", "MXN", 10_000_000},
		// Food Pantry (leaf) block.
		{"Food Pantry", "Food Purchases", "MXN", 150_000},
	} {
		got, _, ok := psProgOwnRow(t, table, tc.prog, tc.acct, tc.ccy)
		if !ok || got != tc.want {
			t.Errorf("%s / %s / %s = %d/%v, want %d", tc.prog, tc.acct, tc.ccy, got, ok, tc.want)
		}
	}

	// Food Pantry has NO USD activity at all: its whole USD block is ABSENT (an empty branch
	// drops out), so its Food Purchases MXN 150,000 lives ONLY in its own MXN block.
	if psProgHeader(table, "Food Pantry", "USD") >= 0 {
		t.Errorf("Food Pantry has a USD block; it has no USD activity, so the branch must drop out")
	}

	// --- Net per program, per currency = revenue − expenses ------------------------------
	for _, tc := range []struct {
		prog, ccy string
		wantNet   int64
	}{
		{"General", "USD", 3_567_500},   // == p15.9 chTotal (org R/E activity)
		{"General", "MXN", 9_140_000},   // == p15.9 chTotal
		{"Educacion", "USD", 170_000},   // 320,000 rev − 150,000 exp
		{"Educacion", "MXN", 9_500_000}, // 10,000,000 rev − 500,000 exp
		{"Food Pantry", "MXN", -150_000},
	} {
		if got := psProgNet(t, table, tc.prog, tc.ccy); got != tc.wantNet {
			t.Errorf("net %s (%s) = %d, want %d", tc.prog, tc.ccy, got, tc.wantNet)
		}
	}

	// --- Program-tree ROW structure (p31): General is a depth-0 RowSubtotal header; its
	// child programs Educación + Food Pantry nest at depth 1, also RowSubtotal headers. The
	// header row's money cell carries the rolled net and is NOT drillable (a rollup).
	for _, tc := range []struct {
		prog, ccy string
		wantDepth int
	}{
		{"General", "MXN", 0},
		{"Educacion", "MXN", 1},
		{"Food Pantry", "MXN", 1},
		{"General", "USD", 0},
		{"Educacion", "USD", 1},
	} {
		h := psProgHeader(table, tc.prog, tc.ccy)
		if h < 0 {
			t.Fatalf("program %q header missing in %s block", tc.prog, tc.ccy)
		}
		row := table.Rows[h]
		if row.Indent != tc.wantDepth {
			t.Errorf("%s (%s) header depth = %d, want %d", tc.prog, tc.ccy, row.Indent, tc.wantDepth)
		}
		if row.Kind != reports.RowSubtotal {
			t.Errorf("%s (%s) header kind = %v, want RowSubtotal", tc.prog, tc.ccy, row.Kind)
		}
		if row.Cells[psMoneyCol(table)].Drill != nil {
			t.Errorf("%s (%s) header cell is drillable; a program rollup must not drill", tc.prog, tc.ccy)
		}
	}

	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "program_statement.txt", []byte(textDump))
	checkGolden(t, "program_statement.csv", csvBuf.Bytes())
}

// TestProgramStatementRollupCorrectness: a parent program's rolled figures == Σ (its own +
// descendants') RAW activity. Asserted against the RAW f.Expected.Program oracle (which is
// per-program, non-rolled) so the report's rollup is checked against an independent tally,
// NOT against its own toolkit call. General == General-direct + Educación + Food Pantry.
func TestProgramStatementRollupCorrectness(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := psReport(t)
	p := psParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Independently sum the RAW oracle per (account, currency) across ALL programs — since
	// General is the root, its rolled block must equal this org-wide raw sum.
	type ac struct {
		acct ids.AccountID
		ccy  string
	}
	rawTotal := map[ac]int64{}
	for _, pc := range f.Expected.Program {
		rawTotal[ac{pc.Account, pc.Currency}] += pc.Amount
	}

	names := psAccountNames(t, f) // account id -> name (for locating the row)

	for k, wantRaw := range rawTotal {
		name := names[k.acct]
		// Displayed value is |raw| (revenue −raw shown positive; expense +raw). Compare
		// magnitudes so the check is sign-convention-independent.
		got, _, ok := psProgOwnRow(t, table, "General", name, k.ccy)
		if !ok {
			t.Errorf("General block missing %s/%s (raw total %d)", name, k.ccy, wantRaw)
			continue
		}
		if got != absInt(wantRaw) {
			t.Errorf("General rolled %s/%s = %d, want |raw org total| %d", name, k.ccy, got, absInt(wantRaw))
		}
	}
}

// TestProgramStatementRawPartition: the RAW partition (General-direct + Educación + Food
// Pantry) reconciles to the total R/E activity per currency — the task's "sum across programs
// reconciles to total R/E" check, done at the ORACLE level (summing the RAW, non-rolled
// per-program cells, which do NOT double-count).
func TestProgramStatementRawPartition(t *testing.T) {
	f := fixture.New(t)

	rawByCcy := map[string]int64{}
	for _, pc := range f.Expected.Program {
		rawByCcy[pc.Currency] += pc.Amount
	}
	// Expected total R/E net-debit per currency (revenue credits negative + expense debits
	// positive), hand-derived: USD = 2,327,500 exp − 5,895,000 rev = −3,567,500 ;
	// MXN = 860,000 exp − 10,000,000 rev = −9,140,000.
	if rawByCcy["USD"] != -3_567_500 {
		t.Errorf("raw partition USD = %d, want -3,567,500", rawByCcy["USD"])
	}
	if rawByCcy["MXN"] != -9_140_000 {
		t.Errorf("raw partition MXN = %d, want -9,140,000", rawByCcy["MXN"])
	}
}

// TestProgramStatementSingleSubtree: the ?program= view shows ONE program subtree (rolled
// up), the chosen program as the depth-0 root. Choosing Educación (a leaf) shows the same
// rolled figures as its comparative block.
func TestProgramStatementSingleSubtree(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := psReport(t)

	// Single-program view for Educación (a leaf subtree).
	p := psParams(f)
	p.Program = reports.ProgramID(f.IDs.Educacion)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run single: %v", err)
	}
	// Still three columns (Program/Account, Currency, Amount) — the single money column.
	if len(table.Columns) != 3 {
		t.Fatalf("single view has %d columns, want 3 (Program/Account, Currency, Amount)", len(table.Columns))
	}
	// Educación is now the depth-0 root of its own subtree.
	h := psProgHeader(table, "Educacion", "MXN")
	if h < 0 || table.Rows[h].Indent != 0 {
		t.Fatalf("single view: Educación must be the depth-0 root")
	}
	// Educación's rolled cells == its leaf oracle.
	for _, tc := range []struct {
		acct, ccy string
		want      int64
	}{
		{"Program Service Fees", "USD", 120_000},
		{"Program Supplies", "USD", 150_000},
		{"Program Supplies", "MXN", 500_000},
		{"Government Grants", "MXN", 10_000_000},
	} {
		got, _, ok := psProgOwnRow(t, table, "Educacion", tc.acct, tc.ccy)
		if !ok || got != tc.want {
			t.Errorf("single Educación %s/%s = %d/%v, want %d", tc.acct, tc.ccy, got, ok, tc.want)
		}
	}
	// Net per currency.
	if got := psProgNet(t, table, "Educacion", "USD"); got != 170_000 {
		t.Errorf("single Educación net USD = %d, want 170,000", got)
	}
	if got := psProgNet(t, table, "Educacion", "MXN"); got != 9_500_000 {
		t.Errorf("single Educación net MXN = %d, want 9,500,000", got)
	}
}

// TestProgramStatementGrantProgramScope (p27.4): a program-SUBTREE-scoped report grant
// restricts the report's rows to the granted subtree. Scoping to Educacion (a leaf, so the
// subtree is just itself) must show Educacion's activity and MUST NOT leak the sibling Food
// Pantry subtree -- crucially even in the ROLLED General (root) block, which without the
// filter folds in every program. This proves the filter is applied to the RAW split program
// BEFORE the ancestor rollup (a sibling never contributes to any cell, incl. an ancestor's).
func TestProgramStatementGrantProgramScope(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := psReport(t)

	// Baseline (UNSCOPED): the General block folds in Food Pantry's Food Purchases MXN
	// (210,000 General-direct + 150,000 Food Pantry = 360,000) — confirming the sibling IS
	// present without a scope (the goldens do not move; the scope is purely additive).
	base := psParams(f)
	baseT, err := rep.Run(ctx, reports.NewToolkit(f.Store, base), base)
	if err != nil {
		t.Fatalf("run unscoped: %v", err)
	}
	if got, _, ok := psProgOwnRow(t, baseT, "General", "Food Purchases", "MXN"); !ok || got != 360_000 {
		t.Fatalf("unscoped General Food Purchases MXN = %d/%v, want 360,000 (sibling present)", got, ok)
	}

	// Scope the grant to Educacion (a leaf subtree = {Educacion}). Food Pantry is a SIBLING
	// and must vanish from every block.
	p := psParams(f)
	p.ProgramScope = []reports.ProgramID{reports.ProgramID(f.IDs.Educacion)}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run scoped: %v", err)
	}

	// The rolled General block now reflects ONLY Educacion's activity (the root folds in its
	// in-scope descendants; Food Pantry is filtered out at the raw split).
	//   - Food Purchases MXN: General-direct 210,000 is a General-program split, NOT in the
	//     Educacion subtree, so it is filtered too -> the row is absent/0.
	if got, _, ok := psProgOwnRow(t, table, "General", "Food Purchases", "MXN"); ok && got != 0 {
		t.Errorf("scoped General Food Purchases MXN = %d, want absent/0 (no General-direct or sibling leak)", got)
	}
	//   - Salaries is a General-direct (non-Educacion) expense: filtered out -> absent/0.
	if got, _, ok := psProgOwnRow(t, table, "General", "Salaries", "USD"); ok && got != 0 {
		t.Errorf("scoped General Salaries USD = %d, want absent/0 (out-of-subtree)", got)
	}
	//   - The sibling Food Pantry program block is gone entirely.
	if psProgHeader(table, "Food Pantry", "MXN") >= 0 {
		t.Errorf("scoped view still shows the sibling Food Pantry block")
	}
	//   - Educacion's OWN activity DOES appear in the (now Educacion-only) General block.
	if got, _, ok := psProgOwnRow(t, table, "General", "Program Supplies", "MXN"); !ok || got != 500_000 {
		t.Errorf("scoped General Program Supplies MXN = %d/%v, want 500,000 (Educacion in scope)", got, ok)
	}
	if got, _, ok := psProgOwnRow(t, table, "General", "Government Grants", "MXN"); !ok || got != 10_000_000 {
		t.Errorf("scoped General Government Grants MXN = %d/%v, want 10,000,000 (Educacion in scope)", got, ok)
	}
	// General net == Educacion net (the root now equals the single in-scope subtree).
	if got := psProgNet(t, table, "General", "MXN"); got != 9_500_000 {
		t.Errorf("scoped General net MXN = %d, want 9,500,000 (== Educacion, no sibling)", got)
	}
}

// TestProgramStatementLeafDrillReconciles: a LEAF program's cell drills (single ProgramID)
// to its splits, and the drilled native signed sum equals the cell's pre-display net-debit
// figure. Educación × Program Supplies × MXN = 500,000 (displayed) → raw net-debit 500,000.
func TestProgramStatementLeafDrillReconciles(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := psReport(t)
	p := psParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	_, d, ok := psProgOwnRow(t, table, "Educacion", "Program Supplies", "MXN")
	if !ok || d == nil {
		t.Fatalf("Educación Program Supplies MXN cell not drillable")
	}
	if len(d.ProgramIDs) != 0 {
		t.Errorf("leaf drill should use single ProgramID, got ProgramIDs %v", d.ProgramIDs)
	}
	if d.ProgramID == nil || *d.ProgramID != f.IDs.Educacion {
		t.Errorf("leaf drill ProgramID = %v, want Educación %d", d.ProgramID, f.IDs.Educacion)
	}
	if sum := psDrillSum(t, f, d); sum != 500_000 {
		t.Errorf("Educación Program Supplies MXN drill sum = %d, want 500,000 (raw net-debit)", sum)
	}
}

// TestProgramStatementRollupDrillReconciles: a ROLLUP cell (General, which has descendants)
// drills via the program SET (Drill.ProgramIDs = the subtree), unioning the per-program split
// sets, and the drilled native sum equals the rolled figure. General × Program Supplies × USD
// = 210,000 = 60,000 (General-direct) + 150,000 (Educación) + 0 (Food Pantry).
func TestProgramStatementRollupDrillReconciles(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := psReport(t)
	p := psParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	_, d, ok := psProgOwnRow(t, table, "General", "Program Supplies", "USD")
	if !ok || d == nil {
		t.Fatalf("General Program Supplies USD cell not drillable")
	}
	// A rollup cell uses the program SET, not a single ProgramID.
	if len(d.ProgramIDs) == 0 {
		t.Fatalf("General (rollup) drill should carry ProgramIDs, got none")
	}
	if d.ProgramID != nil {
		t.Errorf("rollup drill should not set a single ProgramID, got %v", *d.ProgramID)
	}
	// The set is General's subtree (self + Educación + Food Pantry).
	wantSet := map[reports.ProgramID]bool{f.IDs.General: true, f.IDs.Educacion: true, f.IDs.FoodPantry: true}
	if len(d.ProgramIDs) != len(wantSet) {
		t.Errorf("rollup drill ProgramIDs = %v, want subtree %v", d.ProgramIDs, wantSet)
	}
	for _, id := range d.ProgramIDs {
		if !wantSet[id] {
			t.Errorf("rollup drill ProgramIDs has unexpected %d", id)
		}
	}
	if sum := psDrillSum(t, f, d); sum != 210_000 {
		t.Errorf("General Program Supplies USD rollup drill sum = %d, want 210,000 (60k+150k+0)", sum)
	}
}

// TestProgramStatementCSVParses: the statement CSV parses to well-formed records with the
// localized header (Program / Account, Currency, Amount).
func TestProgramStatementCSVParses(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := psReport(t)
	p := psParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	exps := goldenExps(t, f)
	var buf bytes.Buffer
	if err := reports.WriteCSV(&buf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	recs := parseCSV(t, buf.Bytes())
	if len(recs) < 2 {
		t.Fatalf("csv has %d records, want header + rows", len(recs))
	}
	wantHeader := []string{"Program / Account", "Currency", "Amount"}
	for i, h := range wantHeader {
		if recs[0][i] != h {
			t.Errorf("csv header[%d] = %q, want %q", i, recs[0][i], h)
		}
	}
}

// psRowByDepth returns the row at index i and whether i is in range.
func psRowAt(tbl reports.Table, i int) (reports.Row, bool) {
	if i < 0 || i >= len(tbl.Rows) {
		return reports.Row{}, false
	}
	return tbl.Rows[i], true
}

// psFindRow returns the index of the FIRST row whose first cell is name and currency (col 1)
// is ccy, at or after start, and whether found.
func psFindRow(tbl reports.Table, name, ccy string, start int) (int, bool) {
	for i := start; i < len(tbl.Rows); i++ {
		row := tbl.Rows[i]
		if len(row.Cells) < 2 {
			continue
		}
		if row.Cells[0].Text == name && row.Cells[1].Text == ccy {
			return i, true
		}
	}
	return 0, false
}

// TestProgramStatementCollapsibleTree (p31): the statement is a COLLAPSIBLE PROGRAM +
// ACCOUNT TREE. The report registers Tree: true (so the web layer emits data-depth + the
// collapse/expand controls); a placeholder PARENT account (the "Expenses" section parent)
// renders as a roll-up SUBTOTAL row over its indented leaf children — WITHIN a program block
// and a currency block, its cell == the sum of that currency's leaves — and its leaves nest
// one level deeper. The subtotal row is NOT drillable (a rollup spans many leaves).
func TestProgramStatementCollapsibleTree(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := psReport(t)

	// The report must be Tree-marked so the template emits data-depth + tree controls.
	if !rep.Tree {
		t.Fatalf("program statement must register Tree: true for collapsibility")
	}

	p := psParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	col := psMoneyCol(table)

	// Within General's block, the "Expenses" placeholder parent renders as a SUBTOTAL row,
	// its cell NOT a drill link (rollups aren't drillable), with its leaf children nesting one
	// level deeper. General is a depth-0 root, so its Expenses placeholder is at depth 1.
	for _, blk := range []struct {
		ccy         string
		wantExp     int64  // General block's Expenses subtotal for this currency
		leafIn      string // a leaf present in this currency block
		leafAmt     int64
		leafOutOf   string // a leaf that must be ABSENT from this currency block
		wantExpDep  int    // depth of the Expenses placeholder under General (depth 0)
		wantLeafDep int    // depth of a leaf under the Expenses placeholder
	}{
		// USD block: Expenses $23,275.00 = Σ children; Food Purchases (MXN-only) absent.
		{"USD", 2_327_500, "Salaries", 1_650_000, "Food Purchases", 1, 2},
		// MXN block: Expenses $8,600.00 = Program Supplies + Food Purchases; Salaries absent.
		{"MXN", 860_000, "Program Supplies", 500_000, "Salaries", 1, 2},
	} {
		h := psProgHeader(table, "General", blk.ccy)
		if h < 0 {
			t.Fatalf("%s block: General header missing", blk.ccy)
		}
		// The Expenses placeholder SUBTOTAL under General (skip the section-label RowData;
		// the placeholder is the RowSubtotal named "Expenses").
		ei := -1
		for i := h + 1; i < len(table.Rows); i++ {
			r := table.Rows[i]
			if r.Kind == reports.RowTotal {
				break // General's own content ended
			}
			if r.Cells[0].Text == "Expenses" && r.Cells[1].Text == blk.ccy && r.Kind == reports.RowSubtotal {
				ei = i
				break
			}
		}
		if ei < 0 {
			t.Fatalf("%s block: General's Expenses placeholder subtotal missing", blk.ccy)
		}
		exp := table.Rows[ei]
		if exp.Indent != blk.wantExpDep {
			t.Errorf("%s Expenses parent depth = %d, want %d (under General depth 0)", blk.ccy, exp.Indent, blk.wantExpDep)
		}
		if exp.Cells[col].Minor != blk.wantExp {
			t.Errorf("%s Expenses subtotal = %d, want %d (Σ children)", blk.ccy, exp.Cells[col].Minor, blk.wantExp)
		}
		if exp.Cells[col].Drill != nil {
			t.Errorf("%s Expenses subtotal cell is drillable; a rollup must not drill", blk.ccy)
		}
		// The in-currency leaf is present, one level deeper than the placeholder.
		li, ok := psFindRow(table, blk.leafIn, blk.ccy, ei+1)
		if !ok {
			t.Fatalf("%s block: leaf %q missing", blk.ccy, blk.leafIn)
		}
		leaf, _ := psRowAt(table, li)
		if leaf.Indent != blk.wantLeafDep {
			t.Errorf("%s leaf %q depth = %d, want %d (under Expenses)", blk.ccy, blk.leafIn, leaf.Indent, blk.wantLeafDep)
		}
		if leaf.Cells[col].Minor != blk.leafAmt {
			t.Errorf("%s leaf %q = %d, want %d", blk.ccy, blk.leafIn, leaf.Cells[col].Minor, blk.leafAmt)
		}
		// The out-of-currency leaf is ABSENT from General's block in this currency.
		if _, _, present := psProgOwnRow(t, table, "General", blk.leafOutOf, blk.ccy); present {
			t.Errorf("%s block leaks out-of-currency leaf %q (per-currency fold broke)", blk.ccy, blk.leafOutOf)
		}
	}
}

// TestProgramStatementNestedSubtotal (p31): a MULTI-LEVEL placeholder parent (a grouping
// account nested UNDER the Expenses section) renders as a nested subtotal whose cell == the
// sum of its SAME-CURRENCY leaves, contiguous ABOVE its own children. The base fixture R/E
// tree is FLAT (section -> leaves), so this path is exercised inline: create "Field Ops"
// (placeholder) under Expenses with a USD and an MXN leaf, post an expense to each (General
// program), and assert the nested rollup arithmetic + depth + contiguity within General's
// block.
func TestProgramStatementNestedSubtotal(t *testing.T) {
	f := fixture.New(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	rep := psReport(t)

	prog := "program"
	fieldOps, err := f.Store.CreateAccount(ctx, store.CreateAccountInput{
		ParentID: &f.IDs.Expenses, Type: "expense", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Field Ops", "es": "Operaciones de campo"}, Subsidiaries: []ids.SubsidiaryID{f.IDs.Root, f.IDs.US, f.IDs.MX},
	})
	if err != nil {
		t.Fatalf("create Field Ops: %v", err)
	}
	fuel, err := f.Store.CreateAccount(ctx, store.CreateAccountInput{
		ParentID: &fieldOps, Type: "expense", DefaultCurrency: "USD", FunctionalClass: &prog,
		Names: map[string]string{"en": "Fuel", "es": "Combustible"}, Subsidiaries: []ids.SubsidiaryID{f.IDs.Root, f.IDs.US, f.IDs.MX},
	})
	if err != nil {
		t.Fatalf("create Fuel: %v", err)
	}
	rations, err := f.Store.CreateAccount(ctx, store.CreateAccountInput{
		ParentID: &fieldOps, Type: "expense", DefaultCurrency: "MXN", FunctionalClass: &prog,
		Names: map[string]string{"en": "Rations", "es": "Raciones"}, Subsidiaries: []ids.SubsidiaryID{f.IDs.Root, f.IDs.US, f.IDs.MX},
	})
	if err != nil {
		t.Fatalf("create Rations: %v", err)
	}

	// DR Fuel 400.00 USD (program General), CR Checking US. DR Rations 900.00 MXN, CR
	// Checking MX. Each is a balanced R/E expense carrying the General program (D24).
	if _, err := f.Store.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: f.IDs.US, Currency: "USD", Memo: "test fuel",
		Splits: []store.SplitInput{
			{AccountID: fuel, Amount: 40_000, ProgramID: &f.IDs.General},
			{AccountID: f.IDs.CheckingUS, Amount: -40_000},
		},
	}); err != nil {
		t.Fatalf("post fuel txn: %v", err)
	}
	if _, err := f.Store.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: f.IDs.MX, Currency: "MXN", Memo: "test rations",
		Splits: []store.SplitInput{
			{AccountID: rations, Amount: 90_000, ProgramID: &f.IDs.General},
			{AccountID: f.IDs.CheckingMX, Amount: -90_000},
		},
	}); err != nil {
		t.Fatalf("post rations txn: %v", err)
	}

	p := psParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	col := psMoneyCol(table)

	// USD block, General's block: Field Ops (nested placeholder, depth 2 = under General(0) >
	// Expenses(1)) subtotal == Fuel leaf (400.00 = 40,000), Fuel at depth 3.
	hUSD := psProgHeader(table, "General", "USD")
	foUSDi, ok := psFindRow(table, "Field Ops", "USD", hUSD+1)
	if !ok {
		t.Fatalf("USD block: Field Ops nested subtotal missing")
	}
	foUSD := table.Rows[foUSDi]
	if foUSD.Kind != reports.RowSubtotal {
		t.Errorf("Field Ops USD kind = %v, want RowSubtotal", foUSD.Kind)
	}
	if foUSD.Indent != 2 {
		t.Errorf("Field Ops USD depth = %d, want 2 (General 0 > Expenses 1 > Field Ops 2)", foUSD.Indent)
	}
	fuelUSDi, ok := psFindRow(table, "Fuel", "USD", foUSDi+1)
	if !ok {
		t.Fatalf("USD block: Fuel leaf missing")
	}
	fuelUSD := table.Rows[fuelUSDi]
	if fuelUSD.Indent != foUSD.Indent+1 {
		t.Errorf("Fuel depth = %d, want Field Ops+1 (%d)", fuelUSD.Indent, foUSD.Indent+1)
	}
	if foUSD.Cells[col].Minor != 40_000 || fuelUSD.Cells[col].Minor != 40_000 {
		t.Errorf("Field Ops USD subtotal = %d, Fuel = %d, want both 40,000 (rollup == same-currency child)",
			foUSD.Cells[col].Minor, fuelUSD.Cells[col].Minor)
	}

	// MXN block, General's block: Field Ops subtotal == Rations leaf (900.00 = 90,000).
	if got, _, ok := psProgOwnRow(t, table, "General", "Field Ops", "MXN"); !ok || got != 90_000 {
		t.Errorf("Field Ops MXN subtotal = %d/%v, want 90,000 (== Rations)", got, ok)
	}

	// CONTIGUITY (the treetable data-depth contract): Field Ops (depth 2) must be IMMEDIATELY
	// followed by its deeper child (Fuel, depth 3) — no same-or-shallower row interleaves.
	next := table.Rows[foUSDi+1]
	if next.Indent <= foUSD.Indent {
		t.Errorf("row after Field Ops has depth %d <= parent %d — subtree not contiguous", next.Indent, foUSD.Indent)
	}
}

// --- helpers ----------------------------------------------------------------

// psAccountNames returns account id -> resolved (en) name from the store tree.
func psAccountNames(t *testing.T, f *fixture.Fixture) map[ids.AccountID]string {
	t.Helper()
	tree, err := f.Store.Tree(context.Background(), "en", nil)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	m := make(map[ids.AccountID]string, len(tree))
	for _, r := range tree {
		m[r.ID] = r.Name
	}
	return m
}

// psDrillSum mirrors the web drill handler: it loops the account SET × the program SET
// (Drill.ProgramIDs), summing the signed splits each (account, program) filter selects. When
// ProgramIDs is empty it falls back to the single ProgramID (the leaf drill shape).
func psDrillSum(t *testing.T, f *fixture.Fixture, d *reports.Drill) int64 {
	t.Helper()
	progs := d.ProgramIDs
	var ptrs []*reports.ProgramID
	if len(progs) == 0 {
		ptrs = []*reports.ProgramID{d.ProgramID}
	} else {
		ptrs = make([]*reports.ProgramID, len(progs))
		for i := range progs {
			id := progs[i]
			ptrs[i] = &id
		}
	}
	var sum int64
	for _, prog := range ptrs {
		filter := store.DrillFilter{
			Scope:     d.Scope,
			Currency:  d.Currency,
			AsOf:      d.AsOf,
			From:      d.From,
			To:        d.To,
			FundID:    d.FundID,
			ProgramID: prog,
			Class:     d.Class,
		}
		for _, acct := range d.AccountIDs {
			filter.AccountID = acct
			splits, err := f.Store.DrillSplits(context.Background(), filter)
			if err != nil {
				t.Fatalf("drill splits: %v", err)
			}
			for _, s := range splits {
				sum += s.Amount
			}
		}
	}
	return sum
}

// absInt returns the absolute value of v.
func absInt(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
