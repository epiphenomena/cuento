package reports

import (
	"context"
	"sort"
)

// FundPeriodReportID is the id (URL slug + registry key) of the FUND ACTIVITY BY
// PERIOD report: an account x period MATRIX for ONE fund. Rows are the accounts the
// fund touches (each receipt/expense/application account, per native currency);
// columns are time periods per the chosen granularity (month/quarter/year), PLUS a
// Total column. A cell is the fund's ACTIVITY in that account for that period.
//
// It has a fund selector and a granularity control but NO date range: the window
// spans ALL of the fund's data (LedgerDateRange). GranNone ("total") collapses to a
// single Total column. NATIVE currency (no conversion), so a multi-currency account
// contributes one row per currency and every column foots to the Total by int64
// addition (a matrix that does not foot is wrong; native addition makes it exact).
const FundPeriodReportID = "fund_period"

// registerFundPeriod registers the fund-activity-by-period report into reg under the
// "funds" group. It offers the report-specific FUND selector and the granularity
// control (no period); the window is the whole ledger, computed in the run.
func registerFundPeriod(reg *Registry) {
	reg.Register(Report{
		ID:         FundPeriodReportID,
		TitleKey:   "reports.fund_period.title",
		Group:      "funds",
		ParamsSpec: ParamsSpec{Fund: true, Granularity: true},
		Run:        runFundPeriod,
		// The granular breakdown fans into many period columns -> render full-viewport
		// width so none truncate (mirrors the income statement's comparative layout).
		WideMatrix: true,
		// Not program-dimensioned (mirrors fund_activity; the "funds" group carries no
		// program-dimensioned report).
	})
}

// fundPeriodCell keys a matrix cell: one (account, currency) row over one period column.
type fundPeriodKey struct {
	acct AccountID
	ccy  string
}

// runFundPeriod builds ONE fund's account x period activity matrix. No fund chosen
// (p.Fund == 0) yields an empty table (just the Account + Total header columns), the
// framework's nothing-to-show rule, so a bare hit renders 200 with the params form.
func runFundPeriod(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	if p.Fund == 0 {
		return Table{Columns: []Column{
			{HeaderKey: "reports.fund_period.col.account", Align: AlignLeft},
			{HeaderKey: "reports.fund_period.col.currency", Align: AlignLeft},
			{HeaderKey: "reports.fund_period.col.total", Align: AlignRight},
		}}, nil
	}

	// Window = the whole ledger [lo, hi] (no from/to param). An EMPTY ledger has no
	// data, so the report is just its header. buildPeriods then decomposes [lo,hi] into
	// the comparative period columns per the granularity (GranNone => a single column).
	lo, hi, ok, err := tk.Store().LedgerDateRange(ctx)
	if err != nil {
		return Table{}, err
	}
	if !ok {
		return Table{Columns: []Column{
			{HeaderKey: "reports.fund_period.col.account", Align: AlignLeft},
			{HeaderKey: "reports.fund_period.col.currency", Align: AlignLeft},
			{HeaderKey: "reports.fund_period.col.total", Align: AlignRight},
		}}, nil
	}
	p.From, p.To = lo, hi

	periods, err := buildPeriods(tk, p)
	if err != nil {
		return Table{}, err
	}

	// The fund's splits across ALL accounts, as-of hi. Each carries a date, account,
	// native currency, and signed amount -- everything the matrix needs. Bucket each
	// split into its period column by date (the SAME [from,to] bounds buildPeriods used),
	// so a split lands in exactly one column and the columns foot to the Total.
	rows, err := tk.Store().FundLedger(ctx, FundID(p.Fund), hi)
	if err != nil {
		return Table{}, err
	}

	acctNames, err := accountNameMap(ctx, tk, p.LangOr())
	if err != nil {
		return Table{}, err
	}
	order, err := accountTreeOrder(ctx, tk, p.LangOr())
	if err != nil {
		return Table{}, err
	}

	// cells[col][key] = the fund's activity in (account, currency) during period col;
	// total[key] = the row's whole-window activity (sum of the period columns, int64).
	ncol := len(periods)
	cells := make([]map[fundPeriodKey]int64, ncol)
	for i := range cells {
		cells[i] = map[fundPeriodKey]int64{}
	}
	total := map[fundPeriodKey]int64{}
	rowSet := map[fundPeriodKey]bool{}

	for _, r := range rows {
		key := fundPeriodKey{acct: AccountID(r.AccountID), ccy: r.Currency}
		rowSet[key] = true
		total[key] += r.Amount
		// The period column whose [from,to] contains this split's date. Contiguous
		// buildPeriods columns are disjoint and cover [lo,hi], so exactly one matches.
		for i, pr := range periods {
			if r.Date >= pr.from && r.Date <= pr.to {
				cells[i][key] += r.Amount
				break
			}
		}
	}

	// Deterministic row order: chart-of-accounts order, then currency within an account.
	keys := make([]fundPeriodKey, 0, len(rowSet))
	for k := range rowSet {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if order[keys[i].acct] != order[keys[j].acct] {
			return order[keys[i].acct] < order[keys[j].acct]
		}
		return keys[i].ccy < keys[j].ccy
	})

	// GranNone ("total") collapses to a SINGLE Total column: buildPeriods yields one
	// whole-window bucket with an empty label, so a per-period column would just duplicate
	// the Total. Drop it and render only [Account, Currency, Total].
	granNone := ncol == 1 && periods[0].label == ""

	// Columns: Account, Currency, one per period (a period identifier header, verbatim),
	// then Total. Under GranNone the period columns are omitted (Total only).
	cols := []Column{
		{HeaderKey: "reports.fund_period.col.account", Align: AlignLeft},
		{HeaderKey: "reports.fund_period.col.currency", Align: AlignLeft},
	}
	if !granNone {
		for _, pr := range periods {
			cols = append(cols, Column{HeaderKey: pr.label, Align: AlignRight})
		}
	}
	cols = append(cols, Column{HeaderKey: "reports.fund_period.col.total", Align: AlignRight})

	t := Table{Columns: cols}
	for _, k := range keys {
		cellsRow := make([]Cell, 0, ncol+3)
		cellsRow = append(cellsRow, TextCell(acctNames[k.acct]), TextCell(k.ccy))
		if !granNone {
			for i := range periods {
				cellsRow = append(cellsRow, MoneyCell(cells[i][k], k.ccy))
			}
		}
		cellsRow = append(cellsRow, MoneyCell(total[k], k.ccy))
		t.Rows = append(t.Rows, Row{Cells: cellsRow, Kind: RowData})
	}

	return t, nil
}
