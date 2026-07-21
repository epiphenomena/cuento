package reports

import (
	"context"
	"sort"

	"cuento/internal/store"
)

// FundStatementReportID is the id (URL slug + registry key) of the FUND STATEMENT
// report: a full-detail, all-time line statement for ONE fund. Unlike the p15.8
// fund_activity statement (which aggregates a fund's period into Opening/Received/
// Applied/Closing lines), this is the raw LINE DETAIL — every split tagged the fund,
// across all accounts, GROUPED BY ACCOUNT, with the split's Date, Description, Memo,
// and signed native Amount, plus a per-(account,currency) subtotal. It has a fund
// selector but NO date range and NO granularity: it spans ALL of the fund's data
// (as-of the latest posting date via LedgerDateRange), the whole story of the fund.
//
// NATIVE currency (no conversion): a fund's splits read in the money they were
// received/spent in, mirroring the fund_activity single-fund statement. A multi-
// currency account therefore contributes one subtotal PER currency.
const FundStatementReportID = "fund_statement"

// registerFundStatement registers the fund-statement report into reg under the
// "funds" group. It offers ONLY the report-specific FUND selector (no period, no
// granularity); the window is the whole ledger (LedgerDateRange), computed in the run.
func registerFundStatement(reg *Registry) {
	reg.Register(Report{
		ID:         FundStatementReportID,
		TitleKey:   "reports.fund_statement.title",
		Group:      "funds",
		ParamsSpec: ParamsSpec{Fund: true},
		Run:        runFundStatement,
		// Line detail with a Date/Description/Memo/Amount register shape; not program-
		// dimensioned (mirrors fund_activity — a fund's line detail is balance-centric, and
		// the "funds" group carries no program-dimensioned report).
	})
}

// runFundStatement builds ONE fund's all-time line statement, grouped by account. No
// fund chosen (p.Fund == 0) yields an empty table (just the header), the framework's
// nothing-to-show rule, so a bare hit renders 200 with the params form.
func runFundStatement(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	cols := []Column{
		{HeaderKey: "reports.fund_statement.col.date", Align: AlignLeft},
		{HeaderKey: "reports.fund_statement.col.description", Align: AlignLeft},
		{HeaderKey: "reports.fund_statement.col.memo", Align: AlignLeft},
		{HeaderKey: "reports.fund_statement.col.currency", Align: AlignLeft},
		{HeaderKey: "reports.fund_statement.col.amount", Align: AlignRight},
		{HeaderKey: "reports.fund_statement.col.balance", Align: AlignRight},
	}
	if p.Fund == 0 {
		return Table{Columns: cols}, nil
	}

	// As-of = the newest posting date in the whole ledger (no upper bound on the fund's
	// story). An EMPTY ledger has no data, so the report is just its header.
	_, asof, ok, err := tk.Store().LedgerDateRange(ctx)
	if err != nil {
		return Table{}, err
	}
	if !ok {
		return Table{Columns: cols}, nil
	}

	rows, err := tk.Store().FundLedger(ctx, FundID(p.Fund), asof)
	if err != nil {
		return Table{}, err
	}

	acctNames, err := accountNameMap(ctx, tk, p.LangOr())
	if err != nil {
		return Table{}, err
	}
	// The chart-of-accounts pre-order (Tree order): accounts are sectioned in that
	// order so the statement reads type-by-type (assets, then liabilities, revenue,
	// expenses) exactly as the chart of accounts sorts.
	order, err := accountTreeOrder(ctx, tk, p.LangOr())
	if err != nil {
		return Table{}, err
	}

	t := Table{Columns: cols}

	// Group the fund's lines by account (preserving each account's internal
	// (date, split_id) order, which FundLedger already returns). One SECTION per
	// account the fund touches, ordered by the chart-of-accounts order.
	byAcct := make(map[AccountID][]store.FundLedgerRow)
	var acctIDs []AccountID
	for _, r := range rows {
		id := AccountID(r.AccountID)
		if _, seen := byAcct[id]; !seen {
			acctIDs = append(acctIDs, id)
		}
		byAcct[id] = append(byAcct[id], r)
	}
	sort.SliceStable(acctIDs, func(i, j int) bool {
		return order[acctIDs[i]] < order[acctIDs[j]]
	})

	for _, id := range acctIDs {
		lines := byAcct[id]
		// Account section header (a proper-noun account name as TEXT, spanning the row).
		t.Rows = append(t.Rows, Row{
			Cells: []Cell{
				TextCell(acctNames[id]),
				TextCell(""), TextCell(""), TextCell(""),
				BlankMoneyCell(), BlankMoneyCell(),
			},
			Kind: RowSubtotal,
		})
		// Per-(account,currency) running total across this account's lines, so the
		// subtotal foots to the sum of the account's amounts in each currency (int64,
		// no rounding). The FundLedger running_balance is fund-WIDE and account-order-
		// blind, so it is NOT reused here; each line shows the per-account running total.
		running := make(map[string]int64)
		for _, ln := range lines {
			running[ln.Currency] += ln.Amount
			t.Rows = append(t.Rows, Row{
				Cells: []Cell{
					DateCell(ln.Date).WithTxn(ln.TxnID),
					TextCell(ln.Description),
					TextCell(ln.SplitMemo),
					TextCell(ln.Currency),
					MoneyCell(ln.Amount, ln.Currency),
					MoneyCell(running[ln.Currency], ln.Currency),
				},
				Kind: RowData,
			})
		}
		// Account subtotal, one row per currency the account moved (sorted for
		// determinism). The subtotal Amount is the SUM of the account's line amounts in
		// that currency (== the closing per-account running total).
		ccys := make([]string, 0, len(running))
		for c := range running {
			ccys = append(ccys, c)
		}
		sort.Strings(ccys)
		for _, c := range ccys {
			t.Rows = append(t.Rows, Row{
				Cells: []Cell{
					LabelCell("reports.fund_statement.subtotal"),
					TextCell(""), TextCell(""),
					TextCell(c),
					MoneyCell(running[c], c),
					BlankMoneyCell(),
				},
				Kind: RowSectionTotal,
			})
		}
	}

	return t, nil
}

// accountTreeOrder returns account id -> its 0-based position in the chart-of-accounts
// pre-order (the store's Tree order), so a report can section accounts type-by-type in
// the same order the chart of accounts sorts. Accounts absent from the map sort last.
func accountTreeOrder(ctx context.Context, tk *Toolkit, lang string) (map[AccountID]int, error) {
	tree, err := tk.Store().Tree(ctx, lang, nil)
	if err != nil {
		return nil, err
	}
	m := make(map[AccountID]int, len(tree))
	for i, r := range tree {
		m[AccountID(r.ID)] = i
	}
	return m, nil
}
