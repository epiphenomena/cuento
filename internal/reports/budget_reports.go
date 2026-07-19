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
		ID:         CashflowProjectionReportID,
		TitleKey:   "reports.cashflow_projection.title",
		Group:      "budget",
		ParamsSpec: ParamsSpec{Period: true, Granularity: true, Budget: true},
		Run:        runCashflowProjection,
		// p27.4b: NOT ProgramDimensioned. The projection's opening/running/end balances come
		// from CurrentCashFundBalancesAsOf -- per-FUND spendable cash, which carries NO
		// program dimension (cash isn't program-tagged). Only the flow deltas carry a
		// program; filtering flows but not the opening would ship org-wide opening balances
		// (the leak). Stripping the opening would gut the projection. So a purely
		// program-scoped grant does NOT reach it (needs an unscoped "budget" grant); the
		// program-carrying budget_variance keeps the "budget" group's scoped reach.
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
		WideMatrix:         true, // p30.9: monthly-column pivot fans wide -- full-viewport shell.
		MeasureToggle:      true, // p30.9: budgeted/actual/variance client-side measure toggle.
	})
}

// runBudgetVariance computes the budget-variance Table (p30.9 redesign). It PIVOTS the
// (bucket, key) cells into a MONTHLY GRID: one ROW per key -- (fully-qualified account
// path, fund, program, currency) -- and one COLUMN per month bucket across the plan
// span, plus a trailing per-key Total column. Each grid cell is a CellMeasures folding
// all three measures (budgeted / actual / variance) so the web layer can toggle which
// one shows client-side (no round trip); the text/CSV renderers emit all three. The
// row-TOTAL column and the per-currency grand-total rows carry an over/under magnitude
// bucket (VarianceBucket) the web layer color-codes. Variance = Actual − Budgeted (the
// net-debit convention: positive = over budget / under-collection), unchanged from p27.3.
//
// The ACTUAL measure of a per-month cell drills (period, clamped to the bucket span) for
// a restricted (fund != 0) key; the budgeted/variance measures never drill (a plan and a
// derived figure). Total cells never drill (they span months).
func runBudgetVariance(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	g := p.Granularity
	if g == GranNone {
		g = GranMonth
	}

	// The month columns: the shared bucket span (same keys BudgetVariancePlan buckets by,
	// so a cell's Bucket lands in exactly one column). Header = the bucket START date
	// (verbatim; the web layer reformats per the user's date setting).
	buckets, err := budgetVarianceBuckets(p, g)
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: budgetVarianceColumns(buckets)}

	// No budget chosen: an empty table (the framework's nothing-to-show rule).
	if p.Budget == 0 {
		return t, nil
	}

	scope := Scope{Sub: p.Scope}
	cells, err := tk.BudgetVariancePlan(ctx, scope, p.Budget, p.From, p.To, g)
	if err != nil {
		return Table{}, err
	}

	names, err := budgetKeyNames(ctx, tk, p.LangOr())
	if err != nil {
		return Table{}, err
	}

	// Group the flat cells by key, indexed by bucket, so each row spreads across the
	// month columns. keyOrder keeps deterministic row order (first-seen, then sorted).
	type keyAgg struct {
		key     BudgetKey
		byMonth map[string]BudgetVsActualCell
	}
	rows := map[BudgetKey]*keyAgg{}
	var keyOrder []BudgetKey
	for _, c := range cells {
		agg, ok := rows[c.Key]
		if !ok {
			agg = &keyAgg{key: c.Key, byMonth: map[string]BudgetVsActualCell{}}
			rows[c.Key] = agg
			keyOrder = append(keyOrder, c.Key)
		}
		agg.byMonth[c.Bucket] = c
	}
	sortBudgetKeys(keyOrder, names)

	// Per-currency grand totals (re-summed from the emitted cells, closed by total rows).
	grandBudgeted := map[string]int64{}
	grandActual := map[string]int64{}
	var ccyOrder []string
	seenCcy := map[string]bool{}

	for _, key := range keyOrder {
		agg := rows[key]
		cs := make([]Cell, 0, len(buckets)+5)
		cs = append(
			cs,
			TextCell(names.account[key.Account]),
			fundLabelCell(key.Fund, names.fund),
			TextCell(names.program[key.Program]),
			TextCell(key.Currency),
		)
		var rowB, rowA int64
		for _, b := range buckets {
			c := agg.byMonth[b.start] // zero-valued when this key has no activity that month
			rowB += c.Budgeted
			rowA += c.Actual
			mc := MeasuresCell(c.Budgeted, c.Actual, c.Variance, key.Currency, VarianceNeutral)
			if d := actualDrill(key, b.start, b.end, p.From, p.To); d != nil {
				mc = mc.WithDrill(d)
			}
			cs = append(cs, mc)
		}
		// Row TOTAL: the per-key sum across months, magnitude-classed for color.
		cs = append(cs, MeasuresCell(rowB, rowA, rowA-rowB, key.Currency, VarianceBucket(rowB, rowA-rowB)))
		t.Rows = append(t.Rows, Row{Cells: cs, Kind: RowData})

		grandBudgeted[key.Currency] += rowB
		grandActual[key.Currency] += rowA
		if !seenCcy[key.Currency] {
			seenCcy[key.Currency] = true
			ccyOrder = append(ccyOrder, key.Currency)
		}
	}

	// Per-currency grand-total rows: a label + currency, blank month columns, and the
	// rolled Total cell (magnitude-classed) so the color reads at the bottom line.
	sort.Strings(ccyOrder)
	for _, ccy := range ccyOrder {
		b, a := grandBudgeted[ccy], grandActual[ccy]
		cs := make([]Cell, 0, len(buckets)+5)
		cs = append(cs, LabelCell("reports.budget_variance.total"), TextCell(""), TextCell(""), TextCell(ccy))
		for range buckets {
			cs = append(cs, BlankMoneyCell())
		}
		cs = append(cs, MeasuresCell(b, a, a-b, ccy, VarianceBucket(b, a-b)))
		t.Rows = append(t.Rows, Row{Cells: cs, Kind: RowTotal})
	}

	return t, nil
}

// budgetVarianceBuckets returns the month (granularity g) columns over the report window
// [p.From,p.To], each carrying its START (the bucket key BudgetVariancePlan buckets cells
// by, used as the verbatim column header) and inclusive END (the drill clamp). It reuses
// the same projectionBuckets machinery the cashflow report uses, so the columns align
// with the pivoted variance cells.
func budgetVarianceBuckets(p Params, g Granularity) ([]projBucket, error) {
	return projectionBuckets(p.From, p.To, g)
}

// budgetVarianceColumns builds the monthly-pivot columns: Account | Fund | Program |
// Currency | <month start date...> | Total. Each month column header is its START date
// (a verbatim period marker the web layer reformats per the user's setting, like the
// cashflow report's bucket headers -- so no per-month i18n keys are invented).
func budgetVarianceColumns(buckets []projBucket) []Column {
	cols := []Column{
		{HeaderKey: "reports.budget_variance.col.account", Align: AlignLeft},
		{HeaderKey: "reports.budget_variance.col.fund", Align: AlignLeft},
		{HeaderKey: "reports.budget_variance.col.program", Align: AlignLeft},
		{HeaderKey: "reports.budget_variance.col.currency", Align: AlignLeft},
	}
	for _, b := range buckets {
		cols = append(cols, Column{HeaderKey: b.start, Align: AlignRight})
	}
	cols = append(cols, Column{HeaderKey: "reports.budget_variance.col.total", Align: AlignRight})
	return cols
}

// sortBudgetKeys orders the pivot rows deterministically by their DISPLAY identity:
// account path, then fund name, then program name, then currency -- so the row order is
// stable across runs and reads top-to-bottom by the qualified label a reviewer sees.
func sortBudgetKeys(keys []BudgetKey, names keyNames) {
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if x, y := names.account[a.Account], names.account[b.Account]; x != y {
			return x < y
		}
		if x, y := names.fund[a.Fund], names.fund[b.Fund]; x != y {
			return x < y
		}
		if x, y := names.program[a.Program], names.program[b.Program]; x != y {
			return x < y
		}
		return a.Currency < b.Currency
	})
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
	prog := key.Program
	return &Drill{
		Scope:      key.Subsidiary,
		AccountIDs: []AccountID{key.Account},
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
	fund    map[FundID]string
	program map[ProgramID]string
}

// budgetKeyNames loads the account/fund/program display names a budget report row
// resolves, once per run. Account names are per-lang (D5); fund and program names are
// stored proper nouns (single Name).
func budgetKeyNames(ctx context.Context, tk *Toolkit, lang string) (keyNames, error) {
	st := tk.Store()
	// p30.9: the budget-variance row label is the FULLY-QUALIFIED (dotted-path) account
	// name, so a row identifies exactly which account -- reuse the shared hierarchy-path
	// machinery (per-lang, D5) instead of the bare leaf name.
	acct, err := st.AccountPaths(ctx, lang)
	if err != nil {
		return keyNames{}, err
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
		prog[ProgramID(n.ID)] = n.Name
	}
	return keyNames{account: acct, fund: fund, program: prog}, nil
}

// fundLabelCell builds the FUND column cell: the fund's stored name for a restricted
// key, or the localized "Unrestricted" label for fund 0 (D20).
func fundLabelCell(fund FundID, funds map[FundID]string) Cell {
	if fund == 0 {
		return LabelCell("reports.budget.unrestricted")
	}
	return TextCell(funds[fund])
}
