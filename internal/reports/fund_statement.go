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
		//
		// Tree (p26 fund #21): the account dimension is a COLLAPSIBLE account hierarchy.
		// Placeholder parents (Assets, Revenue, Expenses) roll up their descendants as
		// nested subtotal rows; each leaf account is itself a collapsible parent over its
		// own detail lines. The generic template + treetable.js wire click-to-collapse
		// from the per-row Indent (data-depth).
		Tree: true,
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

	// The chart-of-accounts tree (pre-order) gives the account hierarchy: placeholder
	// parents (Assets, Revenue, Expenses), leaf accounts, resolved names, and each
	// account's depth. The account dimension is presented as this collapsible tree
	// (p26 fund #21) rather than a flat list of section headers.
	storeTree, err := tk.Store().Tree(ctx, p.LangOr(), nil)
	if err != nil {
		return Table{}, err
	}
	tree := toTreeNodes(storeTree)
	children, roots, isPlaceholder, name, depth, _ := indexTree(tree)

	t := Table{Columns: cols}

	// Group the fund's lines by leaf account (preserving each account's internal
	// (date, split_id) order, which FundLedger already returns). Only accounts the fund
	// touches carry lines; the tree walk surfaces just those leaves and their ancestors.
	byAcct := make(map[AccountID][]store.FundLedgerRow)
	for _, r := range rows {
		id := AccountID(r.AccountID)
		byAcct[id] = append(byAcct[id], r)
	}

	// subtreeCcys collects the distinct currencies moved beneath a node (a leaf's own,
	// a placeholder's descendants'), and subtreeSum their per-currency native sums, so a
	// placeholder parent can show a native rollup WHEN single-currency (mixed-currency
	// subtrees have no honest single native figure -- blank, mirroring balance_sheet's
	// native convention). hasAct marks whether a node's subtree carries any fund line, so
	// empty chart branches (no activity in this fund) drop out entirely.
	subtreeSum := make(map[AccountID]map[string]int64)
	hasAct := make(map[AccountID]bool)
	var fold func(id AccountID) (map[string]int64, bool)
	fold = func(id AccountID) (map[string]int64, bool) {
		if !isPlaceholder[id] {
			sum := map[string]int64{}
			for _, ln := range byAcct[id] {
				sum[ln.Currency] += ln.Amount
			}
			subtreeSum[id] = sum
			hasAct[id] = len(byAcct[id]) > 0
			return sum, hasAct[id]
		}
		sum := map[string]int64{}
		any := false
		for _, c := range children[id] {
			cs, act := fold(c)
			for ccy, v := range cs {
				sum[ccy] += v
			}
			any = any || act
		}
		subtreeSum[id] = sum
		hasAct[id] = any
		return sum, any
	}
	for _, r := range roots {
		fold(r)
	}

	// rollupCell returns the native rollup money cell for a placeholder parent: the
	// single-currency subtree sum (rare across mixed funds), else blank (no honest single
	// native figure). rollupCcy is the currency the cell carries (empty when blank).
	rollup := func(id AccountID) (Cell, string) {
		sum := subtreeSum[id]
		if len(sum) == 1 {
			for ccy, v := range sum {
				return MoneyCell(v, ccy), ccy
			}
		}
		return BlankMoneyCell(), ""
	}

	// Walk the tree pre-order (parent immediately precedes its subtree -- the treetable
	// data-depth contract). A placeholder parent WITH activity emits one nested subtotal
	// row at its depth; a leaf the fund touches emits a collapsible header at its depth,
	// then its detail lines and per-currency subtotal one level deeper (so the header is a
	// parent whose descendants -- the lines -- collapse as a unit).
	var walk func(id AccountID)
	walk = func(id AccountID) {
		if !hasAct[id] {
			return
		}
		if isPlaceholder[id] {
			cell, ccy := rollup(id)
			t.Rows = append(t.Rows, Row{
				Indent: depth[id],
				Cells: []Cell{
					TextCell(name[id]),
					TextCell(""), TextCell(""),
					TextCell(ccy),
					cell,
					BlankMoneyCell(),
				},
				Kind: RowSubtotal,
			})
			for _, c := range children[id] {
				walk(c)
			}
			return
		}

		lines := byAcct[id]
		lineDepth := depth[id] + 1

		// Leaf account header: a collapsible parent (its detail lines follow one level
		// deeper). Its amount cell rolls up the account's single-currency native sum (or
		// blank if the account is multi-currency, mirroring the placeholder convention).
		hdrCell, hdrCcy := rollup(id)
		t.Rows = append(t.Rows, Row{
			Indent: depth[id],
			Cells: []Cell{
				TextCell(name[id]),
				TextCell(""), TextCell(""),
				TextCell(hdrCcy),
				hdrCell,
				BlankMoneyCell(),
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
				Indent: lineDepth,
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
		// determinism), at the detail depth (a sibling of the lines). The subtotal Amount
		// is the SUM of the account's line amounts in that currency (== the closing
		// per-account running total).
		ccys := make([]string, 0, len(running))
		for c := range running {
			ccys = append(ccys, c)
		}
		sort.Strings(ccys)
		for _, c := range ccys {
			t.Rows = append(t.Rows, Row{
				Indent: lineDepth,
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
	for _, r := range roots {
		walk(r)
	}

	return t, nil
}
