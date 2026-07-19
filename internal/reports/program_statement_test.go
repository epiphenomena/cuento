package reports_test

// p15.10 program statement report tests. The DECISION-MAKER view of revenue and expense
// per PROGRAM (D24), the source p15.11 draws 990 Part III from. Every number is
// HAND-DERIVED from the synthetic fixture (PLAN Appendix D, internal/testutil/fixture) —
// f.Expected.Program is the RAW (per-program, non-rolled) oracle; the report rolls each
// program's figures UP the program tree (a parent folds in its descendants), and these
// tests assert both the RAW partition and the ROLLED columns.
//
// GROUP programs. NATIVE currency, per-currency rows: Account | Currency | <program...>.
// The COMPARATIVE view (default) shows every program in tree pre-order — General,
// Educación, Food Pantry — as columns; the ROOT (General) column IS the org-wide total
// (D24 single seeded root), so no separate Total column is emitted. Revenue displayed
// POSITIVE (net-debit credit ×−1), Expense POSITIVE, net = revenue − expenses.
//
// HAND-DERIVED (root scope, 2025-01-01..2026-06-30), ROLLED per-program cells:
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

// TestProgramStatementConverted exercises the p26.54 OPTIONAL currency conversion:
// setting a target currency converts the whole matrix to one currency at the period-end
// closing rate, DROPS the Currency column, and collapses each account to ONE row per
// program column (a multi-currency account no longer emits a row per native currency).
// Native (empty target) is the default and is covered by every other test + the golden;
// this is the converted path (no golden -- verify reachability).
func TestProgramStatementConverted(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := psReport(t)

	// Native (default): the leading columns are Account, Currency, then the programs.
	natP := psParams(f)
	natT, err := rep.Run(ctx, reports.NewToolkit(f.Store, natP), natP)
	if err != nil {
		t.Fatalf("run native: %v", err)
	}
	if natT.Columns[1].HeaderKey != "reports.program_statement.col.currency" {
		t.Fatalf("native column 1 = %q, want the currency column", natT.Columns[1].HeaderKey)
	}

	// Converted to USD: the Currency column is GONE (column 1 is the first program).
	convP := psParams(f)
	convP.TargetCurrency = "USD"
	convT, err := rep.Run(ctx, reports.NewToolkit(f.Store, convP), convP)
	if err != nil {
		t.Fatalf("run converted: %v", err)
	}
	if convT.Columns[1].HeaderKey == "reports.program_statement.col.currency" {
		t.Errorf("converted view still carries a Currency column")
	}
	// The account column is still first; the second column is a program name header.
	if convT.Columns[0].HeaderKey != "reports.program_statement.col.account" {
		t.Errorf("converted column 0 = %q, want the account column", convT.Columns[0].HeaderKey)
	}
	// Converting collapses per-currency rows -> the converted table has FEWER rows than
	// the native per-currency table (multi-currency accounts + totals collapse).
	if len(convT.Rows) >= len(natT.Rows) {
		t.Errorf("converted rows (%d) not fewer than native rows (%d) -- per-currency rows did not collapse",
			len(convT.Rows), len(natT.Rows))
	}
	// Every money cell in the converted table is labelled in the target currency (USD),
	// and no converted cell is drillable (a converted figure sums across native
	// currencies, so it is not drillable -- the trial-balance rule).
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

// psColIndex returns the money-cell column index for the program named progName in the
// comparative table, by matching the header (the leading two columns are Account,
// Currency, so the first program column is index 2 in the cells).
func psColIndex(t *testing.T, tbl reports.Table, progName string) int {
	t.Helper()
	for i, c := range tbl.Columns {
		if c.HeaderKey == progName {
			return i
		}
	}
	t.Fatalf("program column %q not found in headers", progName)
	return 0
}

// psCell returns the minor amount in column col of the DATA row whose account name (col 0)
// is acctName and currency (col 1) is ccy, plus its Drill and whether found.
func psCell(tbl reports.Table, acctName, ccy string, col int) (int64, *reports.Drill, bool) {
	for _, row := range tbl.Rows {
		if row.Kind != reports.RowData || len(row.Cells) <= col {
			continue
		}
		if row.Cells[0].Text == acctName && row.Cells[1].Text == ccy {
			return row.Cells[col].Minor, row.Cells[col].Drill, true
		}
	}
	return 0, nil, false
}

// psRowByLabel returns col's minor amount in the SUBTOTAL/TOTAL row whose label key (col 0)
// is labelKey and currency (col 1) is ccy.
func psRowByLabel(t *testing.T, tbl reports.Table, labelKey, ccy string, col int) int64 {
	t.Helper()
	for _, row := range tbl.Rows {
		if len(row.Cells) <= col {
			continue
		}
		if row.Cells[0].Kind == reports.CellLabel && row.Cells[0].Text == labelKey && row.Cells[1].Text == ccy {
			return row.Cells[col].Minor
		}
	}
	t.Fatalf("labeled row %q (%s) not found", labelKey, ccy)
	return 0
}

// TestProgramStatementGolden runs the comparative statement and asserts the per-program
// per-account ROLLED cells, the tree rollup discriminators (General folds in its
// descendants), and the net-per-program line BY HAND, then compares the rendered text +
// CSV to committed goldens.
func TestProgramStatementGolden(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := psReport(t)

	p := psParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run program statement: %v", err)
	}

	gen := psColIndex(t, table, "General")
	edu := psColIndex(t, table, "Educacion")
	fp := psColIndex(t, table, "Food Pantry")

	// Per-program per-account ROLLED cells (revenue shown +, expense shown +).
	type cellWant struct {
		acct, ccy string
		col       int
		want      int64
	}
	for _, tc := range []cellWant{
		// General column — revenue (positive), incl. the rolled-in children.
		{"Contributions", "USD", gen, 5_275_000},
		{"Government Grants", "MXN", gen, 10_000_000},
		{"Government Grants", "USD", gen, 200_000},
		{"Program Service Fees", "USD", gen, 120_000},
		// General column — expense, ROLLUP discriminators (fold in Educación / Food Pantry).
		{"Program Supplies", "USD", gen, 210_000}, // 60,000 General + 150,000 Educación
		{"Program Supplies", "MXN", gen, 500_000}, // only Educación
		{"Food Purchases", "MXN", gen, 360_000},   // 210,000 General + 150,000 Food Pantry
		{"Salaries", "USD", gen, 1_650_000},
		// Educación (leaf) column.
		{"Program Service Fees", "USD", edu, 120_000},
		{"Program Supplies", "USD", edu, 150_000},
		{"Program Supplies", "MXN", edu, 500_000},
		{"Government Grants", "MXN", edu, 10_000_000},
		// Food Pantry (leaf) column.
		{"Food Purchases", "MXN", fp, 150_000},
	} {
		got, _, ok := psCell(table, tc.acct, tc.ccy, tc.col)
		if !ok || got != tc.want {
			t.Errorf("cell %s/%s col %d = %d/%v, want %d", tc.acct, tc.ccy, tc.col, got, ok, tc.want)
		}
	}

	// Food Pantry has NO revenue and no USD/other-expense activity beyond Food Purchases:
	// its Program Supplies cell must be absent (no row) OR zero — assert the Salaries row's
	// Food Pantry cell is 0 (General-only account, absent from Food Pantry's rolled set).
	if v, _, ok := psCell(table, "Salaries", "USD", fp); ok && v != 0 {
		t.Errorf("Food Pantry Salaries USD = %d, want 0 (no program activity)", v)
	}

	// Net per program, per currency = revenue − expenses.
	netKey := "reports.program_statement.net"
	for _, tc := range []struct {
		ccy      string
		col      int
		wantNet  int64
		progName string
	}{
		{"USD", gen, 3_567_500, "General"},   // == p15.9 chTotal (org R/E activity)
		{"MXN", gen, 9_140_000, "General"},   // == p15.9 chTotal
		{"USD", edu, 170_000, "Educacion"},   // 320,000 rev − 150,000 exp
		{"MXN", edu, 9_500_000, "Educacion"}, // 10,000,000 rev − 500,000 exp
		{"MXN", fp, -150_000, "Food Pantry"}, // 0 rev − 150,000 exp
	} {
		if got := psRowByLabel(t, table, netKey, tc.ccy, tc.col); got != tc.wantNet {
			t.Errorf("net %s (%s) = %d, want %d", tc.progName, tc.ccy, got, tc.wantNet)
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

// TestProgramStatementRollupCorrectness: a parent program's rolled column == Σ (its own +
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
	gen := psColIndex(t, table, "General")

	// Independently sum the RAW oracle per (account, currency) across ALL programs — since
	// General is the root, its rolled column must equal this org-wide raw sum.
	type ac struct {
		acct int64
		ccy  string
	}
	rawTotal := map[ac]int64{}
	for _, pc := range f.Expected.Program {
		rawTotal[ac{pc.Account, pc.Currency}] += pc.Amount
	}

	// Account id -> name (for locating the table row).
	names := psAccountNames(t, f)

	for k, wantRaw := range rawTotal {
		name := names[k.acct]
		// The row's displayed sign depends on the account type: revenue is shown ×−1,
		// expense ×+1. Recover the raw net-debit from the displayed cell by matching the
		// oracle sign convention: revenue oracle amounts are NEGATIVE, expense POSITIVE.
		got, _, ok := psCell(table, name, k.ccy, gen)
		if !ok {
			t.Errorf("General column missing %s/%s (raw total %d)", name, k.ccy, wantRaw)
			continue
		}
		// Displayed value is |raw| (revenue −raw shown positive; expense +raw). Compare
		// magnitudes so the check is sign-convention-independent.
		if got != absInt(wantRaw) {
			t.Errorf("General rolled %s/%s = %d, want |raw org total| %d", name, k.ccy, got, absInt(wantRaw))
		}
	}
}

// TestProgramStatementRawPartition: the RAW partition (General-direct + Educación + Food
// Pantry) reconciles to the total R/E activity per currency — the task's "sum across
// programs reconciles to total R/E" check, done at the ORACLE level (summing the RAW,
// non-rolled per-program cells, which do NOT double-count).
func TestProgramStatementRawPartition(t *testing.T) {
	f := fixture.New(t)

	// Total R/E net-debit per currency from the account-balances oracle (revenue +
	// expense leaves). Independently: USD net-debit = expenses − revenue-magnitude.
	// Simplest independent tally: sum the RAW per-program oracle across the LEAF programs
	// AND General-direct == every program cell (raw) — which partitions the total.
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

// TestProgramStatementSingleSubtree: the ?program= view shows ONE program (rolled up), with
// the Account | Currency | Amount layout and a net row. Choosing General shows the same
// rolled figures as its comparative column.
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
	// Exactly ONE program column (Account, Currency, Amount => 3 columns).
	if len(table.Columns) != 3 {
		t.Fatalf("single view has %d columns, want 3 (Account, Currency, Amount)", len(table.Columns))
	}
	// The single column is index 2. Educación's rolled cells == its leaf oracle.
	for _, tc := range []struct {
		acct, ccy string
		want      int64
	}{
		{"Program Service Fees", "USD", 120_000},
		{"Program Supplies", "USD", 150_000},
		{"Program Supplies", "MXN", 500_000},
		{"Government Grants", "MXN", 10_000_000},
	} {
		got, _, ok := psCell(table, tc.acct, tc.ccy, 2)
		if !ok || got != tc.want {
			t.Errorf("single Educación %s/%s = %d/%v, want %d", tc.acct, tc.ccy, got, ok, tc.want)
		}
	}
	// Net per currency.
	netKey := "reports.program_statement.net"
	if got := psRowByLabel(t, table, netKey, "USD", 2); got != 170_000 {
		t.Errorf("single Educación net USD = %d, want 170,000", got)
	}
	if got := psRowByLabel(t, table, netKey, "MXN", 2); got != 9_500_000 {
		t.Errorf("single Educación net MXN = %d, want 9,500,000", got)
	}
}

// TestProgramStatementGrantProgramScope (p27.4): a program-SUBTREE-scoped report grant
// restricts the report's rows to the granted subtree. Scoping to Educacion (a leaf, so
// the subtree is just itself) must show Educacion's activity and MUST NOT leak the
// sibling Food Pantry subtree -- crucially even in the ROLLED General (root) column,
// which without the filter folds in every program. This proves the filter is applied to
// the RAW split program BEFORE the ancestor rollup (a sibling never contributes to any
// cell, incl. an ancestor's).
func TestProgramStatementGrantProgramScope(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := psReport(t)

	// Baseline (UNSCOPED): the General column folds in Food Pantry's Food Purchases MXN
	// (210,000 General-direct + 150,000 Food Pantry = 360,000) and General-direct-only
	// Salaries -- confirming the sibling IS present without a scope (the goldens do not
	// move; the scope is purely additive).
	base := psParams(f)
	baseT, err := rep.Run(ctx, reports.NewToolkit(f.Store, base), base)
	if err != nil {
		t.Fatalf("run unscoped: %v", err)
	}
	genBase := psColIndex(t, baseT, "General")
	if got, _, ok := psCell(baseT, "Food Purchases", "MXN", genBase); !ok || got != 360_000 {
		t.Fatalf("unscoped General Food Purchases MXN = %d/%v, want 360,000 (sibling present)", got, ok)
	}

	// Scope the grant to Educacion (a leaf subtree = {Educacion}). Food Pantry is a
	// SIBLING and must vanish from every column.
	p := psParams(f)
	p.ProgramScope = []reports.ProgramID{reports.ProgramID(f.IDs.Educacion)}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run scoped: %v", err)
	}
	gen := psColIndex(t, table, "General")

	// The rolled General column now reflects ONLY Educacion's activity (the root folds in
	// its in-scope descendants; Food Pantry is filtered out at the raw split).
	//   - Food Purchases MXN: General-direct 210,000 is a General-program split, NOT in the
	//     Educacion subtree, so it is filtered too -> the cell is absent/0 (Food Pantry's
	//     150,000 certainly does not leak). Assert NO Food Pantry leak: General MXN Food
	//     Purchases is either absent or, if present, does not include the 150,000/360,000.
	if got, _, ok := psCell(table, "Food Purchases", "MXN", gen); ok && got != 0 {
		t.Errorf("scoped General Food Purchases MXN = %d, want absent/0 (no General-direct or sibling leak)", got)
	}
	//   - Salaries is a General-direct (non-Educacion) expense: filtered out -> absent/0.
	if got, _, ok := psCell(table, "Salaries", "USD", gen); ok && got != 0 {
		t.Errorf("scoped General Salaries USD = %d, want absent/0 (out-of-subtree)", got)
	}
	//   - Educacion's OWN activity DOES appear in the (now Educacion-only) General column:
	//     Program Supplies MXN 500,000, Government Grants MXN 10,000,000.
	if got, _, ok := psCell(table, "Program Supplies", "MXN", gen); !ok || got != 500_000 {
		t.Errorf("scoped General Program Supplies MXN = %d/%v, want 500,000 (Educacion in scope)", got, ok)
	}
	if got, _, ok := psCell(table, "Government Grants", "MXN", gen); !ok || got != 10_000_000 {
		t.Errorf("scoped General Government Grants MXN = %d/%v, want 10,000,000 (Educacion in scope)", got, ok)
	}
	// General net == Educacion net (the root now equals the single in-scope subtree).
	netKey := "reports.program_statement.net"
	if got := psRowByLabel(t, table, netKey, "MXN", gen); got != 9_500_000 {
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
	edu := psColIndex(t, table, "Educacion")

	_, d, ok := psCell(table, "Program Supplies", "MXN", edu)
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
// drills via the program SET (Drill.ProgramIDs = the subtree), unioning the per-program
// split sets, and the drilled native sum equals the rolled figure. General × Program
// Supplies × USD = 210,000 = 60,000 (General-direct) + 150,000 (Educación) + 0 (Food Pantry).
func TestProgramStatementRollupDrillReconciles(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := psReport(t)
	p := psParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	gen := psColIndex(t, table, "General")

	_, d, ok := psCell(table, "Program Supplies", "USD", gen)
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
// localized header (Account, Currency, then the program names).
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
	wantHeader := []string{"Account", "Currency", "General", "Educacion", "Food Pantry"}
	for i, h := range wantHeader {
		if recs[0][i] != h {
			t.Errorf("csv header[%d] = %q, want %q", i, recs[0][i], h)
		}
	}
}

// psRow returns the full Row (kind, indent, cells) for the row whose account/label cell
// (col 0) is name AND currency cell (col 1) is ccy, and whether found. Matches TEXT
// (account name) and LABEL (section) rows alike on the raw first-cell string.
func psRow(tbl reports.Table, name, ccy string) (reports.Row, bool) {
	for _, row := range tbl.Rows {
		if len(row.Cells) < 2 {
			continue
		}
		if row.Cells[0].Text == name && row.Cells[1].Text == ccy {
			return row, true
		}
	}
	return reports.Row{}, false
}

// TestProgramStatementCollapsibleTree (p29.15): the statement is a COLLAPSIBLE ACCOUNT
// TREE. The report registers Tree: true (so the web layer emits data-depth + the
// collapse/expand controls), and a placeholder PARENT account (the "Expenses" section
// parent) renders as a roll-up SUBTOTAL row over its indented leaf children — WITHIN a
// currency block, its per-program cell == the sum of that currency's leaves. The subtotal
// row is NOT drillable (a rollup spans many leaves). This is the SoA machinery, now on the
// program statement.
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
	gen := psColIndex(t, table, "General")

	// The "Expenses" placeholder parent renders as a SUBTOTAL row in each currency block,
	// at tree depth 0 (a top-level R/E section parent -> Indent 0), and its cell is NOT a
	// drill link (rollups aren't drillable).
	for _, blk := range []struct {
		ccy       string
		wantExp   int64 // General column Expenses subtotal for this currency
		leafIn    string
		leafAmt   int64
		leafOutOf string // a leaf that must be ABSENT from this currency block
	}{
		// USD block: Expenses $23,275.00 = Σ children (Salaries $16,500 + Program Supplies
		// $2,100 + Occupancy $3,050 + Insurance $600 + Bank Fees $25 + Event Costs $1,000);
		// Food Purchases (MXN-only) must NOT appear here.
		{"USD", 2_327_500, "Salaries", 1_650_000, "Food Purchases"},
		// MXN block: Expenses $8,600.00 = Program Supplies $5,000 + Food Purchases $3,600;
		// Salaries (USD-only) must NOT appear here (the per-currency fold).
		{"MXN", 860_000, "Program Supplies", 500_000, "Salaries"},
	} {
		row, ok := psRow(table, "Expenses", blk.ccy)
		if !ok {
			t.Fatalf("%s block: Expenses placeholder subtotal row missing", blk.ccy)
		}
		if row.Kind != reports.RowSubtotal {
			t.Errorf("%s Expenses parent kind = %v, want RowSubtotal", blk.ccy, row.Kind)
		}
		if row.Indent != 0 {
			t.Errorf("%s Expenses parent indent = %d, want 0 (top-level section parent)", blk.ccy, row.Indent)
		}
		if row.Cells[gen].Minor != blk.wantExp {
			t.Errorf("%s Expenses subtotal General = %d, want %d (Σ children)", blk.ccy, row.Cells[gen].Minor, blk.wantExp)
		}
		if row.Cells[gen].Drill != nil {
			t.Errorf("%s Expenses subtotal cell is drillable; a rollup must not drill", blk.ccy)
		}
		// The in-currency leaf is present and nests one level deeper (Indent parent+1); the
		// out-of-currency leaf is ABSENT from this block.
		leaf, ok := psRow(table, blk.leafIn, blk.ccy)
		if !ok {
			t.Fatalf("%s block: leaf %q missing", blk.ccy, blk.leafIn)
		}
		if leaf.Indent != row.Indent+1 {
			t.Errorf("%s leaf %q indent = %d, want parent+1 (%d)", blk.ccy, blk.leafIn, leaf.Indent, row.Indent+1)
		}
		if leaf.Cells[gen].Minor != blk.leafAmt {
			t.Errorf("%s leaf %q General = %d, want %d", blk.ccy, blk.leafIn, leaf.Cells[gen].Minor, blk.leafAmt)
		}
		if _, present := psRow(table, blk.leafOutOf, blk.ccy); present {
			t.Errorf("%s block leaks out-of-currency leaf %q (per-currency fold broke)", blk.ccy, blk.leafOutOf)
		}
	}
}

// TestProgramStatementNestedSubtotal (p29.15): a MULTI-LEVEL placeholder parent (a grouping
// account nested UNDER the Expenses section) renders as a nested subtotal — at depth 2,
// contiguous ABOVE its own children — whose per-program cell == the sum of its SAME-CURRENCY
// leaves. The base fixture R/E tree is FLAT (section -> leaves), so this path is exercised
// inline: create "Field Ops" (placeholder) under Expenses with a USD and an MXN leaf, post an
// expense to each (General program), and assert the nested rollup arithmetic + depth + order.
func TestProgramStatementNestedSubtotal(t *testing.T) {
	f := fixture.New(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	rep := psReport(t)

	prog := "program"
	// "Field Ops" is a placeholder EXPENSE parent under the Expenses section (no leaf txns
	// of its own), with a USD leaf (Fuel) and an MXN leaf (Rations) under it.
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
	gen := psColIndex(t, table, "General")

	// USD block: Field Ops (nested placeholder) subtotal == Fuel leaf (400.00 = 40,000).
	foUSD, ok := psRow(table, "Field Ops", "USD")
	if !ok {
		t.Fatalf("USD block: Field Ops nested subtotal missing")
	}
	if foUSD.Kind != reports.RowSubtotal {
		t.Errorf("Field Ops USD kind = %v, want RowSubtotal", foUSD.Kind)
	}
	// Nested one level below the Expenses section parent (Indent 0) -> Indent 1.
	if foUSD.Indent != 1 {
		t.Errorf("Field Ops USD indent = %d, want 1 (nested under Expenses)", foUSD.Indent)
	}
	fuelUSD, ok := psRow(table, "Fuel", "USD")
	if !ok {
		t.Fatalf("USD block: Fuel leaf missing")
	}
	if fuelUSD.Indent != foUSD.Indent+1 {
		t.Errorf("Fuel indent = %d, want Field Ops+1 (%d)", fuelUSD.Indent, foUSD.Indent+1)
	}
	if foUSD.Cells[gen].Minor != 40_000 || fuelUSD.Cells[gen].Minor != 40_000 {
		t.Errorf("Field Ops USD subtotal = %d, Fuel = %d, want both 40,000 (rollup == same-currency child)",
			foUSD.Cells[gen].Minor, fuelUSD.Cells[gen].Minor)
	}
	// The MXN-only Rations leaf must NOT appear in the USD block (per-currency fold).
	if _, present := psRow(table, "Rations", "USD"); present {
		t.Errorf("USD block leaks MXN-only Rations leaf")
	}

	// MXN block: Field Ops subtotal == Rations leaf (900.00 = 90,000); Fuel (USD) absent.
	foMXN, ok := psRow(table, "Field Ops", "MXN")
	if !ok {
		t.Fatalf("MXN block: Field Ops nested subtotal missing")
	}
	if foMXN.Cells[gen].Minor != 90_000 {
		t.Errorf("Field Ops MXN subtotal General = %d, want 90,000 (== Rations)", foMXN.Cells[gen].Minor)
	}
	if _, present := psRow(table, "Fuel", "MXN"); present {
		t.Errorf("MXN block leaks USD-only Fuel leaf")
	}

	// CONTIGUITY (the treetable data-depth contract): Field Ops (depth 1) must be
	// IMMEDIATELY followed by its deeper children (depth 2) until a row at depth <= 1 — no
	// same-or-shallower row interleaves. Walk the USD block's rows from Field Ops.
	idx := -1
	for i, row := range table.Rows {
		if len(row.Cells) >= 2 && row.Cells[0].Text == "Field Ops" && row.Cells[1].Text == "USD" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("Field Ops USD row not located for contiguity check")
	}
	// The immediately following row is its child (deeper): Fuel at Indent 2.
	next := table.Rows[idx+1]
	if next.Indent <= foUSD.Indent {
		t.Errorf("row after Field Ops has indent %d <= parent %d — subtree not contiguous", next.Indent, foUSD.Indent)
	}
}

// --- helpers ----------------------------------------------------------------

// psAccountNames returns account id -> resolved (en) name from the store tree.
func psAccountNames(t *testing.T, f *fixture.Fixture) map[int64]string {
	t.Helper()
	tree, err := f.Store.Tree(context.Background(), "en", nil)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	m := make(map[int64]string, len(tree))
	for _, r := range tree {
		m[r.ID] = r.Name
	}
	return m
}

// psDrillSum mirrors the web drill handler: it loops the account SET × the program SET
// (Drill.ProgramIDs), summing the signed splits each (account, program) filter selects.
// When ProgramIDs is empty it falls back to the single ProgramID (the leaf drill shape).
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
