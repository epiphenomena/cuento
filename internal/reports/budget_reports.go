package reports

// budget_reports.go is p27.3: the two redesigned BUDGET-group reports, layered over
// the p27.2 split-derived model via the budget_plan.go toolkit
// (CashflowProjectionPlan / BudgetVariancePlan) -- replacing the retired
// schedule-expansion reports (actuals_vs_budget + the old cashflow_projection body).
// They own NO HTTP and NO html/template (like every Phase-15 report) -- the web layer
// wraps their Table in the app shell and drives them through the Registry. Both take
// the report-specific BUDGET param (?budget=, now a budget-PLAN id) and default their
// period to the plan's own span of split dates (web layer).
//
// TWO REPORTS, ONE GROUP: a projected cash series vs. a projected-vs-actual variance
// table are distinct concerns, so they are two reports, not one with a mode param.
// Both live in the "budget" report group.
//
//   1. cashflow_projection (id REUSED) -- the projected CASH series. Rows per fund/
//      currency: Fund | Currency | Start | <per-bucket projected balance...> | End.
//      Start = each fund's ACTUAL current-cash balance at the period start (per fund,
//      DECISIONS tension 1); each bucket column = the projected balance at that
//      bucket's END (Start rolled forward through the projected inflows/outflows to
//      that date); End = the final projected balance. Inflow/outflow is classified by
//      the categorized leg's account type (revenue/receivable = inflow; expense/
//      payable = outflow). No drill (a projection is a plan, not posted transactions).
//
//   2. budget_variance (NEW id, retiring actuals_vs_budget) -- the projected-vs-actual
//      variance table. Rows = (bucket, key): Bucket | Account | Fund | Program |
//      Currency | Budgeted | Actual | Variance. Variance = Actual - Budgeted (positive
//      = higher net-debit than budget = expense OVERSPEND / revenue UNDER-collection).
//      DRILL on the ACTUAL cell only (the budgeted column is a plan): the actual drills
//      to the splits producing it, clamped to the bucket span. Unrestricted (fund 0)
//      actual cells are NOT drillable (the drill query filters sp.fund_id = ?, which
//      cannot express fund-IS-NULL).

import (
	"context"
	"sort"
)

// CashflowProjectionReportID / BudgetVarianceReportID are the two budget reports'
// ids (URL slug + registry key), both under the "budget" group. The cashflow id is
// REUSED from the retired report; budget_variance is NEW (retiring actuals_vs_budget).
const (
	CashflowProjectionReportID = "cashflow_projection"
	BudgetVarianceReportID     = "budget_variance"
)

// registerCashflowProjection registers the cashflow-projection report (p27.3) under
// the "budget" group. It offers the period (from/to -- defaulted to the plan's span
// by the web layer), the granularity (week/month/year buckets), and the report-
// specific BUDGET (plan) selector.
func registerCashflowProjection(reg *Registry) {
	reg.Register(Report{
		ID:                 CashflowProjectionReportID,
		TitleKey:           "reports.cashflow_projection.title",
		Group:              "budget",
		ParamsSpec:         ParamsSpec{Period: true, Granularity: true, Budget: true},
		Run:                runCashflowProjection,
		ProgramDimensioned: true, // p27.4: budget-splits carry a program (grant-subtree filterable).
	})
}

// registerBudgetVariance registers the budget-variance report (p27.3) under the
// "budget" group. Same params as cashflow (both bucket by the granularity).
func registerBudgetVariance(reg *Registry) {
	reg.Register(Report{
		ID:                 BudgetVarianceReportID,
		TitleKey:           "reports.budget_variance.title",
		Group:              "budget",
		ParamsSpec:         ParamsSpec{Period: true, Granularity: true, Budget: true},
		Run:                runBudgetVariance,
		ProgramDimensioned: true, // p27.4: variance rows are keyed by program (grant-subtree filterable).
	})
}

// runBudgetVariance computes the budget-variance Table (p27.3). Rows = one per
// (bucket, key): Bucket | Account | Fund | Program | Currency | Budgeted | Actual |
// Variance. The ACTUAL cell drills (period, clamped to the bucket span) for a
// restricted (fund != 0) cell; the budgeted cell never drills (it is a plan).
func runBudgetVariance(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	t := Table{
		Columns: []Column{
			{HeaderKey: "reports.budget_variance.col.bucket", Align: AlignLeft},
			{HeaderKey: "reports.budget_variance.col.account", Align: AlignLeft},
			{HeaderKey: "reports.budget_variance.col.fund", Align: AlignLeft},
			{HeaderKey: "reports.budget_variance.col.program", Align: AlignLeft},
			{HeaderKey: "reports.budget_variance.col.currency", Align: AlignLeft},
			{HeaderKey: "reports.budget_variance.col.budgeted", Align: AlignRight},
			{HeaderKey: "reports.budget_variance.col.actual", Align: AlignRight},
			{HeaderKey: "reports.budget_variance.col.variance", Align: AlignRight},
		},
	}

	// No budget chosen: an empty table (the framework's nothing-to-show rule).
	if p.Budget == 0 {
		return t, nil
	}

	scope := Scope{Sub: p.Scope}
	g := p.Granularity
	if g == GranNone {
		g = GranMonth
	}

	cells, err := tk.BudgetVariancePlan(ctx, scope, p.Budget, p.From, p.To, g)
	if err != nil {
		return Table{}, err
	}

	names, err := budgetKeyNames(ctx, tk, p.LangOr())
	if err != nil {
		return Table{}, err
	}

	// Running per-currency section totals of the three money columns, re-summed from the
	// emitted cells and closed with a total row -- so a golden diff surfaces a dropped row.
	totalBudgeted := map[string]int64{}
	totalActual := map[string]int64{}
	var ccyOrder []string
	seenCcy := map[string]bool{}

	for _, c := range cells {
		key := c.Key
		bEnd, err := bucketEnd(c.Bucket, g)
		if err != nil {
			return Table{}, err
		}

		actualCell := MoneyCell(c.Actual, key.Currency)
		if d := actualDrill(key, c.Bucket, bEnd, p.From, p.To); d != nil {
			actualCell = actualCell.WithDrill(d)
		}

		t.Rows = append(t.Rows, Row{
			Cells: []Cell{
				DateCell(c.Bucket),
				TextCell(names.account[key.Account]),
				fundLabelCell(key.Fund, names.fund),
				TextCell(names.program[key.Program]),
				TextCell(key.Currency),
				MoneyCell(c.Budgeted, key.Currency), // budgeted: a plan, NOT drillable
				actualCell,
				MoneyCell(c.Variance, key.Currency),
			},
			Kind: RowData,
		})

		totalBudgeted[key.Currency] += c.Budgeted
		totalActual[key.Currency] += c.Actual
		if !seenCcy[key.Currency] {
			seenCcy[key.Currency] = true
			ccyOrder = append(ccyOrder, key.Currency)
		}
	}

	sort.Strings(ccyOrder)
	for _, ccy := range ccyOrder {
		b, a := totalBudgeted[ccy], totalActual[ccy]
		t.Rows = append(t.Rows, Row{
			Cells: []Cell{
				LabelCell("reports.budget_variance.total"),
				TextCell(""), TextCell(""), TextCell(""),
				TextCell(ccy),
				MoneyCell(b, ccy),
				MoneyCell(a, ccy),
				MoneyCell(a-b, ccy),
			},
			Kind: RowTotal,
		})
	}

	return t, nil
}

// actualDrill builds the DrillPeriod filter for a variance cell's actuals of key over
// bucket [bucketStart,bucketEnd], clamped to the report window [from,to]. It returns
// nil (not drillable) for an UNRESTRICTED (fund 0) cell: the drill store query filters
// sp.fund_id = ? which cannot express fund-IS-NULL, so a nil FundID would sum every
// fund and not reconcile (carried forward from the retired report). A restricted (fund
// != 0) cell drills by its single fund; the drilled native splits' signed sum equals
// the cell's Actual (the p15.3d reconciliation invariant).
//
// SCOPE = the KEY's own subsidiary, NOT the report scope. BudgetKeyActivity groups
// actuals by t.subsidiary_id, so each cell is per-subsidiary; DrillSplits closes the
// descendant set of its Scope, so drilling with the (possibly consolidated) report
// scope would sum a sibling subsidiary's matching splits and OVER-count.
func actualDrill(key BudgetKey, bucketStart, bucketEnd, from, to string) *Drill {
	if key.Fund == 0 {
		return nil // unrestricted: not drillable (fund-IS-NULL is inexpressible)
	}
	f := bucketStart
	if from != "" && from > f {
		f = from
	}
	tt := bucketEnd
	if to != "" && to < tt {
		tt = to
	}
	fund := key.Fund
	prog := int64(key.Program)
	return &Drill{
		Scope:      key.Subsidiary,
		AccountIDs: []int64{int64(key.Account)},
		Currency:   key.Currency,
		FundID:     &fund,
		ProgramID:  &prog,
		Mode:       DrillPeriod,
		From:       f,
		To:         tt,
	}
}

// runCashflowProjection computes the cashflow-projection Table (p27.3). Rows per
// fund/currency: Fund | Currency | Start | <per-bucket projected balance...> | End.
// Start = each fund's ACTUAL current-cash balance at the period start; each bucket
// column = the projected balance at that bucket's END (Start rolled forward through
// the projected inflow/outflow amounts on-or-before that date); End = the final
// projected balance. No drill (a projection is a plan, not posted transactions).
func runCashflowProjection(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	g := p.Granularity
	if g == GranNone {
		g = GranMonth
	}

	buckets, err := projectionBuckets(p.From, p.To, g)
	if err != nil {
		return Table{}, err
	}

	t := Table{Columns: cashflowColumns(buckets)}

	if p.Budget == 0 {
		return t, nil
	}

	scope := Scope{Sub: p.Scope}
	series, err := tk.CashflowProjectionPlan(ctx, scope, p.Budget, p.From, p.To)
	if err != nil {
		return Table{}, err
	}

	names, err := budgetKeyNames(ctx, tk, p.LangOr())
	if err != nil {
		return Table{}, err
	}

	// Deterministic row order: by fund id then currency.
	fcs := make([]FundCurrency, 0, len(series))
	for fc := range series {
		fcs = append(fcs, fc)
	}
	sort.Slice(fcs, func(i, j int) bool {
		if fcs[i].Fund != fcs[j].Fund {
			return fcs[i].Fund < fcs[j].Fund
		}
		return fcs[i].Currency < fcs[j].Currency
	})

	for _, fc := range fcs {
		ser := series[fc]
		cells := make([]Cell, 0, len(buckets)+4)
		cells = append(
			cells,
			fundLabelCell(fc.Fund, names.fund),
			TextCell(fc.Currency),
			MoneyCell(ser.Start, fc.Currency),
		)
		for _, b := range buckets {
			cells = append(cells, MoneyCell(balanceAt(ser, b.end), fc.Currency))
		}
		cells = append(cells, MoneyCell(ser.End, fc.Currency))
		t.Rows = append(t.Rows, Row{Cells: cells, Kind: RowData})
	}

	return t, nil
}

// projBucket is one cashflow-projection column: its start (the bucket key, used as
// the localized-verbatim header) and its inclusive end (the date the running balance
// is sampled at).
type projBucket struct {
	start string
	end   string
}

// projectionBuckets returns the period buckets over [from,to] at granularity g, each
// with its start (bucket key) and inclusive end. The bucket keys are the same ones
// bucketKey produces, so the projection columns align with the variance rows.
func projectionBuckets(from, to string, g Granularity) ([]projBucket, error) {
	if from == "" || to == "" {
		return nil, nil
	}
	var out []projBucket
	cur := from
	for cur <= to {
		start, err := bucketKey(cur, g)
		if err != nil {
			return nil, err
		}
		end, err := bucketEnd(start, g)
		if err != nil {
			return nil, err
		}
		out = append(out, projBucket{start: start, end: end})
		next, err := nextDay(end)
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return out, nil
}

// cashflowColumns builds the cashflow report's columns: Fund | Currency | Start |
// <bucket end date...> | End. Each bucket column's header is its END date (a verbatim
// date the web layer reformats per the user's setting -- a period marker, not a label
// key), so a reader sees the projected balance AS OF that date.
func cashflowColumns(buckets []projBucket) []Column {
	cols := []Column{
		{HeaderKey: "reports.cashflow_projection.col.fund", Align: AlignLeft},
		{HeaderKey: "reports.cashflow_projection.col.currency", Align: AlignLeft},
		{HeaderKey: "reports.cashflow_projection.col.start", Align: AlignRight},
	}
	for _, b := range buckets {
		cols = append(cols, Column{HeaderKey: b.end, Align: AlignRight})
	}
	cols = append(cols, Column{HeaderKey: "reports.cashflow_projection.col.end", Align: AlignRight})
	return cols
}

// balanceAt returns the projected running balance of ser at (inclusive) date d: the
// At of the latest FlowDate on-or-before d, or Start when no flow has occurred yet
// (the balance is constant between flow dates). FlowDates is ascending, so a linear
// scan finds the last one <= d.
func balanceAt(ser ProjectionSeries, d string) int64 {
	bal := ser.Start
	for _, fd := range ser.FlowDates {
		if fd <= d {
			bal = ser.At[fd]
		} else {
			break
		}
	}
	return bal
}

// keyNames holds the resolved display names a budget report row needs: account name
// (per lang, D5), fund name (a stored proper noun), and program name (a stored proper
// noun). Loaded once per report run (bounded reference data).
type keyNames struct {
	account map[AccountID]string
	fund    map[int64]string
	program map[ProgramID]string
}

// budgetKeyNames loads the account/fund/program display names a budget report row
// resolves, once per run. Account names are per-lang (D5); fund and program names are
// stored proper nouns (single Name).
func budgetKeyNames(ctx context.Context, tk *Toolkit, lang string) (keyNames, error) {
	st := tk.Store()
	tree, err := st.Tree(ctx, lang, nil)
	if err != nil {
		return keyNames{}, err
	}
	acct := make(map[AccountID]string, len(tree))
	for _, r := range tree {
		acct[r.ID] = r.Name
	}
	fund, err := fundNames(ctx, st)
	if err != nil {
		return keyNames{}, err
	}
	progTree, err := st.ProgramTree(ctx)
	if err != nil {
		return keyNames{}, err
	}
	prog := make(map[ProgramID]string, len(progTree))
	for _, n := range progTree {
		prog[n.ID] = n.Name
	}
	return keyNames{account: acct, fund: fund, program: prog}, nil
}

// fundLabelCell builds the FUND column cell: the fund's stored name for a restricted
// key, or the localized "Unrestricted" label for fund 0 (D20).
func fundLabelCell(fund int64, funds map[int64]string) Cell {
	if fund == 0 {
		return LabelCell("reports.budget.unrestricted")
	}
	return TextCell(funds[fund])
}
