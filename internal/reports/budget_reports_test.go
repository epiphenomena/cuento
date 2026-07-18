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
		Scope: f.IDs.Root, Budget: sp.Plan, From: sp.From, To: sp.To,
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
		Scope: f.IDs.Root, Budget: sp.Plan, From: sp.From, To: sp.To,
		Granularity: reports.GranMonth, Lang: "en",
	}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run budget variance: %v", err)
	}

	if dataRows := countDataRows(table); dataRows == 0 {
		t.Fatalf("budget variance produced no data rows")
	}

	// At least one DATA row must carry a non-zero BUDGETED figure (proving the plan's
	// projected splits reached the report). The budgeted cell (col 5) must NOT drill.
	var sawBudgeted bool
	for _, r := range table.Rows {
		if r.Kind != reports.RowData || len(r.Cells) < 8 {
			continue
		}
		if r.Cells[5].Minor != 0 {
			sawBudgeted = true
			if r.Cells[5].Drill != nil {
				t.Errorf("budgeted cell must NOT be drillable (it is a plan)")
			}
		}
		// An unrestricted (fund 0) actual cell is not drillable; the fund label cell
		// renders the i18n key for the unrestricted group.
		if r.Cells[2].Text == "reports.budget.unrestricted" && r.Cells[6].Drill != nil {
			t.Errorf("unrestricted actual cell must NOT be drillable")
		}
		// A restricted (proper-noun fund) actual cell that drills must reconcile.
		if r.Cells[2].Text != "reports.budget.unrestricted" && r.Cells[6].Drill != nil {
			if sum := drillSum(t, f, r.Cells[6].Drill); sum != r.Cells[6].Minor {
				t.Errorf("drilled actual sum = %d, want %d (reconciliation)", sum, r.Cells[6].Minor)
			}
		}
	}
	if !sawBudgeted {
		t.Errorf("no variance row carried a non-zero budgeted figure")
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
		p := reports.Params{Scope: f.IDs.Root, From: "2026-01-01", To: "2026-12-31", Lang: "en"} // Budget == 0
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
