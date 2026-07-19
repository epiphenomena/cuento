package reports_test

// p26.51 capital-campaign report tests. The data is the ExtendCapitalCampaign seam
// (a restricted "Restore the Way" fund with multi-quarter USD + MXN revenue, a Land
// purchase, and Construction (fixed-asset) purchases); every asserted figure is
// HAND-DERIVED in the seam (fixture.CampaignExpected) or computed here from it.
//
// The report is run CONVERTED to USD (its intended mode): each figure converts at the
// relevant quarter-end / report-date closing rate (D12), so the matrix is a single
// clean USD series. The RNA identity (RNA = GrossRev - GrossExp - Capitalized) is
// asserted in NATIVE per currency against the seam's hand values (it is exact only
// native; converting introduces the accepted FX residual). Land ties exactly to its
// native figure because Land is USD-only (USD->USD is identity).

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/testutil/fixture"
)

// capitalCampaignReport fetches the registered report from Default().
func capitalCampaignReport(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.CapitalCampaignReportID)
	if !ok {
		t.Fatalf("capital-campaign report %q not registered in Default()", reports.CapitalCampaignReportID)
	}
	return rep
}

// campaignParams runs the report over the campaign span, root scope, converted to USD.
func campaignParams(f *fixture.Fixture) reports.Params {
	c := f.Expected.Campaign
	return reports.Params{
		Scope:          reports.SubsidiaryID(f.IDs.Root),
		Fund:           reports.FundID(c.Fund),
		From:           c.From, // 2025-01-01
		To:             c.To,   // 2025-12-31
		TargetCurrency: "USD",
		Lang:           "en",
	}
}

// TestCapitalCampaignRNAIdentityNative pins campus.py's core identity on the seam's
// NATIVE hand figures, per currency: RNA = GrossRevenue - GrossExpense - (Land +
// Construction). This is the report's correctness contract; the golden below shows the
// converted presentation of the same data.
func TestCapitalCampaignRNAIdentityNative(t *testing.T) {
	f := fixture.New(t)
	f.ExtendCapitalCampaign(t)
	c := f.Expected.Campaign

	// USD: 22,000 (20,000 gift + 2,000 loan proceeds) - 1,500 - (8,000 + 7,000) = 5,500.
	if got := c.GrossRevenueUSD - c.GrossExpenseUSD - c.LandUSD - c.ConstructionUSD; got != c.RNAUSD {
		t.Errorf("USD RNA identity: %d, want %d", got, c.RNAUSD)
	}
	// MXN: 100,000 - 0 - (0 + 60,000) = 40,000.
	if got := c.GrossRevenueMXN - c.GrossExpenseMXN - c.LandMXN - c.ConstructionMXN; got != c.RNAMXN {
		t.Errorf("MXN RNA identity: %d, want %d", got, c.RNAMXN)
	}
}

// TestCapitalCampaignFundStatementBridge proves the report's engine agrees with the
// already-tested p15.8 FundStatement: the campaign fund's spendable Closing over the
// whole span EQUALS the seam's native RNA per currency (both are Rev - Exp -
// Capitalized with opening 0), tying the new report to the shipped one.
func TestCapitalCampaignFundStatementBridge(t *testing.T) {
	f := fixture.New(t)
	f.ExtendCapitalCampaign(t)
	c := f.Expected.Campaign
	ctx := context.Background()

	st, err := reports.NewToolkit(f.Store, reports.Params{}).
		FundPeriodStatement(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)}, reports.FundID(c.Fund), c.From, c.To)
	if err != nil {
		t.Fatalf("fund statement: %v", err)
	}
	if st.Received["USD"] != c.GrossRevenueUSD {
		t.Errorf("USD received = %d, want %d", st.Received["USD"], c.GrossRevenueUSD)
	}
	if st.Received["MXN"] != c.GrossRevenueMXN {
		t.Errorf("MXN received = %d, want %d", st.Received["MXN"], c.GrossRevenueMXN)
	}
	if st.AppliedExpense["USD"] != c.GrossExpenseUSD {
		t.Errorf("USD applied expense = %d, want %d", st.AppliedExpense["USD"], c.GrossExpenseUSD)
	}
	// Capitalized (non-expense applications) = Land + Construction per currency.
	if st.Capitalized["USD"] != c.LandUSD+c.ConstructionUSD {
		t.Errorf("USD capitalized = %d, want %d", st.Capitalized["USD"], c.LandUSD+c.ConstructionUSD)
	}
	if st.Capitalized["MXN"] != c.LandMXN+c.ConstructionMXN {
		t.Errorf("MXN capitalized = %d, want %d", st.Capitalized["MXN"], c.LandMXN+c.ConstructionMXN)
	}
	// Spendable Closing == native RNA per currency.
	if st.Closing["USD"] != c.RNAUSD {
		t.Errorf("USD closing = %d, want RNA %d", st.Closing["USD"], c.RNAUSD)
	}
	if st.Closing["MXN"] != c.RNAMXN {
		t.Errorf("MXN closing = %d, want RNA %d", st.Closing["MXN"], c.RNAMXN)
	}
}

// TestCapitalCampaignLandTies proves the capital-detail section carries a "Land" row
// whose as-of-report-date converted figure equals the native Land amount (Land is
// USD-only, so USD->USD is identity) -- the report analogue of the real-data Land tie
// to campus.py's $166,482.00. The report never branches on the account name.
func TestCapitalCampaignLandTies(t *testing.T) {
	f := fixture.New(t)
	f.ExtendCapitalCampaign(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := capitalCampaignReport(t)

	p := campaignParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// The Land detail row: first cell the account NAME, the Capitalized column (index 4)
	// its as-of figure. Land is USD -> converted USD == native.
	got, ok := capitalDetailAmount(table, "Land")
	if !ok {
		t.Fatalf("capital-detail section missing a Land row")
	}
	if got != f.Expected.Campaign.LandUSD {
		t.Errorf("Land as-of = %d, want native %d (USD->USD identity)", got, f.Expected.Campaign.LandUSD)
	}
	// Construction is also present (the fixed-asset rollup member).
	if _, ok := capitalDetailAmount(table, "Construction in Progress"); !ok {
		t.Errorf("capital-detail section missing a Construction row")
	}
}

// TestCapitalCampaignQuarterlySeries proves the report emits a QUARTERLY series (one
// data row per quarter of the span) with the per-quarter flows and as-of balances, and
// that the cumulative-total row carries the whole-span cumulative revenue converted at
// the report date. It also confirms Net Cash = Gross Revenue - Gross Expenses per row.
func TestCapitalCampaignQuarterlySeries(t *testing.T) {
	f := fixture.New(t)
	f.ExtendCapitalCampaign(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := capitalCampaignReport(t)

	p := campaignParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// The span 2025-01-01..2025-12-31 is FOUR calendar quarters.
	dataRows := 0
	for _, row := range table.Rows {
		if row.Kind == reports.RowData && row.Cells[0].Kind == reports.CellDate {
			dataRows++
			// Net Cash (col 3) == Gross Revenue (col 1) - Gross Expenses (col 2).
			rev, exp, net := row.Cells[1].Minor, row.Cells[2].Minor, row.Cells[3].Minor
			if net != rev-exp {
				t.Errorf("quarter %s: net cash %d != rev %d - exp %d", row.Cells[0].Text, net, rev, exp)
			}
		}
	}
	if dataRows != 4 {
		t.Errorf("quarterly data rows = %d, want 4 (Q1..Q4 2025)", dataRows)
	}

	// The cumulative total row is present and emphasized.
	if !hasTotalRow(table) {
		t.Errorf("missing cumulative total row")
	}
}

// TestCapitalCampaignMatrixCells pins the report's EMITTED matrix cells to hand numbers
// (not the golden), covering the task's "assert the quarterly matrix, the RNA formula,
// and per-currency conversion":
//
//   - Q1 (2025-03-31) is USD-ONLY (the gift + the Land buy), so USD->USD is identity and
//     the cells are RATE-FREE: Capitalized == Land 8,000.00 (800,000); RNA == Gross rev
//     2,000,000 - Land 800,000 = 1,200,000 (the RNA formula on the report's own cells).
//   - Q2 (2025-06-30) Gross revenue is the MXN 100,000.00 grant CONVERTED to USD at the
//     2025-06-01 on-or-before rate (USD->MXN 17.323529412 => MXN->USD reciprocal):
//     10,000,000 minor MXN / 17.323529412 = 577,250 minor USD ($5,772.50), half-even.
//     This is the per-currency-conversion tie the task names.
func TestCapitalCampaignMatrixCells(t *testing.T) {
	f := fixture.New(t)
	f.ExtendCapitalCampaign(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := capitalCampaignReport(t)

	p := campaignParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Q1 rate-free ties (Gross rev col 1, Capitalized col 4, RNA col 5). Q1 holds only
	// the gift and the Land buy, so Gross revenue is the gift (2,000,000) and Capitalized
	// is Land (800,000); RNA == the row's OWN Gross rev - Capitalized (the RNA formula on
	// the report's own cells, rate-free). The loan-financed construction is in Q3, so Q1's
	// cells are the pre-loan figures.
	q1 := quarterRowCells(t, table, "2025-03-31")
	if got := q1[1].Minor; got != int64(2_000_000) {
		t.Errorf("Q1 gross revenue = %d, want the gift 2,000,000 (rate-free)", got)
	}
	if got := q1[4].Minor; got != f.Expected.Campaign.LandUSD {
		t.Errorf("Q1 capitalized = %d, want Land %d (rate-free)", got, f.Expected.Campaign.LandUSD)
	}
	if got, want := q1[5].Minor, q1[1].Minor-q1[4].Minor; got != want {
		t.Errorf("Q1 RNA = %d, want gross rev - capitalized = %d (RNA formula, rate-free)", got, want)
	}

	// Q2 per-currency conversion tie (Gross revenue col 1): MXN 100,000.00 -> USD at the
	// 2025-06 rate. Hand: 10,000,000 / 17.323529412 = 577,250 minor (half-even).
	q2 := quarterRowCells(t, table, "2025-06-30")
	if got, want := q2[1].Minor, int64(577_250); got != want {
		t.Errorf("Q2 gross revenue (converted) = %d, want %d (MXN 100,000 -> USD @ 2025-06 rate)", got, want)
	}
}

// TestCapitalCampaignColumnReconciles is the p26.68 correctness contract on the report's
// EMITTED table: (a) the cumulative Capitalized COLUMN equals the sum of the capital-detail
// ROWS shown below it, and (b) the cumulative Restricted Net Assets equals Gross Revenue -
// Gross Expenses - Capitalized (its documented formula). The old report violated BOTH: it
// netted liability splits into the Capitalized column (which the asset-only detail could
// not reconcile) and accumulated RNA as an independent spendable balance. Routing the
// report through FundPeriodStatement makes both hold by construction. Asserted on the
// converted (USD) run so the residual is the accepted 1-cent FX rounding (the task's
// exact-formula contract is proven NATIVE in TestCapitalCampaignRNAIdentityNative).
func TestCapitalCampaignColumnReconciles(t *testing.T) {
	f := fixture.New(t)
	f.ExtendCapitalCampaign(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := capitalCampaignReport(t)

	p := campaignParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	total := totalRowCells(t, table)
	rev, exp, capCol, rna := total[1].Minor, total[2].Minor, total[4].Minor, total[5].Minor

	// (a) Capitalized column == sum of the capital-detail rows (Land + Construction).
	var detailSum int64
	for _, row := range table.Rows {
		if row.Kind == reports.RowData && len(row.Cells) >= 6 && row.Cells[0].Kind == reports.CellText {
			detailSum += row.Cells[4].Minor
		}
	}
	if detailSum != capCol {
		t.Errorf("Capitalized column %d != sum of detail rows %d", capCol, detailSum)
	}

	// (b) RNA == Rev - Exp - Capitalized (within the accepted 1-cent FX rounding residual
	// on the converted run; it is EXACT natively, see TestCapitalCampaignRNAIdentityNative).
	want := rev - exp - capCol
	if diff := rna - want; diff < -1 || diff > 1 {
		t.Errorf("RNA %d != Rev %d - Exp %d - Capitalized %d (= %d); diff %d", rna, rev, exp, capCol, want, diff)
	}
}

// TestCapitalCampaignEmptyFund proves the framework nothing-to-show rule: with no fund
// chosen the report returns an empty Table (so a bare hit renders 200).
func TestCapitalCampaignEmptyFund(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := capitalCampaignReport(t)
	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), To: "2025-12-31", Lang: "en"} // Fund 0
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(table.Rows) != 0 {
		t.Errorf("empty-fund run has %d rows, want 0", len(table.Rows))
	}
}

// TestCapitalCampaignGolden compares the converted (USD) rendered text + CSV to the
// committed goldens (regenerated with -update, reviewed).
func TestCapitalCampaignGolden(t *testing.T) {
	f := fixture.New(t)
	f.ExtendCapitalCampaign(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := capitalCampaignReport(t)

	p := campaignParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "capital_campaign.txt", []byte(textDump))
	checkGolden(t, "capital_campaign.csv", csvBuf.Bytes())
}

// --- helpers ----------------------------------------------------------------

// capitalDetailAmount returns the Capitalized-column (index 4) amount of the capital
// DETAIL row whose account name (col 0, a TextCell) is name, and whether it was found.
func capitalDetailAmount(t reports.Table, name string) (int64, bool) {
	for _, row := range t.Rows {
		if row.Kind != reports.RowData || len(row.Cells) < 6 {
			continue
		}
		if row.Cells[0].Kind == reports.CellText && row.Cells[0].Text == name {
			return row.Cells[4].Minor, true
		}
	}
	return 0, false
}

// quarterRowCells returns the cells of the quarterly DATA row whose first cell is the
// quarter-end DATE end, failing the test if absent.
func quarterRowCells(t *testing.T, tbl reports.Table, end string) []reports.Cell {
	t.Helper()
	for _, row := range tbl.Rows {
		if row.Kind == reports.RowData && len(row.Cells) >= 6 &&
			row.Cells[0].Kind == reports.CellDate && row.Cells[0].Text == end {
			return row.Cells
		}
	}
	t.Fatalf("no quarter row for %s", end)
	return nil
}

// hasTotalRow reports whether the table carries an emphasized RowTotal.
func hasTotalRow(t reports.Table) bool {
	for _, row := range t.Rows {
		if row.Kind == reports.RowTotal {
			return true
		}
	}
	return false
}

// totalRowCells returns the cells of the cumulative RowTotal row, failing if absent.
func totalRowCells(t *testing.T, tbl reports.Table) []reports.Cell {
	t.Helper()
	for _, row := range tbl.Rows {
		if row.Kind == reports.RowTotal {
			return row.Cells
		}
	}
	t.Fatalf("no cumulative total row")
	return nil
}
