package reports_test

// p19.4 budget REPORT tests (actuals-vs-budget + cashflow projection). Every number
// is REUSED verbatim from the p19.2 toolkit's already-hand-verified fixtures
// (budget_test.go): the ActualsVsBudget 250k-budget / 300k-actual / +50k-variance
// BecaAgua case, the fund-0-isolated 200k unrestricted actual, and the
// CashflowProjection BecaAgua MXN 9,700,000 start -> 10,300,000 end. So these tests
// prove the REPORT renders those toolkit numbers faithfully, not that the toolkit is
// right (that is budget_test.go's job).
//
// Design points pinned here: NO PRO-RATA is visible in the golden (a monthly line's
// amount in ONE bucket row); Variance = Actual - Budgeted (the p19.2 sign);
// Cashflow Start == FundBalancesAsOf(from), End == Start + budgeted flows; the ACTUAL
// column drills and its drilled splits reconcile to the actual figure (restricted
// cell); the reports appear only for a budget-granted user (index test lives in web).

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/store"
	"cuento/internal/testutil/fixture"
)

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

// TestBudgetReportsRegistered asserts both p19.4 reports are registered under the
// new "budget" group (so the index/matrix/report_groups sync pick them up).
func TestBudgetReportsRegistered(t *testing.T) {
	budgetReport(t, reports.ActualsVsBudgetReportID)
	budgetReport(t, reports.CashflowProjectionReportID)
}

// buildActualsBudget rebuilds the p19.2 TestActualsVsBudget budget over the fixture:
// a BecaAgua/MXN ProgramSupplies expense line, onetime 2025-05-15, amount 250,000.
// Returns the budget id.
func buildActualsBudget(t *testing.T, ctx context.Context, f *fixture.Fixture) int64 {
	t.Helper()
	id, err := f.Store.CreateBudget(ctx, store.BudgetInput{
		Name: "FY2025", PeriodStart: "2025-01-01", PeriodEnd: "2025-12-31",
	})
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	d := "2025-05-15"
	sched, err := f.Store.CreateSchedule(ctx, store.ScheduleInput{Name: "once-may", Kind: "onetime", AnchorDate: &d})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	beca := f.IDs.BecaAgua
	if _, err := f.Store.CreateBudgetLine(ctx, id, store.BudgetLineInput{
		SubsidiaryID: f.IDs.MX, AccountID: f.IDs.ProgramSupplies, FundID: &beca,
		ProgramID: f.IDs.Educacion, Amount: 250_000, Currency: "MXN", ScheduleID: sched,
	}); err != nil {
		t.Fatalf("create budget line: %v", err)
	}
	return id
}

// avbRow finds the DATA row for (bucket, account, fund, program, currency) in the
// actuals-vs-budget table, returning budgeted/actual/variance cells (cols 5/6/7).
func avbRow(t reports.Table, bucket string, acctName, fundLabel, progName, ccy string) (reports.Row, bool) {
	for _, r := range t.Rows {
		if r.Kind != reports.RowData || len(r.Cells) < 8 {
			continue
		}
		if r.Cells[0].Text == bucket && r.Cells[1].Text == acctName &&
			r.Cells[3].Text == progName && r.Cells[4].Text == ccy {
			// fund column is either a TEXT proper noun or a LABEL (unrestricted).
			fc := r.Cells[2]
			if fc.Text == fundLabel {
				return r, true
			}
		}
	}
	return reports.Row{}, false
}

// TestActualsVsBudgetGolden runs the actuals-vs-budget report over the p19.2 budget at
// MONTHLY granularity, re-verifies the 250k/300k/+50k BecaAgua figures and the isolated
// 200k unrestricted actual straight off the emitted cells, asserts the drilled actual
// reconciles, and compares text + CSV goldens. The monthly line appears in ONE bucket
// row (no pro-rata, visible in the golden).
func TestActualsVsBudgetGolden(t *testing.T) {
	f := fixture.New(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	budgetID := buildActualsBudget(t, ctx, f)

	rep := budgetReport(t, reports.ActualsVsBudgetReportID)
	p := reports.Params{
		Scope: f.IDs.MX, Budget: budgetID, From: "2025-01-01", To: "2025-12-31",
		Granularity: reports.GranMonth, Lang: "en",
	}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run actuals-vs-budget: %v", err)
	}

	acct := mustAccountName(t, f, f.IDs.ProgramSupplies, "en")
	becaName := mustFundName(t, f, f.IDs.BecaAgua)
	prog := mustProgramName(t, f, f.IDs.Educacion)

	// BecaAgua key, 2025-05 bucket: budgeted 250,000 / actual 300,000 / variance +50,000.
	beca, ok := avbRow(table, "2025-05-01", acct, becaName, prog, "MXN")
	if !ok {
		t.Fatalf("no BecaAgua row in bucket 2025-05-01")
	}
	if beca.Cells[5].Minor != 250_000 || beca.Cells[6].Minor != 300_000 || beca.Cells[7].Minor != 50_000 {
		t.Errorf("BecaAgua 2025-05: budgeted=%d actual=%d variance=%d, want 250000/300000/50000",
			beca.Cells[5].Minor, beca.Cells[6].Minor, beca.Cells[7].Minor)
	}

	// Unrestricted key, 2025-05 bucket: budgeted 0 / actual 200,000 / variance +200,000
	// (the OTHER half of the mixed txn, fund-0 isolated).
	// The unrestricted fund cell is a LABEL, so its raw Text is the i18n KEY.
	unr, ok := avbRow(table, "2025-05-01", acct, "reports.budget.unrestricted", prog, "MXN")
	if !ok {
		t.Fatalf("no unrestricted row in bucket 2025-05-01")
	}
	if unr.Cells[5].Minor != 0 || unr.Cells[6].Minor != 200_000 || unr.Cells[7].Minor != 200_000 {
		t.Errorf("unrestricted 2025-05: budgeted=%d actual=%d variance=%d, want 0/200000/200000",
			unr.Cells[5].Minor, unr.Cells[6].Minor, unr.Cells[7].Minor)
	}

	// NO PRO-RATA: the budgeted 250,000 appears in EXACTLY ONE bucket row across the year.
	var budgetedRows int
	for _, r := range table.Rows {
		if r.Kind == reports.RowData && len(r.Cells) >= 8 &&
			r.Cells[2].Text == becaName && r.Cells[5].Minor == 250_000 {
			budgetedRows++
		}
	}
	if budgetedRows != 1 {
		t.Errorf("budgeted 250000 appears in %d bucket rows, want exactly 1 (no pro-rata)", budgetedRows)
	}

	// DRILL RECONCILIATION on the ACTUAL cell (restricted BecaAgua): the drilled splits'
	// signed sum equals the actual figure (300,000). The budgeted cell is NOT drillable.
	if beca.Cells[6].Drill == nil {
		t.Fatalf("BecaAgua actual cell is not drillable")
	}
	if beca.Cells[5].Drill != nil {
		t.Errorf("budgeted cell must NOT be drillable (it is a plan)")
	}
	if sum := drillSum(t, f, beca.Cells[6].Drill); sum != beca.Cells[6].Minor {
		t.Errorf("drilled actual sum = %d, want %d (reconciliation)", sum, beca.Cells[6].Minor)
	}

	// Unrestricted actual cell is NOT drillable (fund-IS-NULL inexpressible, DECISIONS p19.4).
	if unr.Cells[6].Drill != nil {
		t.Errorf("unrestricted actual cell must NOT be drillable")
	}

	// Golden artifacts.
	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "actuals_vs_budget.txt", []byte(textDump))
	checkGolden(t, "actuals_vs_budget.csv", csvBuf.Bytes())
}

// TestCashflowProjectionGolden runs the cashflow-projection report over the p19.2
// projection budget (BecaAgua MXN: +1,000,000 revenue 2026-08-15, -400,000 expense
// 2026-09-15), verifies Start == 9,700,000 (FundBalancesAsOf period start) and End ==
// 10,300,000 straight off the emitted cells, and compares goldens.
func TestCashflowProjectionGolden(t *testing.T) {
	f := fixture.New(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	id, err := f.Store.CreateBudget(ctx, store.BudgetInput{
		Name: "Projection", PeriodStart: "2026-07-01", PeriodEnd: "2026-12-31",
	})
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	rev := "2026-08-15"
	exp := "2026-09-15"
	revSched, err := f.Store.CreateSchedule(ctx, store.ScheduleInput{Name: "rev", Kind: "onetime", AnchorDate: &rev})
	if err != nil {
		t.Fatalf("create rev schedule: %v", err)
	}
	expSched, err := f.Store.CreateSchedule(ctx, store.ScheduleInput{Name: "exp", Kind: "onetime", AnchorDate: &exp})
	if err != nil {
		t.Fatalf("create exp schedule: %v", err)
	}
	beca := f.IDs.BecaAgua
	if _, err := f.Store.CreateBudgetLine(ctx, id, store.BudgetLineInput{
		SubsidiaryID: f.IDs.MX, AccountID: f.IDs.GovernmentGrants, FundID: &beca,
		ProgramID: f.IDs.Educacion, Amount: 1_000_000, Currency: "MXN", ScheduleID: revSched,
	}); err != nil {
		t.Fatalf("create rev line: %v", err)
	}
	if _, err := f.Store.CreateBudgetLine(ctx, id, store.BudgetLineInput{
		SubsidiaryID: f.IDs.MX, AccountID: f.IDs.ProgramSupplies, FundID: &beca,
		ProgramID: f.IDs.Educacion, Amount: 400_000, Currency: "MXN", ScheduleID: expSched,
	}); err != nil {
		t.Fatalf("create exp line: %v", err)
	}

	rep := budgetReport(t, reports.CashflowProjectionReportID)
	// Root scope: BecaAgua spans US+MX (its consolidated MXN balance is root-visible).
	p := reports.Params{
		Scope: f.IDs.Root, Budget: id, From: "2026-07-01", To: "2026-12-31",
		Granularity: reports.GranMonth, Lang: "en",
	}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run cashflow projection: %v", err)
	}

	becaName := mustFundName(t, f, f.IDs.BecaAgua)
	// The Start column is col 2, End is the last column; find the BecaAgua/MXN row.
	row, ok := cashflowRow(table, becaName, "MXN")
	if !ok {
		t.Fatalf("no BecaAgua/MXN row")
	}
	start := row.Cells[2].Minor
	end := row.Cells[len(row.Cells)-1].Minor
	if start != 9_700_000 {
		t.Errorf("BecaAgua/MXN start = %d, want 9700000 (FundBalancesAsOf period start)", start)
	}
	if end != 10_300_000 {
		t.Errorf("BecaAgua/MXN end = %d, want 10300000 (9700000 + 1000000 - 400000)", end)
	}

	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "cashflow_projection.txt", []byte(textDump))
	checkGolden(t, "cashflow_projection.csv", csvBuf.Bytes())
}

// --- p26.80: the SAMPLE-BUDGET seam goldens --------------------------------
// The two tests above build a tiny bespoke budget inline to pin one hand-verified
// figure each. These two run BOTH budget reports over the canonical, opt-in
// fixture.ExtendSampleBudget seam (a fuller budget: several lines across programs /
// accounts / funds / subsidiaries on the seeded common schedules), giving the
// reports realistic coverage and a reviewed golden. They emit NEW golden files
// (*_sample.{txt,csv}) so the existing inline goldens are untouched. Numbers are
// SYNTHETIC (rule 11); the goldens are the oracle (reviewed via `make golden`),
// with a couple of structural cell assertions to catch a silently-empty table.

// TestActualsVsBudgetSampleGolden runs actuals-vs-budget over the sample-budget seam
// at monthly granularity, root scope (consolidated), and compares text + CSV goldens.
func TestActualsVsBudgetSampleGolden(t *testing.T) {
	f := fixture.New(t)
	f.ExtendSampleBudget(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	sb := f.Expected.SampleBudget
	rep := budgetReport(t, reports.ActualsVsBudgetReportID)
	p := reports.Params{
		Scope: f.IDs.Root, Budget: sb.Budget, From: sb.From, To: sb.To,
		Granularity: reports.GranMonth, Lang: "en",
	}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run actuals-vs-budget (sample): %v", err)
	}

	// Structural sanity: the sample budget must produce data rows (a silently-empty
	// table would still "pass" a golden compare against an empty golden).
	if dataRows := countDataRows(table); dataRows == 0 {
		t.Fatalf("sample actuals-vs-budget produced no data rows")
	}

	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "actuals_vs_budget_sample.txt", []byte(textDump))
	checkGolden(t, "actuals_vs_budget_sample.csv", csvBuf.Bytes())
}

// TestCashflowProjectionSampleGolden runs cashflow-projection over the sample-budget
// seam at monthly granularity, root scope, and compares text + CSV goldens.
func TestCashflowProjectionSampleGolden(t *testing.T) {
	f := fixture.New(t)
	f.ExtendSampleBudget(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	sb := f.Expected.SampleBudget
	rep := budgetReport(t, reports.CashflowProjectionReportID)
	p := reports.Params{
		Scope: f.IDs.Root, Budget: sb.Budget, From: sb.From, To: sb.To,
		Granularity: reports.GranMonth, Lang: "en",
	}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run cashflow projection (sample): %v", err)
	}

	if dataRows := countDataRows(table); dataRows == 0 {
		t.Fatalf("sample cashflow projection produced no data rows")
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

// countDataRows counts the RowData rows in a table (structural sanity for the sample
// goldens -- a nonzero count proves the budget actually produced report rows).
func countDataRows(t reports.Table) int {
	n := 0
	for _, r := range t.Rows {
		if r.Kind == reports.RowData {
			n++
		}
	}
	return n
}

// TestBudgetReportsNoBudget: with no budget chosen (Budget == 0) each report returns
// an empty Table (the framework's nothing-to-show rule) so a bare hit renders 200.
func TestBudgetReportsNoBudget(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	for _, id := range []string{reports.ActualsVsBudgetReportID, reports.CashflowProjectionReportID} {
		rep := budgetReport(t, id)
		p := reports.Params{Scope: f.IDs.Root, From: "2026-07-01", To: "2026-12-31", Lang: "en"} // Budget == 0
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

// mustAccountName resolves an account's name per lang, failing the test on error.
func mustAccountName(t *testing.T, f *fixture.Fixture, id int64, lang string) string {
	t.Helper()
	name, err := f.Store.AccountName(context.Background(), id, lang)
	if err != nil {
		t.Fatalf("account name %d: %v", id, err)
	}
	return name
}

// mustFundName resolves a fund's stored name, failing the test on error.
func mustFundName(t *testing.T, f *fixture.Fixture, id int64) string {
	t.Helper()
	funds, err := f.Store.ListFunds(context.Background())
	if err != nil {
		t.Fatalf("list funds: %v", err)
	}
	for _, fd := range funds {
		if fd.ID == id {
			return fd.Name
		}
	}
	t.Fatalf("fund %d not found", id)
	return ""
}

// mustProgramName resolves a program's stored name, failing the test on error.
func mustProgramName(t *testing.T, f *fixture.Fixture, id int64) string {
	t.Helper()
	tree, err := f.Store.ProgramTree(context.Background())
	if err != nil {
		t.Fatalf("program tree: %v", err)
	}
	for _, n := range tree {
		if n.ID == id {
			return n.Name
		}
	}
	t.Fatalf("program %d not found", id)
	return ""
}

// NOTE: the drilled-actual reconciliation uses the shared drillSum helper
// (fund_activity_test.go), which builds the store.DrillFilter from a *reports.Drill
// exactly as the web drill handler does.
