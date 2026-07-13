package reports

// budget_reports.go is p19.4: the two BUDGET-group reports, layered over the p19.2
// budget toolkit (budget.go: BudgetVsActual / CashflowProjection). They own NO HTTP
// and NO html/template (like every Phase-15 report) -- the web layer wraps their
// Table in the app shell and drives them through the Registry. Both take the report-
// specific BUDGET param (?budget=, mirroring the Account/Fund/Program/Reconciliation
// params) and default their period to the chosen budget's own period (web layer).
//
// TWO REPORTS, ONE GROUP (DECISIONS p19.4): the two concerns are distinct -- a
// forecast/variance table vs. a projected-balance series -- so they are two reports,
// not one report with a mode param. Both live in the new "budget" report group.
//
//   1. actuals_vs_budget -- the forecast/variance report. Rows = (bucket, budget-line
//      key): Bucket | Account | Fund | Program | Currency | Budgeted | Actual |
//      Variance. Granularity (week/month/year) buckets each occurrence WHOLE into the
//      single bucket its date falls in (NO pro-rata, the p19.2 design point -- a
//      monthly line shows in ONE bucket row). Per-fund: the Fund column breaks
//      restricted vs unrestricted out (fund 0 = unrestricted, D20). Variance = Actual
//      - Budgeted (the p19.2 sign: positive = higher net-debit than budget = expense
//      OVERSPEND / revenue UNDER-collection). DRILL on the ACTUAL cell only (the
//      budgeted column is a plan, not transactions): the actual drills to the splits
//      producing it, clamped to the bucket's date span. Unrestricted (fund 0) actual
//      cells are NOT drillable (the drill query filters sp.fund_id = ?, which cannot
//      express fund-IS-NULL; a nil FundID would sum ALL funds and not reconcile) --
//      documented in DECISIONS.
//
//   2. cashflow_projection -- the projected fund-balance series. Rows per fund/
//      currency: Fund | Currency | Start | <per-bucket projected balance...> | End.
//      Start = the current actual net-asset fund balance at the period start
//      (FundBalancesAsOf(from)); each bucket column = the projected balance at that
//      bucket's END (Start rolled forward through the budgeted occurrence flows to
//      that date); End = the final projected balance. This is the "will the restricted
//      fund run dry / what's the projected unrestricted position" view. No drill (a
//      projection is a plan, not posted transactions).

import (
	"context"
	"sort"
)

// ActualsVsBudgetReportID / CashflowProjectionReportID are the two budget reports'
// ids (URL slug + registry key), both under the "budget" group.
const (
	ActualsVsBudgetReportID    = "actuals_vs_budget"
	CashflowProjectionReportID = "cashflow_projection"
)

// registerActualsVsBudget registers the actuals-vs-budget report (p19.4) under the
// "budget" group. It offers the period (from/to -- defaulted to the budget's period
// by the web layer), the granularity (week/month/year buckets), and the report-
// specific BUDGET selector; the shared web params form renders all three.
func registerActualsVsBudget(reg *Registry) {
	reg.Register(Report{
		ID:         ActualsVsBudgetReportID,
		TitleKey:   "reports.actuals_vs_budget.title",
		Group:      "budget",
		ParamsSpec: ParamsSpec{Period: true, Granularity: true, Budget: true},
		Run:        runActualsVsBudget,
	})
}

// registerCashflowProjection registers the cashflow-projection report (p19.4) under
// the "budget" group. Same params as actuals-vs-budget (the projection buckets its
// flow dates by the same granularity).
func registerCashflowProjection(reg *Registry) {
	reg.Register(Report{
		ID:         CashflowProjectionReportID,
		TitleKey:   "reports.cashflow_projection.title",
		Group:      "budget",
		ParamsSpec: ParamsSpec{Period: true, Granularity: true, Budget: true},
		Run:        runCashflowProjection,
	})
}

// runActualsVsBudget computes the actuals-vs-budget Table (p19.4). Rows = one per
// (bucket, budget-line key): Bucket | Account | Fund | Program | Currency | Budgeted
// | Actual | Variance. The ACTUAL cell drills (period, clamped to the bucket span)
// for a restricted (fund != 0) cell; the budgeted cell never drills (it is a plan).
func runActualsVsBudget(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	t := Table{
		Columns: []Column{
			{HeaderKey: "reports.actuals_vs_budget.col.bucket", Align: AlignLeft},
			{HeaderKey: "reports.actuals_vs_budget.col.account", Align: AlignLeft},
			{HeaderKey: "reports.actuals_vs_budget.col.fund", Align: AlignLeft},
			{HeaderKey: "reports.actuals_vs_budget.col.program", Align: AlignLeft},
			{HeaderKey: "reports.actuals_vs_budget.col.currency", Align: AlignLeft},
			{HeaderKey: "reports.actuals_vs_budget.col.budgeted", Align: AlignRight},
			{HeaderKey: "reports.actuals_vs_budget.col.actual", Align: AlignRight},
			{HeaderKey: "reports.actuals_vs_budget.col.variance", Align: AlignRight},
		},
	}

	// No budget chosen: an empty table (the framework's nothing-to-show rule).
	if p.Budget == 0 {
		return t, nil
	}

	scope := Scope{Sub: p.Scope}
	g := p.Granularity
	if g == GranNone {
		// The report needs bucketing to show occurrences; default to monthly when the
		// user leaves granularity unset (the web layer offers week/month/year).
		g = GranMonth
	}

	cells, err := tk.BudgetVsActual(ctx, scope, p.Budget, p.From, p.To, g)
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

	// Total rows per currency (sorted), one per currency present: Σbudgeted, Σactual,
	// Σvariance = Σactual - Σbudgeted (the same identity per cell).
	sort.Strings(ccyOrder)
	for _, ccy := range ccyOrder {
		b, a := totalBudgeted[ccy], totalActual[ccy]
		t.Rows = append(t.Rows, Row{
			Cells: []Cell{
				LabelCell("reports.actuals_vs_budget.total"),
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

// actualDrill builds the DrillPeriod filter for an actuals cell of key over bucket
// [bucketStart,bucketEnd], clamped to the report window [from,to]. It returns nil
// (not drillable) for an UNRESTRICTED (fund 0) cell: the drill store query filters
// sp.fund_id = ? which cannot express fund-IS-NULL, so a nil FundID would sum every
// fund and not reconcile to the unrestricted-only actual (DECISIONS p19.4). A
// restricted (fund != 0) cell drills by its single fund; the drilled native splits'
// signed sum equals the cell's Actual (the p15.3d reconciliation invariant).
//
// SCOPE = the KEY's own subsidiary, NOT the report scope. BudgetKeyActivity groups
// actuals by t.subsidiary_id, so each cell is per-subsidiary; DrillSplits closes the
// descendant set of its Scope, so drilling with the (possibly consolidated) report
// scope would sum a sibling subsidiary's matching splits and OVER-count. Using the
// key's exact posting sub (closure {itself} for a leaf) makes the drilled sum equal
// the per-subsidiary cell at ANY report scope, by construction (DECISIONS p19.4).
func actualDrill(key BudgetKey, bucketStart, bucketEnd, from, to string) *Drill {
	if key.Fund == 0 {
		return nil // unrestricted: not drillable (fund-IS-NULL is inexpressible)
	}
	// Clamp the bucket span to the report window: a Monday-start week bucket can begin
	// before `from`, and the last bucket can end after `to`; the actual is bounded to
	// [from,to], so the drill must be too, or the sum won't reconcile at the edges.
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

// runCashflowProjection computes the cashflow-projection Table (p19.4). Rows per
// fund/currency: Fund | Currency | Start | <per-bucket projected balance...> | End.
// Start = the current actual net-asset fund balance at the period start; each bucket
// column = the projected balance at that bucket's END (Start rolled forward through
// the budgeted occurrence flows on-or-before that date); End = the final projected
// balance. No drill (a projection is a plan, not posted transactions).
func runCashflowProjection(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	g := p.Granularity
	if g == GranNone {
		g = GranMonth
	}

	// The bucket columns: one per period bucket over [from,to], their END dates the
	// projection is sampled at. Built independent of the projection so the columns are
	// stable even when a fund has no flow in a bucket.
	buckets, err := projectionBuckets(p.From, p.To, g)
	if err != nil {
		return Table{}, err
	}

	t := Table{Columns: cashflowColumns(buckets)}

	if p.Budget == 0 {
		return t, nil
	}

	scope := Scope{Sub: p.Scope}
	series, err := tk.CashflowProjection(ctx, scope, p.Budget, p.From, p.To)
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
// BudgetVsActual/bucketKey produce, so the projection columns align with the AVB
// rows for the same budget/period/granularity.
func projectionBuckets(from, to string, g Granularity) ([]projBucket, error) {
	if from == "" || to == "" {
		return nil, nil
	}
	// Walk bucket starts from `from` to `to`: bucket(from), then step to the next
	// bucket's start (this bucket's end + 1 day), until past `to`.
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
		// Advance to the day after this bucket's end (the next bucket's first day).
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
// key, or the localized "Unrestricted" label for fund 0 (D20). Mirrors the sibling
// reports' fund cells.
func fundLabelCell(fund int64, funds map[int64]string) Cell {
	if fund == 0 {
		return LabelCell("reports.budget.unrestricted")
	}
	return TextCell(funds[fund])
}
