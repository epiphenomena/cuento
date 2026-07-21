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

// TestIncomeStatementFXMultiColumnFoots pins the single-scan FX-column behavior
// (fxSnapshotsByFunctional): with COMPARATIVE (monthly / quarterly) columns the FX
// gain/loss line is computed from ONE dated scan whose per-boundary snapshots are
// differenced per column. The invariant that batching must satisfy — and that the old
// two-snapshots-per-column path guaranteed by construction — is TELESCOPING:
//
//   - each period column's FX cell sums to the Total column (footing), AND
//   - the sum of the period columns equals the whole-range (GranNone) FX figure.
//
// The ExtendFX fixture recognizes the Lempira remeasurement across the monthly HNL rate
// schedule (2025-01 .. 2026-06), so the loss is spread over MULTIPLE columns — a genuine
// multi-boundary telescoping, not the degenerate single-column case. A reorder or a
// dropped/duplicated boundary in the single-scan accumulation would break footing here.
func TestIncomeStatementFXMultiColumnFoots(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	f.ExtendFX(t)
	ctx := context.Background()
	rep := incomeStatementReport(t)

	// The whole-range (GranNone) FX figure is the telescoping target.
	whole := isGoldenParams(f)
	whole.Granularity = reports.GranNone
	wt, err := rep.Run(ctx, reports.NewToolkit(f.Store, whole), whole)
	if err != nil {
		t.Fatalf("run GranNone: %v", err)
	}
	wholeFX, ok := isTotalFor(wt, "reports.income_statement.fx_gain_loss")
	if !ok {
		t.Fatal("GranNone FX line missing (fixture should have FX exposure)")
	}
	if wholeFX == 0 {
		t.Fatal("GranNone FX figure is 0; the multi-column footing check would be vacuous")
	}

	// For BOTH monthly and quarterly fan-outs, the FX period columns must foot to the Total
	// column AND sum to the whole-range figure.
	for _, gran := range []reports.Granularity{reports.GranMonth, reports.GranQuarter} {
		p := isGoldenParams(f)
		p.Granularity = gran
		table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
		if err != nil {
			t.Fatalf("run gran %v: %v", gran, err)
		}
		fxRow, ok := isRowFor(table, "reports.income_statement.fx_gain_loss")
		if !ok {
			t.Fatalf("gran %v: FX line missing", gran)
		}
		// Cells: [label, period_0 .. period_{n-1}, total]. Sum the period columns.
		if len(fxRow) < 3 {
			t.Fatalf("gran %v: FX row has %d cells, want label + >=1 period + total", gran, len(fxRow))
		}
		var periodSum int64
		var columns int
		for _, c := range fxRow[1 : len(fxRow)-1] {
			if c.Blank {
				continue
			}
			periodSum += c.Minor
			columns++
		}
		total := fxRow[len(fxRow)-1].Minor
		if periodSum != total {
			t.Errorf("gran %v: FX period columns sum %d != Total column %d (must foot)", gran, periodSum, total)
		}
		if total != wholeFX {
			t.Errorf("gran %v: FX Total %d != whole-range figure %d (telescoping)", gran, total, wholeFX)
		}
		if gran == reports.GranMonth && columns < 2 {
			t.Errorf("gran %v: FX recognized in only %d columns; the fixture should spread it across >=2 (else this test is vacuous)", gran, columns)
		}
	}
}

// isFuncCols returns, for the FUNCTIONAL (GranNone) layout, the row's Admin, Fundraising,
// Program, Total cells (columns 1..4) and whether the row has that shape.
func isFuncCols(row reports.Row) (admin, fr, prog, tot reports.Cell, ok bool) {
	if len(row.Cells) != 5 {
		return reports.Cell{}, reports.Cell{}, reports.Cell{}, reports.Cell{}, false
	}
	return row.Cells[1], row.Cells[2], row.Cells[3], row.Cells[4], true
}

// TestIncomeStatementGranNone exercises the DEFAULT (no-granularity) path. At TOTAL
// granularity the single "Period" column is DROPPED and replaced by three FUNCTIONAL
// columns -- Admin (management), Fundraising, Program (the residual Total − Admin −
// Fundraising) -- plus the Total column (5 columns: Line, Admin, Fundraising, Program,
// Total), mirroring the functional-expenses statement. Per data / section-total row the
// three functional columns FOOT to Total exactly; revenue rows (classless, D24) carry
// BLANK Admin/Fundraising and Program == Total.
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

	// Line + Admin + Fundraising + Program + Total = 5 columns, no Period column.
	if len(table.Columns) != 5 {
		t.Fatalf("GranNone columns = %d, want 5 (Line, Admin, Fundraising, Program, Total)", len(table.Columns))
	}
	wantHeaders := []string{
		"reports.income_statement.col.line",
		"reports.income_statement.col.admin",
		"reports.income_statement.col.fundraising",
		"reports.income_statement.col.program",
		"reports.income_statement.col.total",
	}
	for i, want := range wantHeaders {
		if table.Columns[i].HeaderKey != want {
			t.Errorf("GranNone column %d header = %q, want %q", i, table.Columns[i].HeaderKey, want)
		}
	}
	for _, c := range table.Columns {
		if c.HeaderKey == "reports.income_statement.col.period" {
			t.Errorf("GranNone still carries a Period column; it must be dropped")
		}
	}

	// Per DATA / SECTION-TOTAL row: Admin + Fundraising + Program == Total (exact, since
	// Program is the residual). Skip the FX / net reconciling rows (no functional class).
	reconciling := map[string]bool{
		"reports.income_statement.fx_gain_loss": true,
		"reports.income_statement.net":          true,
	}
	for _, row := range table.Rows {
		admin, fr, prog, tot, ok := isFuncCols(row)
		if !ok || tot.Kind != reports.CellMoney || tot.Blank {
			continue // heading row (blank Total)
		}
		if reconciling[row.Cells[0].Text] {
			continue
		}
		// Blank functional cells count as zero for the footing check.
		sum := prog.Minor
		if !admin.Blank {
			sum += admin.Minor
		}
		if !fr.Blank {
			sum += fr.Minor
		}
		if sum != tot.Minor {
			t.Errorf("row %q: Admin+Fundraising+Program (%d) != Total (%d)", row.Cells[0].Text, sum, tot.Minor)
		}
	}

	// A revenue row (Contributions): classless, so Admin/Fundraising BLANK, Program == Total.
	if cells, ok := isRowFor(table, "Contributions"); ok && len(cells) == 5 {
		if !cells[1].Blank || !cells[2].Blank {
			t.Errorf("revenue row Contributions: Admin/Fundraising should be blank, got %+v / %+v", cells[1], cells[2])
		}
		if cells[3].Minor != cells[4].Minor {
			t.Errorf("revenue row Contributions: Program (%d) != Total (%d)", cells[3].Minor, cells[4].Minor)
		}
	} else {
		t.Errorf("no 5-column Contributions row")
	}

	// A pure-management expense (Occupancy): Admin == Total, Fundraising 0, Program 0 EXACTLY
	// (no residual rounding noise on a single-class account).
	if cells, ok := isRowFor(table, "Occupancy"); ok && len(cells) == 5 {
		if cells[3].Blank || cells[3].Minor != 0 {
			t.Errorf("pure-management Occupancy: Program should be 0 exactly, got %+v", cells[3])
		}
		if cells[1].Minor != cells[4].Minor {
			t.Errorf("pure-management Occupancy: Admin (%d) != Total (%d)", cells[1].Minor, cells[4].Minor)
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

// TestIncomeStatementFunctionalTiesFunctionalExpenses cross-checks the new functional
// columns against an INDEPENDENT report: the Statement of Activities' Admin, Fundraising
// AND (residual) Program expense-section totals must equal the functional-expenses (990
// Part IX) grand-total's management, fundraising and program columns EXACTLY -- both derive
// from FunctionalMatrix at the transaction-date rate over the same scope/period/target.
//
// It runs BOTH without and WITH the Lempira FX exposure (ExtendFX). The FX case is the
// load-bearing one: because Program is the residual (Total − Admin − Fundraising), the
// horizontal foot holds unconditionally even if FunctionalMatrix silently under-counted
// Admin/Fundraising in the FX currency -- only this direct fe<->is tie proves the
// FX-currency expense is genuinely class-tagged and NOT quietly dumped into the residual.
func TestIncomeStatementFunctionalTiesFunctionalExpenses(t *testing.T) {
	for _, tc := range []struct {
		name string
		fx   bool
	}{
		{"no FX", false},
		{"with FX exposure", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture.New(t)
			f.ExtendRates(t)
			if tc.fx {
				f.ExtendFX(t)
			}
			ctx := context.Background()

			feP := feGoldenParams(f)
			feT, err := functionalExpensesReport(t).Run(ctx, reports.NewToolkit(f.Store, feP), feP)
			if err != nil {
				t.Fatalf("run functional expenses: %v", err)
			}
			feGrand, ok := feRowFor(feT, "reports.functional_expenses.total")
			if !ok {
				t.Fatal("no functional-expenses grand-total row")
			}
			// FE grand cols: Line, Program, Management, Fundraising, Total.
			feProg, feMgmt, feFund := feGrand[1].Minor, feGrand[2].Minor, feGrand[3].Minor

			isP := isGoldenParams(f)
			isP.Granularity = reports.GranNone
			isT, err := incomeStatementReport(t).Run(ctx, reports.NewToolkit(f.Store, isP), isP)
			if err != nil {
				t.Fatalf("run income statement: %v", err)
			}
			var isAdmin, isFund, isProg int64
			found := false
			for _, row := range isT.Rows {
				if row.Cells[0].Text == "reports.income_statement.total.expenses" && len(row.Cells) == 5 {
					isAdmin, isFund, isProg = row.Cells[1].Minor, row.Cells[2].Minor, row.Cells[3].Minor
					found = true
				}
			}
			if !found {
				t.Fatal("no 5-column total-expenses row in the income statement")
			}
			if isAdmin != feMgmt {
				t.Errorf("income-statement Admin (%d) != functional-expenses management (%d)", isAdmin, feMgmt)
			}
			if isFund != feFund {
				t.Errorf("income-statement Fundraising (%d) != functional-expenses fundraising (%d)", isFund, feFund)
			}
			if isProg != feProg {
				t.Errorf("income-statement Program (%d) != functional-expenses program (%d) -- residual off by rounding grain / dropped FX-currency class?", isProg, feProg)
			}
		})
	}
}

// TestIncomeStatementFundFunctionalColumns is the fund + total-granularity blind spot:
// with a fund selected the new functional columns must be FUND-AWARE (org-wide Admin/
// Fundraising against a fund-scoped Total would make the residual Program garbage). Scoped
// to Beca Agua at GranNone: every data / section-total row foots (Admin+Fundraising+Program
// == Total) AND the leaf Admin/Fundraising sum to the section Admin/Fundraising totals.
func TestIncomeStatementFundFunctionalColumns(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := incomeStatementReport(t)

	p := isGoldenParams(f)
	p.Granularity = reports.GranNone
	p.Fund = reports.FundID(f.IDs.BecaAgua)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run fund-scoped GranNone: %v", err)
	}
	if len(table.Columns) != 5 {
		t.Fatalf("fund GranNone columns = %d, want 5", len(table.Columns))
	}

	reconciling := map[string]bool{
		"reports.income_statement.fx_gain_loss": true,
		"reports.income_statement.net":          true,
	}
	// Horizontal footing on data + section-total rows.
	for _, row := range table.Rows {
		admin, fr, prog, tot, ok := isFuncCols(row)
		if !ok || tot.Kind != reports.CellMoney || tot.Blank || reconciling[row.Cells[0].Text] {
			continue
		}
		sum := prog.Minor
		if !admin.Blank {
			sum += admin.Minor
		}
		if !fr.Blank {
			sum += fr.Minor
		}
		if sum != tot.Minor {
			t.Errorf("fund row %q: Admin+Fundraising+Program (%d) != Total (%d)", row.Cells[0].Text, sum, tot.Minor)
		}
	}

	// Vertical footing: Σ expense-leaf Admin == the expense section's Admin total (proves
	// the fund-aware functional split feeds the same fund-scoped rows as the Total backbone).
	var leafAdmin, leafFund int64
	inExpense := false
	for _, row := range table.Rows {
		name := row.Cells[0].Text
		switch name {
		case "reports.income_statement.section.expenses":
			inExpense = true
			continue
		case "reports.income_statement.total.expenses":
			admin, fr, _, _, _ := isFuncCols(row)
			if !admin.Blank && admin.Minor != leafAdmin {
				t.Errorf("expense-section Admin total (%d) != Σ leaf Admin (%d)", admin.Minor, leafAdmin)
			}
			if !fr.Blank && fr.Minor != leafFund {
				t.Errorf("expense-section Fundraising total (%d) != Σ leaf Fundraising (%d)", fr.Minor, leafFund)
			}
			inExpense = false
			continue
		}
		if !inExpense || row.Kind != reports.RowData {
			continue
		}
		if admin, fr, _, _, ok := isFuncCols(row); ok {
			if !admin.Blank {
				leafAdmin += admin.Minor
			}
			if !fr.Blank {
				leafFund += fr.Minor
			}
		}
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

// TestIncomeStatementFundFilter narrows the Statement of Activities to ONE fund (Beca
// Agua, p15.5 fund selector) and hand-verifies the fund's R/E flows via the native
// (rate-independent) data layer, plus that unrelated accounts drop from the render.
//
// HAND-VERIFIED (Beca Agua, native, full span — the fixture oracle):
//
//	Revenue  GovGrants  USD 200,000  (2,000.00) + MXN 10,000,000 (100,000.00)  [credits]
//	Expense             USD 150,000  (1,500.00) + MXN    300,000 (3,000.00)    [debits]
//
// So the fund's native R/E net-debit sums are USD -50,000 (200,000 credit - 150,000
// debit => surplus, net-debit negative) and MXN -9,700,000; presented as a positive
// surplus USD 50,000 / MXN 9,700,000.
func TestIncomeStatementFundFilter(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()

	// Fund in Params => periodActivityRows reads the fund-filtered store query.
	p := isGoldenParams(f)
	p.Granularity = reports.GranNone
	p.Fund = reports.FundID(f.IDs.BecaAgua)
	tk := reports.NewToolkit(f.Store, p)

	nat, err := tk.Activity(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)},
		f.Expected.ActivityFrom, f.Expected.ActivityTo, reports.ConvertOpts{Mode: reports.RateNone})
	if err != nil {
		t.Fatalf("fund-filtered activity native: %v", err)
	}
	// Sum the R/E accounts' native activity: it must equal Beca Agua's flows ONLY.
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
	if usd != -50_000 {
		t.Errorf("Beca Agua native USD R/E net = %d, want -50000 (200,000 credit - 150,000 debit)", usd)
	}
	if mxn != -9_700_000 {
		t.Errorf("Beca Agua native MXN R/E net = %d, want -9700000 (10,000,000 credit - 300,000 debit)", mxn)
	}
	// Only Beca Agua's revenue account (GovernmentGrants) carries revenue; Contributions
	// (Building Fund) and EventIncome (unrestricted) do NOT belong to Beca Agua.
	if _, ok := nat[reports.AccountID(f.IDs.GovernmentGrants)]; !ok {
		t.Errorf("GovernmentGrants (Beca Agua revenue) missing from fund-filtered activity")
	}
	if _, ok := nat[reports.AccountID(f.IDs.Contributions)]; ok {
		t.Errorf("Contributions (Building Fund) leaked into the Beca Agua fund-filtered activity")
	}

	// The rendered report reflects the filter: Contributions and Event Income drop out.
	rep := incomeStatementReport(t)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run fund-filtered income statement: %v", err)
	}
	if _, ok := isRowFor(table, "Contributions"); ok {
		t.Errorf("Contributions (not a Beca Agua flow) rendered in the fund-filtered statement")
	}
	if _, ok := isRowFor(table, "Government Grants"); !ok {
		t.Errorf("Government Grants (Beca Agua revenue) missing from the fund-filtered statement")
	}
}

// TestIncomeStatementProgramFilter narrows the Statement of Activities to ONE program
// (Educacion, a leaf program, p15.5 program selector) and hand-verifies the program's
// R/E flows via the native data layer, reusing the p27.4 ProgramScope machinery.
//
// HAND-VERIFIED (Educacion, native, full span — from Expected.ProgramActivity):
//
//	Revenue  GovGrants  USD 200,000 + MXN 10,000,000 ; ProgramFees USD 120,000  [credits]
//	Expense  ProgramSupplies  USD 150,000 + MXN 500,000                          [debits]
//
// So native R/E net-debit: USD = -(200,000+120,000) + 150,000 = -170,000; MXN =
// -10,000,000 + 500,000 = -9,500,000; presented positive as surplus 170,000 / 9,500,000.
func TestIncomeStatementProgramFilter(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()

	// A user program pick resolves to its subtree; Educacion is a leaf, so the subtree is
	// {Educacion}. periodActivityRows reads ProgramScope directly, so set it here (the
	// report's Run resolves p.Program -> this same scope).
	p := isGoldenParams(f)
	p.Granularity = reports.GranNone
	p.ProgramScope = []reports.ProgramID{reports.ProgramID(f.IDs.Educacion)}
	tk := reports.NewToolkit(f.Store, p)

	nat, err := tk.Activity(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)},
		f.Expected.ActivityFrom, f.Expected.ActivityTo, reports.ConvertOpts{Mode: reports.RateNone})
	if err != nil {
		t.Fatalf("program-filtered activity native: %v", err)
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
	if usd != -170_000 {
		t.Errorf("Educacion native USD R/E net = %d, want -170000", usd)
	}
	if mxn != -9_500_000 {
		t.Errorf("Educacion native MXN R/E net = %d, want -9500000", mxn)
	}
	// FoodPurchases is a Food Pantry / General flow, NOT Educacion => absent.
	if _, ok := nat[reports.AccountID(f.IDs.FoodPurchases)]; ok {
		t.Errorf("FoodPurchases (not Educacion) leaked into the Educacion program-filtered activity")
	}

	// The report resolves the same scope from p.Program and renders it: verify via the
	// user-selection path (p.Program set, ProgramScope left for the report to resolve).
	rep := incomeStatementReport(t)
	pp := isGoldenParams(f)
	pp.Granularity = reports.GranNone
	pp.Program = reports.ProgramID(f.IDs.Educacion)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, pp), pp)
	if err != nil {
		t.Fatalf("run program-filtered income statement: %v", err)
	}
	if _, ok := isRowFor(table, "Food Purchases"); ok {
		t.Errorf("Food Purchases (not Educacion) rendered in the program-filtered statement")
	}
	if _, ok := isRowFor(table, "Program Supplies"); !ok {
		t.Errorf("Program Supplies (Educacion expense) missing from the program-filtered statement")
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
