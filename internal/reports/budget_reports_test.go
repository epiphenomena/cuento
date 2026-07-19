package reports_test

// p27.3 budget REPORT tests (cash-flow projection + budget variance), over the NEW
// split-derived model (budget_plans / budget_splits) -- replacing the retired
// schedule-based p19.4 tests. They run BOTH reports over the canonical, opt-in
// fixture.ExtendSampleBudgetPlan seam (a US-scoped plan with projected R/E + open_item
// splits) and pin the redesign's load-bearing behaviors:
//
//   - cash-flow projection's PER-FUND opening comes from ACTUAL current-cash balances
//     (non-zero for the unrestricted group, which holds the base fixture's Checking US
//     cash), then rolls forward through projected inflows/outflows;
//   - budget variance compares projected R/E splits (signed net-debit) against actual
//     activity, the ACTUAL cell DRILLS (restricted cell reconciles), and an
//     unrestricted (fund 0) actual cell is NOT drillable (fund-IS-NULL inexpressible).
//
// Numbers are SYNTHETIC (rule 11); the goldens are the oracle (reviewed via
// `make golden`), with structural cell assertions to catch a silently-empty table.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/store"
	"cuento/internal/testutil/fixture"
)

// bvFirstMonth is the first MONTH column index in the p30.9 budget-variance pivot:
// columns are Account(0) | Fund(1) | Program(2) | Currency(3) | <month...> | Total.
const bvFirstMonth = 4

// TestVarianceBucket pins the p30.9 over/under-budget MAGNITUDE thresholds (rule 4): the
// pure |variance|/|budgeted| classifier the web layer color-codes. The boundaries are the
// crux of the color feature, so they are pinned here (a ratio tweak must break a test).
func TestVarianceBucket(t *testing.T) {
	cases := []struct {
		name             string
		budgeted, varnce int64
		want             string
	}{
		{"zero variance is neutral", 1000, 0, reports.VarianceNeutral},
		{"9.9% is slight (below 10%)", 1000, 99, reports.VarianceSlight},
		{"exactly 10% is moderate", 1000, 100, reports.VarianceModerate},
		{"24% is moderate (below 25%)", 1000, 240, reports.VarianceModerate},
		{"exactly 25% is large", 1000, 250, reports.VarianceLarge},
		{"50% is large", 1000, 500, reports.VarianceLarge},
		{"negative variance uses |ratio| (under budget)", 1000, -300, reports.VarianceLarge},
		{"zero budget, nonzero variance is large (unbudgeted)", 0, 1500, reports.VarianceLarge},
		{"both zero is neutral", 0, 0, reports.VarianceNeutral},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reports.VarianceBucket(c.budgeted, c.varnce); got != c.want {
				t.Errorf("VarianceBucket(%d, %d) = %q, want %q", c.budgeted, c.varnce, got, c.want)
			}
		})
	}
}

// budgetReport fetches a registered budget report from the default registry (proving
// it IS registered under its id in the "budget" group).
func budgetReport(t *testing.T, id string) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(id)
	if !ok {
		t.Fatalf("budget report %q not registered in Default()", id)
	}
	if rep.Group != "budget" {
		t.Fatalf("report %q group = %q, want %q", id, rep.Group, "budget")
	}
	return rep
}

// TestBudgetReportsRegistered asserts both p27.3 reports are registered under the
// "budget" group (so the index/matrix/report_groups sync pick them up).
func TestBudgetReportsRegistered(t *testing.T) {
	budgetReport(t, reports.CashflowProjectionReportID)
	budgetReport(t, reports.BudgetVarianceReportID)
}

// countDataRows counts the RowData rows in a table (structural sanity -- a nonzero
// count proves the plan actually produced report rows).
func countDataRows(t reports.Table) int {
	n := 0
	for _, r := range t.Rows {
		if r.Kind == reports.RowData {
			n++
		}
	}
	return n
}

// TestCashflowProjectionGolden runs cash-flow projection over the sample plan at
// monthly granularity, root scope (so the US plan's currency + the actual current-cash
// balances are visible). It asserts the unrestricted (fund 0) USD row has a NON-ZERO
// opening (the resolved PER-FUND opening from actuals -- Checking US holds cash) and a
// DIFFERENT ending (projected flows rolled forward), then compares goldens.
func TestCashflowProjectionGolden(t *testing.T) {
	f := fixture.New(t)
	f.ExtendSampleBudgetPlan(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	sp := f.Expected.SampleBudgetPlan
	rep := budgetReport(t, reports.CashflowProjectionReportID)
	p := reports.Params{
		Scope: reports.SubsidiaryID(f.IDs.Root), Budget: sp.Plan, From: sp.From, To: sp.To,
		Granularity: reports.GranMonth, Lang: "en",
	}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run cashflow projection: %v", err)
	}

	if dataRows := countDataRows(table); dataRows == 0 {
		t.Fatalf("cashflow projection produced no data rows")
	}

	// Unrestricted (fund 0) USD row: opening (col 2) is the ACTUAL current-cash balance
	// (non-zero -- Checking US cash), and the ending differs (projected flows applied).
	row, ok := cashflowRow(table, "reports.budget.unrestricted", "USD")
	if !ok {
		t.Fatalf("no unrestricted/USD cashflow row")
	}
	start := row.Cells[2].Minor
	end := row.Cells[len(row.Cells)-1].Minor
	if start == 0 {
		t.Errorf("unrestricted/USD opening = 0, want non-zero (per-fund opening from actuals)")
	}
	if start == end {
		t.Errorf("unrestricted/USD opening == ending (%d); projected flows should move it", start)
	}

	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "cashflow_projection_sample.txt", []byte(textDump))
	checkGolden(t, "cashflow_projection_sample.csv", csvBuf.Bytes())
}

// TestBudgetVarianceGolden runs budget variance over the sample plan at monthly
// granularity, root scope. It asserts a variance row exists with a real BUDGETED
// figure, that the ACTUAL cell of a restricted cell drills and reconciles, and that an
// unrestricted actual cell is NOT drillable; then compares goldens.
func TestBudgetVarianceGolden(t *testing.T) {
	f := fixture.New(t)
	f.ExtendSampleBudgetPlan(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	sp := f.Expected.SampleBudgetPlan
	rep := budgetReport(t, reports.BudgetVarianceReportID)
	p := reports.Params{
		Scope: reports.SubsidiaryID(f.IDs.Root), Budget: sp.Plan, From: sp.From, To: sp.To,
		Granularity: reports.GranMonth, Lang: "en",
	}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run budget variance: %v", err)
	}

	if dataRows := countDataRows(table); dataRows == 0 {
		t.Fatalf("budget variance produced no data rows")
	}

	// p30.9 STRUCTURE: columns are Account | Fund | Program | Currency | <month...> | Total.
	// The last column is the row Total (a CellMeasures), the columns before it (from index
	// bvFirstMonth=4) are the month cells (also CellMeasures). A row label must carry the
	// fully-qualified (dotted-path) ACCOUNT + the FUND + the PROGRAM (rule 1).
	if got, want := len(table.Columns), 5; got < want {
		t.Fatalf("budget variance has %d columns, want >= %d (account/fund/program/currency/months/total)", got, want)
	}
	totalCol := len(table.Columns) - 1

	// A qualified row label: at least one data row's account cell is a DOTTED path (the
	// fixture nests the R/E accounts under Revenue/Expenses), and its fund+program cells
	// are populated -- so the row unambiguously names which account, fund, and program.
	var sawQualified bool
	for _, r := range table.Rows {
		if r.Kind != reports.RowData {
			continue
		}
		acct := r.Cells[0].Text
		if strings.Contains(acct, ".") && r.Cells[1].Text != "" && r.Cells[2].Text != "" {
			sawQualified = true
			break
		}
	}
	if !sawQualified {
		t.Errorf("no data row carried a dotted-path account label with fund+program (rule 1)")
	}

	// Each grid cell carries all three measures (CellMeasures with budgeted/actual/variance),
	// and variance == actual − budgeted. At least one row must carry a non-zero BUDGETED
	// figure somewhere (the plan's projected splits reached the report). Only the ACTUAL
	// measure drills, and a restricted actual drill reconciles; an unrestricted one is nil.
	var sawBudgeted, sawMeasures bool
	for _, r := range table.Rows {
		if r.Kind != reports.RowData {
			continue
		}
		unrestricted := r.Cells[1].Text == "reports.budget.unrestricted"
		for col := bvFirstMonth; col <= totalCol; col++ {
			c := r.Cells[col]
			if c.Kind != reports.CellMeasures {
				t.Fatalf("row cell col %d is kind %d, want CellMeasures", col, c.Kind)
			}
			sawMeasures = true
			if c.Variance != c.Actual-c.Budgeted {
				t.Errorf("cell variance %d != actual %d - budgeted %d", c.Variance, c.Actual, c.Budgeted)
			}
			if c.Budgeted != 0 {
				sawBudgeted = true
			}
			// An unrestricted cell never drills (fund-IS-NULL inexpressible); the Total
			// column never drills (it spans months).
			if (unrestricted || col == totalCol) && c.Drill != nil {
				t.Errorf("col %d (unrestricted=%v total=%v) must NOT drill", col, unrestricted, col == totalCol)
			}
			// A restricted MONTH actual drill must reconcile to the cell's Actual.
			if !unrestricted && col != totalCol && c.Drill != nil {
				if sum := drillSum(t, f, c.Drill); sum != c.Actual {
					t.Errorf("drilled actual sum = %d, want %d (reconciliation)", sum, c.Actual)
				}
			}
		}
	}
	if !sawMeasures {
		t.Errorf("no CellMeasures cell found (the grid should fold three measures per cell)")
	}
	if !sawBudgeted {
		t.Errorf("no variance row carried a non-zero budgeted figure")
	}

	// A color-bucket class is set on an over/under TOTAL: at least one per-currency
	// grand-total row's Total cell carries a non-neutral magnitude bucket (rule 4), and
	// a zero-budget nonzero variance classes LARGE.
	var sawBucket bool
	for _, r := range table.Rows {
		if r.Kind != reports.RowTotal {
			continue
		}
		tc := r.Cells[totalCol]
		if tc.Kind == reports.CellMeasures && tc.Bucket != reports.VarianceNeutral {
			sawBucket = true
		}
	}
	if !sawBucket {
		t.Errorf("no over/under TOTAL carried a color-bucket class (rule 4)")
	}

	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "budget_variance_sample.txt", []byte(textDump))
	checkGolden(t, "budget_variance_sample.csv", csvBuf.Bytes())
}

// TestBudgetReportsNoBudget: with no budget chosen (Budget == 0) each report returns
// an empty Table (the framework's nothing-to-show rule) so a bare hit renders 200.
func TestBudgetReportsNoBudget(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	for _, id := range []string{reports.CashflowProjectionReportID, reports.BudgetVarianceReportID} {
		rep := budgetReport(t, id)
		p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), From: "2026-01-01", To: "2026-12-31", Lang: "en"} // Budget == 0
		table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
		if err != nil {
			t.Fatalf("%s no-budget run: %v", id, err)
		}
		if len(table.Rows) != 0 {
			t.Errorf("%s no-budget table has %d rows, want 0", id, len(table.Rows))
		}
		if len(table.Columns) == 0 {
			t.Errorf("%s no-budget table has no columns; the form still needs the header row", id)
		}
	}
}

// cashflowRow finds the DATA row for (fundLabel, currency) in the cashflow table.
func cashflowRow(t reports.Table, fundLabel, ccy string) (reports.Row, bool) {
	for _, r := range t.Rows {
		if r.Kind != reports.RowData || len(r.Cells) < 4 {
			continue
		}
		if r.Cells[0].Text == fundLabel && r.Cells[1].Text == ccy {
			return r, true
		}
	}
	return reports.Row{}, false
}

// TestBudgetVarianceGrantProgramScope (p27.4b): a program-scoped report grant filters the
// budget-variance report's rows -- BOTH the projected (budget-split) side and the ACTUAL
// side -- to the granted program SUBTREE, so a SIBLING subtree never surfaces, including in
// the rolled per-currency TOTAL row. The sample plan (ExtendSampleBudgetPlan) carries both
// General-program and Educacion-program splits; scoping to Educacion drops every General
// row.
func TestBudgetVarianceGrantProgramScope(t *testing.T) {
	f := fixture.New(t)
	f.ExtendSampleBudgetPlan(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	sp := f.Expected.SampleBudgetPlan
	rep := budgetReport(t, reports.BudgetVarianceReportID)
	base := reports.Params{
		Scope: reports.SubsidiaryID(f.IDs.Root), Budget: sp.Plan, From: sp.From, To: sp.To,
		Granularity: reports.GranMonth, Lang: "en",
	}
	baseT, err := rep.Run(ctx, reports.NewToolkit(f.Store, base), base)
	if err != nil {
		t.Fatalf("run unscoped: %v", err)
	}
	// Baseline: General-program rows ARE present (the sibling is there without a scope).
	if !bvHasProgramRow(baseT, "General") {
		t.Fatalf("unscoped budget variance missing a General-program row (sibling present)")
	}
	baseUSDTotal, ok := bvTotalFor(baseT, "USD")
	if !ok {
		t.Fatalf("unscoped: no USD total row")
	}

	// Scope to Educacion (leaf subtree = {Educacion}): every General-program row vanishes.
	p := base
	p.ProgramScope = []reports.ProgramID{reports.ProgramID(f.IDs.Educacion)}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run scoped: %v", err)
	}
	// No DATA row may carry the General program (the sibling); at least one Educacion row
	// must survive (both the projected and actual sides are filtered, so a row exists).
	for _, r := range table.Rows {
		if r.Kind == reports.RowData && len(r.Cells) >= 3 && r.Cells[2].Text == "General" {
			t.Errorf("scoped budget variance leaks a General-program row: %+v", r.Cells)
		}
	}
	if !bvHasProgramRow(table, "Educacion") {
		t.Errorf("scoped budget variance dropped Educacion (in-subtree) rows")
	}
	// The rolled USD TOTAL reflects only Educacion's budgeted+actual -- it must move off the
	// org-wide figure (a General leak would keep it identical). Both sides were filtered, so
	// the budgeted and actual totals both shrink.
	scopedUSDTotal, ok := bvTotalFor(table, "USD")
	if !ok {
		t.Fatalf("scoped: no USD total row")
	}
	if scopedUSDTotal.budgeted == baseUSDTotal.budgeted {
		t.Errorf("scoped USD budgeted total %d == org-wide %d; a General row leaked into the rollup",
			scopedUSDTotal.budgeted, baseUSDTotal.budgeted)
	}
}

// bvHasProgramRow reports whether any DATA row of a budget-variance table carries the given
// program name in the program column (p30.9: index 2).
func bvHasProgramRow(t reports.Table, program string) bool {
	for _, r := range t.Rows {
		if r.Kind == reports.RowData && len(r.Cells) >= 3 && r.Cells[2].Text == program {
			return true
		}
	}
	return false
}

// bvTotals holds a budget-variance TOTAL row's budgeted + actual figures (a currency).
type bvTotals struct{ budgeted, actual int64 }

// bvTotalFor returns the budgeted/actual figures of the per-currency grand-TOTAL row (the
// label row whose currency cell, index 3, equals ccy). Columns (p30.9): Account | Fund |
// Program | Currency | <month...> | Total; the Total cell (last) is a CellMeasures folding
// budgeted/actual/variance.
func bvTotalFor(t reports.Table, ccy string) (bvTotals, bool) {
	for _, r := range t.Rows {
		if r.Kind == reports.RowTotal && len(r.Cells) >= 5 && r.Cells[3].Text == ccy {
			tc := r.Cells[len(r.Cells)-1]
			return bvTotals{budgeted: tc.Budgeted, actual: tc.Actual}, true
		}
	}
	return bvTotals{}, false
}
