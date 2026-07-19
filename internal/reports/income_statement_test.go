package reports_test

// p15.5 income-statement (statement of activities) report tests. Every asserted number
// is HAND-DERIVED from the canonical synthetic fixture (PLAN Appendix D,
// internal/testutil/fixture) and the p14.1 monthly USD->MXN rate seam -- the fixture is
// the oracle, never the report's own output. The golden files
// (testdata/income_statement.{txt,csv}) are a committed, human-reviewable rendering;
// -update / `make golden` regenerate them deterministically (lang=en, root scope, period
// 2025-01-01..2026-06-30, QUARTERLY comparative columns, USD target, txn-date rates).
//
// The income statement is a FLOW: Revenue then Expense, converted at each transaction's
// DATE rate (D12 RateTxnDate), net surplus/deficit = Revenue - Expense. The comparative
// columns are the sub-periods; the TOTAL column is the SUM of the period columns per row
// (footing rule -- see income_statement.go). Signs: revenue activity is net-debit
// negative, presented POSITIVE (an inflow); expense positive; a surplus positive.
//
// HAND-VERIFIED (root scope, USD target, period 2025-01..2026-06, QUARTERLY, txn-date):
//   - native R/E net (rate-independent oracle): USD -3,567,500 and MXN -9,140,000
//     (net-debit; a surplus is a net credit), presented positive as USD 3,567,500 /
//     MXN 9,140,000 -- exactly NetIncome native per currency;
//   - converted TOTAL column (sum of the 6 quarterly columns, txn-date):
//       Contributions        -5,275,000  -> +5,275,000  (USD, 1:1)
//       Government Grants       -781,594  -> +  781,594
//       Program Service Fees    -120,000  -> +  120,000
//       Event Income            -300,000  -> +  300,000
//       Total revenue                        +6,476,594
//       Salaries              +1,650,000
//       Program Supplies        +238,971
//       Food Purchases          +  20,616  (txn-date; NOT the closing 19,890)
//       Occupancy               + 305,000
//       Insurance               +  60,000
//       Bank Fees               +   2,500
//       Event Costs             + 100,000
//       Total expenses                       +2,377,087
//       NET surplus  = 6,476,594 - 2,377,087 = 4,099,507
//   - the NET (4,099,507) is the sum-of-periods total; it differs by ONE minor unit from
//     a whole-range NetIncome-txn-date (4,099,506) because per-period half-even rounding
//     accumulates differently than a single whole-range round -- the statement MUST foot,
//     so the sum-of-periods figure is authoritative (D-note p15.5). The native surplus
//     (USD 3,567,500 / MXN 9,140,000) is the exact, rate-independent cross-check.

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/testutil/fixture"
)

// isGoldenParams: root scope, full fixture span, quarterly columns, USD target, lang en.
func isGoldenParams(f *fixture.Fixture) reports.Params {
	return reports.Params{
		Scope:          reports.SubsidiaryID(f.IDs.Root),
		From:           f.Expected.ActivityFrom, // 2025-01-01
		To:             f.Expected.ActivityTo,   // 2026-06-30
		Granularity:    reports.GranQuarter,
		TargetCurrency: "USD",
		Lang:           "en",
	}
}

// incomeStatementReport fetches the registered income-statement report from Default().
func incomeStatementReport(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.IncomeStatementReportID)
	if !ok {
		t.Fatalf("income-statement report %q not registered in Default()", reports.IncomeStatementReportID)
	}
	return rep
}

// isTotalFor returns the TOTAL-column (last cell) minor amount for the row whose FIRST
// cell text/label equals key, and whether found. Works for account (TEXT) and
// label/subtotal (LABEL) rows alike (matches on the raw first-cell string).
func isTotalFor(t reports.Table, key string) (int64, bool) {
	for _, row := range t.Rows {
		if len(row.Cells) < 2 {
			continue
		}
		if row.Cells[0].Text == key {
			return row.Cells[len(row.Cells)-1].Minor, true
		}
	}
	return 0, false
}

// isRowFor returns the full cell slice for the row whose first-cell string equals key.
func isRowFor(t reports.Table, key string) ([]reports.Cell, bool) {
	for _, row := range t.Rows {
		if len(row.Cells) > 0 && row.Cells[0].Text == key {
			return row.Cells, true
		}
	}
	return nil, false
}

// isKindFor returns the RowKind of the row whose first-cell string equals key, and
// whether found. Used to assert the p30.10 three-tier total distinction: placeholder
// parents are RowSubtotal, section totals RowSectionTotal, the net RowTotal.
func isKindFor(t reports.Table, key string) (reports.RowKind, bool) {
	for _, row := range t.Rows {
		if len(row.Cells) > 0 && row.Cells[0].Text == key {
			return row.Kind, true
		}
	}
	return 0, false
}

// TestIncomeStatementGolden runs the income statement over the fixture at the pinned
// params, hand-verifies the R/E tree subtotals + the net surplus + txn-date conversion,
// checks the comparative columns foot to the total column, and compares the rendered
// text + CSV to committed goldens.
func TestIncomeStatementGolden(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()

	rep := incomeStatementReport(t)
	p := isGoldenParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run income statement: %v", err)
	}

	// --- The converted TOTAL column per line (hand-derived, sum-of-quarters, txn-date).
	wantTotal := map[string]int64{
		"Contributions":        5_275_000,
		"Government Grants":    781_594,
		"Program Service Fees": 120_000,
		"Event Income":         300_000,
		"Salaries":             1_650_000,
		"Program Supplies":     238_971,
		"Food Purchases":       20_616, // txn-date; discriminator below asserts != closing 19,890
		"Occupancy":            305_000,
		"Insurance":            60_000,
		"Bank Fees":            2_500,
		"Event Costs":          100_000,
	}
	for name, want := range wantTotal {
		got, ok := isTotalFor(table, name)
		if !ok {
			t.Errorf("no row for account %q", name)
			continue
		}
		if got != want {
			t.Errorf("total column %q = %d, want %d", name, got, want)
		}
	}

	// --- The R/E TREE SUBTOTALS (placeholder parents "Revenue"/"Expenses" as subtotal
	// rows) preserve the tree: Total revenue = Σ revenue leaves, Total expenses = Σ
	// expense leaves.
	if got, _ := isTotalFor(table, "reports.income_statement.total.revenue"); got != 6_476_594 {
		t.Errorf("Total revenue = %d, want 6476594", got)
	}
	if got, _ := isTotalFor(table, "reports.income_statement.total.expenses"); got != 2_377_087 {
		t.Errorf("Total expenses = %d, want 2377087", got)
	}
	// The placeholder PARENT rows (proper-noun account names) carry the same subtree sums.
	if got, ok := isTotalFor(table, "Revenue"); !ok || got != 6_476_594 {
		t.Errorf("Revenue parent subtotal = %d/%v, want 6476594", got, ok)
	}
	if got, ok := isTotalFor(table, "Expenses"); !ok || got != 2_377_087 {
		t.Errorf("Expenses parent subtotal = %d/%v, want 2377087", got, ok)
	}

	// --- p30.10 THREE DISTINCT TOTAL TIERS. The "Total revenue"/"Total expenses" SECTION
	// totals are RowSectionTotal (the middle tier — more emphasis than the placeholder
	// parent, less than the grand total), the placeholder PARENTS stay RowSubtotal, and the
	// grand "Change in net assets" stays RowTotal. This ranks the three tiers so a single-
	// parent section's total no longer renders identically to its parent.
	for _, key := range []string{"reports.income_statement.total.revenue", "reports.income_statement.total.expenses"} {
		if k, ok := isKindFor(table, key); !ok || k != reports.RowSectionTotal {
			t.Errorf("section total %q kind = %v/%v, want RowSectionTotal", key, k, ok)
		}
	}
	for _, name := range []string{"Revenue", "Expenses"} {
		if k, ok := isKindFor(table, name); !ok || k != reports.RowSubtotal {
			t.Errorf("placeholder parent %q kind = %v/%v, want RowSubtotal", name, k, ok)
		}
	}
	if k, ok := isKindFor(table, "reports.income_statement.net"); !ok || k != reports.RowTotal {
		t.Errorf("net line kind = %v/%v, want RowTotal", k, ok)
	}

	// --- NET surplus = Revenue - Expense = 6,476,594 - 2,377,087 = 4,099,507 (the sum-
	// of-periods total, which the statement MUST foot to).
	net, ok := isTotalFor(table, "reports.income_statement.net")
	if !ok {
		t.Fatalf("no net surplus row")
	}
	if net != 4_099_507 {
		t.Errorf("net surplus (total col) = %d, want 4099507", net)
	}
	if net != 6_476_594-2_377_087 {
		t.Errorf("net %d != revenue - expense (%d)", net, 6_476_594-2_377_087)
	}

	// --- TXN-DATE vs CLOSING discriminator: Food Purchases converted at txn-date rates
	// is 20,616, NOT the closing-rate 19,890 (the report is a FLOW, D12). This proves the
	// conversion MODE.
	fp, _ := isTotalFor(table, "Food Purchases")
	if fp == 19_890 {
		t.Errorf("Food Purchases converted at CLOSING rate (19,890); must be txn-date (20,616)")
	}
	if fp != 20_616 {
		t.Errorf("Food Purchases txn-date total = %d, want 20616", fp)
	}

	// --- Golden artifacts.
	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "income_statement.txt", []byte(textDump))
	checkGolden(t, "income_statement.csv", csvBuf.Bytes())
}

// TestIncomeStatementFXGolden is the p31.2 demonstration golden: the Statement of
// Activities WITH the Lempira FX exposure (ExtendFX) grows an "FX remeasurement
// gain/(loss)" line, and the "change in net assets" total = Total revenue − Total
// expenses + that FX line. A single-column (GranNone) render keeps the golden compact and
// puts the new line in plain view for review.
func TestIncomeStatementFXGolden(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	f.ExtendFX(t)
	ctx := context.Background()

	rep := incomeStatementReport(t)
	p := isGoldenParams(f)
	p.Granularity = reports.GranNone
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run income statement (FX): %v", err)
	}

	// The FX line carries the −46,174 remeasurement loss.
	fxLine, ok := isTotalFor(table, "reports.income_statement.fx_gain_loss")
	if !ok {
		t.Fatal("FX line missing from statement with exposure")
	}
	if fxLine != f.Expected.FX.RemeasurementUSDMinor {
		t.Errorf("FX line = %d, want %d", fxLine, f.Expected.FX.RemeasurementUSDMinor)
	}
	// Change in net assets folds in the FX line: net == (revenue − expenses) + FX.
	rev, _ := isTotalFor(table, "reports.income_statement.total.revenue")
	exp, _ := isTotalFor(table, "reports.income_statement.total.expenses")
	net, _ := isTotalFor(table, "reports.income_statement.net")
	if net != rev-exp+fxLine {
		t.Errorf("net %d != revenue − expenses + FX (%d − %d + %d = %d)",
			net, rev, exp, fxLine, rev-exp+fxLine)
	}

	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "income_statement_fx.txt", []byte(textDump))
	checkGolden(t, "income_statement_fx.csv", csvBuf.Bytes())
}

// TestIncomeStatementGranNone exercises the DEFAULT (no-granularity) path: an absent
// granularity param resolves to GranNone (web resolveParams), which the report renders as
// a single "Period" column + the Total column (3 columns: Line, Period, Total). The
// single period column equals the total column (one bucket), and the net + section labels
// are present -- the whole-range statement reads correctly without comparative columns.
func TestIncomeStatementGranNone(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := incomeStatementReport(t)

	p := isGoldenParams(f)
	p.Granularity = reports.GranNone
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run GranNone: %v", err)
	}

	// Line + one Period column + Total = 3 columns.
	if len(table.Columns) != 3 {
		t.Fatalf("GranNone columns = %d, want 3 (Line, Period, Total)", len(table.Columns))
	}
	if table.Columns[1].HeaderKey != "reports.income_statement.col.period" {
		t.Errorf("GranNone period column header = %q, want the col.period key", table.Columns[1].HeaderKey)
	}

	// The single period column equals the total column, per money row (one bucket).
	for _, row := range table.Rows {
		if len(row.Cells) != 3 {
			continue
		}
		per, tot := row.Cells[1], row.Cells[2]
		if per.Kind == reports.CellMoney && !per.Blank && tot.Kind == reports.CellMoney && !tot.Blank {
			if per.Minor != tot.Minor {
				t.Errorf("GranNone row %q: period %d != total %d", row.Cells[0].Text, per.Minor, tot.Minor)
			}
		}
	}

	// Net + the whole-range figures match the comparative view (same underlying activity).
	if net, ok := isTotalFor(table, "reports.income_statement.net"); !ok || net != 4_099_506 {
		// GranNone rounds the WHOLE range once per account (a single bucket), so the net is
		// the whole-range NetIncome-txn-date 4,099,506 (vs the quarterly sum-of-periods
		// 4,099,507 -- the one-minor-unit rounding-grain difference, expected).
		t.Errorf("GranNone net = %d/%v, want 4099506 (whole-range single-bucket rounding)", net, ok)
	}
}

// TestIncomeStatementComparativeColumns proves the footing invariant: for EVERY row, the
// sum of the comparative (period) columns equals the total column. This is structural
// (built by adding the per-period converted cells), so it holds exactly for every row --
// data, subtotal, and net alike.
func TestIncomeStatementComparativeColumns(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := incomeStatementReport(t)
	p := isGoldenParams(f)

	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Quarterly over 18 months => 6 period columns; columns = Line + 6 + Total = 8.
	if len(table.Columns) != 8 {
		t.Fatalf("columns = %d, want 8 (Line + 6 quarters + Total)", len(table.Columns))
	}
	nPeriods := len(table.Columns) - 2 // minus Line and Total

	for _, row := range table.Rows {
		if len(row.Cells) != len(table.Columns) {
			continue
		}
		// Sum the period money cells (columns 1..nPeriods) and compare to the total (last).
		var sum int64
		anyMoney := false
		for c := 1; c <= nPeriods; c++ {
			cell := row.Cells[c]
			if cell.Kind == reports.CellMoney && !cell.Blank {
				sum += cell.Minor
				anyMoney = true
			}
		}
		last := row.Cells[len(row.Cells)-1]
		if last.Kind != reports.CellMoney || last.Blank {
			continue // a heading row (blank money cells)
		}
		if !anyMoney {
			continue
		}
		if sum != last.Minor {
			t.Errorf("row %q: Σ period columns (%d) != total column (%d)", row.Cells[0].Text, sum, last.Minor)
		}
	}
}

// TestIncomeStatementNativeNet cross-checks the NET against the fixture's exact,
// RATE-INDEPENDENT R/E oracle: native USD -3,567,500 and MXN -9,140,000 (net-debit),
// which is precisely NetIncome native per currency. This proves the sign handling and
// the R/E account selection independent of any FX rounding.
func TestIncomeStatementNativeNet(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	tk := reports.NewToolkit(f.Store, isGoldenParams(f))

	// The fixture R/E oracle (from Expected.AccountBalances, which at AsOf == full-span
	// activity for these flow accounts): revenue credits + expense debits.
	// Native USD net and MXN net.
	nat, err := tk.Activity(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)},
		f.Expected.ActivityFrom, f.Expected.ActivityTo, reports.ConvertOpts{Mode: reports.RateNone})
	if err != nil {
		t.Fatalf("activity native: %v", err)
	}
	reIDs := []reports.AccountID{
		f.IDs.Contributions, f.IDs.GovernmentGrants, f.IDs.ProgramFees, f.IDs.EventIncome,
		f.IDs.Salaries, f.IDs.ProgramSupplies, f.IDs.FoodPurchases, f.IDs.Occupancy,
		f.IDs.Insurance, f.IDs.BankFees, f.IDs.EventCosts,
	}
	var usd, mxn int64
	for _, id := range reIDs {
		for _, a := range nat[reports.AccountID(id)] {
			switch a.Currency {
			case "USD":
				usd += a.Minor
			case "MXN":
				mxn += a.Minor
			}
		}
	}
	if usd != -3_567_500 {
		t.Errorf("native USD R/E net = %d, want -3567500", usd)
	}
	if mxn != -9_140_000 {
		t.Errorf("native MXN R/E net = %d, want -9140000", mxn)
	}

	// NetIncome native per currency agrees (the toolkit oracle).
	ni, err := tk.NetIncome(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)},
		f.Expected.ActivityFrom, f.Expected.ActivityTo, reports.ConvertOpts{Mode: reports.RateTxnDate, To: "USD"})
	if err != nil {
		t.Fatalf("netincome: %v", err)
	}
	// The report's net (sum-of-periods) is within a minor unit of the whole-range
	// NetIncome (the rounding grain difference). Presented positive = -ni.Minor.
	rep := incomeStatementReport(t)
	p := isGoldenParams(f)
	table, _ := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	net, _ := isTotalFor(table, "reports.income_statement.net")
	if diff := net - (-ni.Minor); diff < -2 || diff > 2 {
		t.Errorf("report net %d not within rounding of NetIncome-derived %d", net, -ni.Minor)
	}
}

// TestIncomeStatementScope: root vs a leaf sub (RV Mexico) differ. RV Mexico (leaf,
// descendant closure D18) carries only MX-posted R/E; US-only revenue (Contributions,
// USD) does not appear in the leaf. Native-independent (structural presence).
func TestIncomeStatementScope(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := incomeStatementReport(t)

	rootP := isGoldenParams(f)
	rootT, err := rep.Run(ctx, reports.NewToolkit(f.Store, rootP), rootP)
	if err != nil {
		t.Fatalf("run root: %v", err)
	}
	leafP := rootP
	leafP.Scope = reports.SubsidiaryID(f.IDs.MX)
	leafT, err := rep.Run(ctx, reports.NewToolkit(f.Store, leafP), leafP)
	if err != nil {
		t.Fatalf("run leaf: %v", err)
	}

	// Contributions (USD, posted at the US sub) appears at root, NOT in the MX leaf.
	if _, ok := isRowFor(rootT, "Contributions"); !ok {
		t.Errorf("root income statement missing Contributions")
	}
	if _, ok := isRowFor(leafT, "Contributions"); ok {
		t.Errorf("leaf(MX) income statement unexpectedly contains US-posted Contributions")
	}
	// The leaf still has expense/revenue activity (Food Purchases is MXN, posted at MX).
	if _, ok := isRowFor(leafT, "Food Purchases"); !ok {
		t.Errorf("leaf(MX) income statement missing MX-posted Food Purchases")
	}
	// Root and leaf net differ.
	rootNet, _ := isTotalFor(rootT, "reports.income_statement.net")
	leafNet, _ := isTotalFor(leafT, "reports.income_statement.net")
	if rootNet == leafNet {
		t.Errorf("root net (%d) == leaf net (%d); scopes must differ", rootNet, leafNet)
	}
}

// TestIncomeStatementDrill: a single-native-currency leaf activity cell carries a
// DrillPeriod filter (that column's sub-period range + native currency); a subtotal row
// does not. The reconciliation (drilled native splits sum == native figure) is exercised
// against the real store in the web package's reports_drill test; here we assert the
// filter is WIRED correctly.
func TestIncomeStatementDrill(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := incomeStatementReport(t)
	p := isGoldenParams(f)

	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Contributions is single-currency (USD): its Total cell drills over the whole range.
	cells, ok := isRowFor(table, "Contributions")
	if !ok {
		t.Fatalf("no Contributions row")
	}
	tot := cells[len(cells)-1]
	if tot.Drill == nil {
		t.Fatalf("Contributions total cell is not drillable")
	}
	if tot.Drill.Mode != reports.DrillPeriod {
		t.Errorf("Contributions drill mode = %v, want DrillPeriod", tot.Drill.Mode)
	}
	if tot.Drill.From != f.Expected.ActivityFrom || tot.Drill.To != f.Expected.ActivityTo {
		t.Errorf("Contributions total drill range = %s..%s, want whole span", tot.Drill.From, tot.Drill.To)
	}
	if tot.Drill.Currency != "USD" || len(tot.Drill.AccountIDs) != 1 || tot.Drill.AccountIDs[0] != f.IDs.Contributions {
		t.Errorf("Contributions drill filter wrong: %+v", tot.Drill)
	}
	// A period cell (column 1) drills to THAT quarter's range.
	q1 := cells[1]
	if q1.Drill == nil {
		t.Errorf("Contributions period cell not drillable")
	} else if q1.Drill.From != "2025-01-01" || q1.Drill.To != "2025-03-31" {
		t.Errorf("Contributions Q1 drill range = %s..%s, want 2025-01-01..2025-03-31", q1.Drill.From, q1.Drill.To)
	}

	// The Total-revenue SUBTOTAL row is NOT drillable (a rollup over many leaves).
	subCells, ok := isRowFor(table, "reports.income_statement.total.revenue")
	if !ok {
		t.Fatalf("no total-revenue row")
	}
	for _, c := range subCells {
		if c.Drill != nil {
			t.Errorf("total-revenue subtotal cell is drillable; must not be")
		}
	}
}

// TestIncomeStatementGrantProgramScope (p27.4b): a program-scoped report grant filters
// the income statement's R/E rows to the granted program SUBTREE, so a SIBLING subtree's
// activity vanishes from every row -- including the rolled Total-expenses/Total-revenue
// SUBTOTAL rows (the leak a rendered-row filter would miss). Scoped to Educacion (a leaf
// subtree = {Educacion}): only Educacion's Government Grants / Program Service Fees
// (revenue) and Program Supplies (expense) survive; every General-direct account
// (Salaries, Occupancy, Insurance, Bank Fees, Event Costs, Contributions, Event Income)
// and Food Pantry's Food Purchases is dropped. USD target (the fixture default) keeps the
// USD figures exact; the assertions are the presence/absence of accounts + a strict
// drop in the expense subtotal, so they are rate-tolerant.
func TestIncomeStatementGrantProgramScope(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := incomeStatementReport(t)

	base := isGoldenParams(f)
	base.Granularity = reports.GranNone // single Total column; native-currency-independent presence
	baseT, err := rep.Run(ctx, reports.NewToolkit(f.Store, base), base)
	if err != nil {
		t.Fatalf("run unscoped: %v", err)
	}
	// Baseline: the org-wide expense subtotal folds in every expense account.
	baseExp, ok := isTotalFor(baseT, "reports.income_statement.total.expenses")
	if !ok {
		t.Fatalf("unscoped: no total-expenses row")
	}
	if _, ok := isRowFor(baseT, "Salaries"); !ok {
		t.Fatalf("unscoped income statement missing Salaries (sibling present)")
	}

	// Scope the grant to Educacion: Food Pantry + every General-direct account are OUT.
	p := base
	p.ProgramScope = []reports.ProgramID{reports.ProgramID(f.IDs.Educacion)}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run scoped: %v", err)
	}

	// Sibling / General-direct accounts vanish from the table entirely (no data row).
	for _, name := range []string{
		"Salaries", "Occupancy", "Insurance", "Bank Fees", "Event Costs",
		"Contributions", "Event Income", "Food Purchases",
	} {
		if _, ok := isRowFor(table, name); ok {
			t.Errorf("scoped income statement leaks out-of-subtree account %q", name)
		}
	}
	// Educacion's OWN accounts survive.
	for _, name := range []string{"Government Grants", "Program Service Fees", "Program Supplies"} {
		if _, ok := isRowFor(table, name); !ok {
			t.Errorf("scoped income statement dropped in-subtree account %q", name)
		}
	}
	// The rolled Total-expenses SUBTOTAL now reflects ONLY Educacion's expenses (Program
	// Supplies) -- strictly less than the org-wide figure. This is the ROLLED-column
	// no-leak assertion (a General-direct or Food Pantry leak would keep it at/above base).
	scopedExp, ok := isTotalFor(table, "reports.income_statement.total.expenses")
	if !ok {
		t.Fatalf("scoped: no total-expenses row")
	}
	if scopedExp >= baseExp {
		t.Errorf("scoped total-expenses %d >= org-wide %d; a sibling subtree leaked into the rollup", scopedExp, baseExp)
	}
	if scopedExp <= 0 {
		t.Errorf("scoped total-expenses %d; Educacion's Program Supplies should remain", scopedExp)
	}
}
