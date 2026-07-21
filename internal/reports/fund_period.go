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
		//
		// Tree (p26 fund #26b): the account dimension is a COLLAPSIBLE account hierarchy.
		// Placeholder parents roll up their descendants' period cells; each leaf account is
		// a collapsible parent over its per-currency rows. treetable.js wires it from the
		// per-row Indent (data-depth).
		Tree: true,
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

	// The chart-of-accounts tree (pre-order) gives the account hierarchy: placeholder
	// parents, leaf accounts, resolved names, and each account's depth. The account
	// dimension is presented as this collapsible tree (p26 fund #26b).
	storeTree, err := tk.Store().Tree(ctx, p.LangOr(), nil)
	if err != nil {
		return Table{}, err
	}
	tree := toTreeNodes(storeTree)
	children, roots, isPlaceholder, name, depth, typeOf := indexTree(tree)

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

	// GranNone ("total") collapses to a SINGLE Total column: buildPeriods yields one
	// whole-window bucket with an empty label, so a per-period column would just duplicate
	// the Total. Drop it and render only [Account, Currency, Total].
	granNone := ncol == 1 && periods[0].label == ""

	// Leading-zero trim (p26 fund #26a): a fund whose whole window starts before its
	// first activity carries a run of ALL-ZERO leading period columns (e.g. a 2025-Q1
	// with nothing in it). Drop that leading run -- the report should open on the first
	// period with activity -- while keeping interior/trailing zero columns (a quiet middle
	// quarter is meaningful) and the Total column. Under GranNone there are no period
	// columns to trim. firstActive = the first period index with any nonzero cell across
	// all (account, currency) keys; -1 means the whole matrix is zero (keep as-is).
	firstActive := 0
	if !granNone {
		firstActive = ncol // default: everything trimmed away only if all zero
		for i := 0; i < ncol; i++ {
			any := false
			for _, v := range cells[i] {
				if v != 0 {
					any = true
					break
				}
			}
			if any {
				firstActive = i
				break
			}
		}
		if firstActive == ncol {
			// All period columns are zero (all activity is a wash within Total): keep them
			// all rather than emitting a period-column-less matrix.
			firstActive = 0
		}
	}
	shownPeriods := periods
	shownCells := cells
	if !granNone {
		shownPeriods = periods[firstActive:]
		shownCells = cells[firstActive:]
	}

	// Deterministic row order via the tree walk (chart-of-accounts pre-order); within a
	// leaf account, per-currency rows sort by currency. Precompute each leaf's currencies.
	leafCcys := make(map[AccountID][]string)
	for k := range rowSet {
		leafCcys[k.acct] = append(leafCcys[k.acct], k.ccy)
	}
	for id := range leafCcys {
		sort.Strings(leafCcys[id])
	}

	// hasAct marks whether a node's subtree carries any fund activity, so empty chart
	// branches drop out. subtreeCcys collects the distinct currencies beneath a node so a
	// placeholder/leaf parent shows a native per-period rollup ONLY when single-currency
	// (mixed subtrees have no honest single native figure -- blank, mirroring the native
	// convention). subtreeCells[id][col] and subtreeTotal[id] roll the shown period cells
	// and Total for a single-currency subtree.
	hasAct := make(map[AccountID]bool)
	subtreeCcys := make(map[AccountID]map[string]bool)
	var fold func(id AccountID) (map[string]bool, bool)
	fold = func(id AccountID) (map[string]bool, bool) {
		if !isPlaceholder[id] {
			ccys := map[string]bool{}
			for _, c := range leafCcys[id] {
				ccys[c] = true
			}
			subtreeCcys[id] = ccys
			hasAct[id] = len(leafCcys[id]) > 0
			return ccys, hasAct[id]
		}
		ccys := map[string]bool{}
		any := false
		for _, c := range children[id] {
			cc, act := fold(c)
			for x := range cc {
				ccys[x] = true
			}
			any = any || act
		}
		subtreeCcys[id] = ccys
		hasAct[id] = any
		return ccys, any
	}
	for _, r := range roots {
		fold(r)
	}

	// rollupRow builds a parent's rolled-up cells (period columns + Total) for the given
	// subtree currency. Only called when the subtree is single-currency (the only case
	// with an honest native rollup); it sums the shown period cells and the Total over
	// every (leaf, ccy) key beneath id.
	subtreeKeys := func(id AccountID) []fundPeriodKey {
		var out []fundPeriodKey
		var add func(n AccountID)
		add = func(n AccountID) {
			if !isPlaceholder[n] {
				for _, c := range leafCcys[n] {
					out = append(out, fundPeriodKey{acct: n, ccy: c})
				}
				return
			}
			for _, c := range children[n] {
				add(c)
			}
		}
		add(id)
		return out
	}

	// Columns: Account, Currency, one per SHOWN period (a period identifier header,
	// verbatim), then Total. Under GranNone the period columns are omitted (Total only).
	cols := []Column{
		{HeaderKey: "reports.fund_period.col.account", Align: AlignLeft},
		{HeaderKey: "reports.fund_period.col.currency", Align: AlignLeft},
	}
	if !granNone {
		for _, pr := range shownPeriods {
			cols = append(cols, Column{HeaderKey: pr.label, Align: AlignRight})
		}
	}
	cols = append(cols, Column{HeaderKey: "reports.fund_period.col.total", Align: AlignRight})

	t := Table{Columns: cols}

	// parentRow emits a placeholder-or-leaf-account rollup row at the given indent: name,
	// a currency cell (the single subtree currency, or blank when mixed), one rollup cell
	// per shown period, and a Total. Mixed-currency parents show blank amount cells.
	parentRow := func(id AccountID, indent int) {
		ccys := subtreeCcys[id]
		single := ""
		if len(ccys) == 1 {
			for c := range ccys {
				single = c
			}
		}
		row := make([]Cell, 0, len(cols))
		row = append(row, TextCell(name[id]), TextCell(single))
		if single == "" {
			// Mixed-currency subtree: no honest single native figure -- blank cells.
			if !granNone {
				for range shownPeriods {
					row = append(row, BlankMoneyCell())
				}
			}
			row = append(row, BlankMoneyCell())
		} else {
			keys := subtreeKeys(id)
			if !granNone {
				for i := range shownPeriods {
					var sum int64
					for _, k := range keys {
						sum += shownCells[i][k]
					}
					row = append(row, MoneyCell(sum, single))
				}
			}
			var tot int64
			for _, k := range keys {
				tot += total[k]
			}
			row = append(row, MoneyCell(tot, single))
		}
		t.Rows = append(t.Rows, Row{Cells: row, Indent: indent, Kind: RowSubtotal})
	}

	// ACCOUNT TYPE is the TOP tier (p26 fund #5): the account tree nests one level deeper so
	// a localized TYPE header (Assets, Liabilities, Net assets, Revenue, Expenses -- reusing
	// the balance-sheet/income-statement section keys) sits at Indent 0. typeShift = +1
	// pushes every tree row down to make room for it. Per-cell figures are unchanged.
	const typeShift = 1

	// Walk the tree pre-order (the treetable data-depth contract). A placeholder parent
	// with activity emits a rollup row at its (shifted) depth; a leaf the fund touches emits
	// one RowData per currency at its (shifted) depth.
	var walk func(id AccountID)
	walk = func(id AccountID) {
		if !hasAct[id] {
			return
		}
		if isPlaceholder[id] {
			parentRow(id, depth[id]+typeShift)
			for _, c := range children[id] {
				walk(c)
			}
			return
		}
		// Leaf account: emit its per-currency data rows directly AT the leaf's depth (no
		// wrapper header -- a leaf here has no distinct detail lines, only its per-currency
		// rows, so mirroring trial_balance/balance_sheet a leaf is NOT wrapped; only a
		// placeholder parent gets a rollup subtotal). The placeholder ancestor collapses
		// these rows as its subtree.
		for _, ccy := range leafCcys[id] {
			k := fundPeriodKey{acct: id, ccy: ccy}
			row := make([]Cell, 0, len(cols))
			row = append(row, TextCell(name[id]), TextCell(ccy))
			if !granNone {
				for i := range shownPeriods {
					row = append(row, MoneyCell(shownCells[i][k], ccy))
				}
			}
			row = append(row, MoneyCell(total[k], ccy))
			t.Rows = append(t.Rows, Row{Cells: row, Indent: depth[id] + typeShift, Kind: RowData})
		}
	}

	// typeTotalRow appends one native TYPE-TOTAL row per currency (RowSectionTotal): the
	// label (Total assets / Total revenue / ...) and, per currency, the summed shown-period
	// cells and Total over every (leaf, ccy) key in the type. Native (no conversion) so each
	// currency has its OWN row (mirroring trial_balance's per-currency native totals -- a
	// type spanning USD+MXN has no honest single native figure). A currency's period columns
	// foot to its Total by int64 addition. Returns the per-currency Total, so the net-change
	// line can combine revenue + expense totals.
	typeTotalRow := func(labelKey string, keys []fundPeriodKey) map[string]int64 {
		byCcy := map[string][]fundPeriodKey{}
		for _, k := range keys {
			byCcy[k.ccy] = append(byCcy[k.ccy], k)
		}
		ccys := make([]string, 0, len(byCcy))
		for c := range byCcy {
			ccys = append(ccys, c)
		}
		sort.Strings(ccys)
		totals := map[string]int64{}
		for _, ccy := range ccys {
			row := make([]Cell, 0, len(cols))
			row = append(row, LabelCell(labelKey), TextCell(ccy))
			if !granNone {
				for i := range shownPeriods {
					var sum int64
					for _, k := range byCcy[ccy] {
						sum += shownCells[i][k]
					}
					row = append(row, MoneyCell(sum, ccy))
				}
			}
			var tot int64
			for _, k := range byCcy[ccy] {
				tot += total[k]
			}
			totals[ccy] = tot
			row = append(row, MoneyCell(tot, ccy))
			t.Rows = append(t.Rows, Row{Cells: row, Kind: RowSectionTotal})
		}
		return totals
	}

	// Group the roots by account TYPE and emit each as the top tier: a TYPE header (Indent
	// 0, native single-currency-or-blank rollup), the type's account subtree one level
	// deeper, then per-currency TYPE-TOTAL rows. Skip a type with no activity in this fund.
	// revTotals/expTotals capture the revenue and expense per-currency totals for the
	// net-change line below.
	order, byType := rootsByType(roots, typeOf)
	revTotals := map[string]int64{}
	expTotals := map[string]int64{}
	for _, typ := range order {
		var typeKeys []fundPeriodKey
		anyAct := false
		for _, r := range byType[typ] {
			if !hasAct[r] {
				continue
			}
			anyAct = true
			typeKeys = append(typeKeys, subtreeKeys(r)...)
		}
		if !anyAct {
			continue
		}
		// Type header (Indent 0): native single-currency rollup over the whole type, else
		// blank cells (a mixed-currency type has no honest single native figure).
		typeCcys := map[string]bool{}
		for _, k := range typeKeys {
			typeCcys[k.ccy] = true
		}
		single := ""
		if len(typeCcys) == 1 {
			for c := range typeCcys {
				single = c
			}
		}
		hdr := make([]Cell, 0, len(cols))
		hdr = append(hdr, LabelCell(accountTypeHeaderKey[typ]), TextCell(single))
		if single == "" {
			if !granNone {
				for range shownPeriods {
					hdr = append(hdr, BlankMoneyCell())
				}
			}
			hdr = append(hdr, BlankMoneyCell())
		} else {
			if !granNone {
				for i := range shownPeriods {
					var sum int64
					for _, k := range typeKeys {
						sum += shownCells[i][k]
					}
					hdr = append(hdr, MoneyCell(sum, single))
				}
			}
			var tot int64
			for _, k := range typeKeys {
				tot += total[k]
			}
			hdr = append(hdr, MoneyCell(tot, single))
		}
		t.Rows = append(t.Rows, Row{Cells: hdr, Indent: 0, Kind: RowSubtotal})

		for _, r := range byType[typ] {
			walk(r)
		}
		totals := typeTotalRow(accountTypeTotalKey[typ], typeKeys)
		switch typ {
		case "revenue":
			revTotals = totals
		case "expense":
			expTotals = totals
		}
	}

	// Net Change in Net Assets = -(Revenue + Expense) per currency. In net-debit space
	// revenue is negative (a credit) and expense positive (a debit), so the fund's surplus
	// (change in net assets) is the NEGATED sum of the revenue and expense totals -- e.g. a
	// fund with -50,000 revenue and 0 expense has a +50,000 change in net assets, matching
	// its asset growth. Emitted only when the fund has revenue or expense activity, one row
	// per currency (native; sums do not cross currencies). The period columns foot to Total.
	netCcys := map[string]bool{}
	for c := range revTotals {
		netCcys[c] = true
	}
	for c := range expTotals {
		netCcys[c] = true
	}
	if len(netCcys) > 0 {
		ccys := make([]string, 0, len(netCcys))
		for c := range netCcys {
			ccys = append(ccys, c)
		}
		sort.Strings(ccys)
		// Per-currency revenue+expense keys, to compute the net-change period columns.
		reKeys := map[string][]fundPeriodKey{}
		for _, typ := range []string{"revenue", "expense"} {
			for _, r := range byType[typ] {
				if !hasAct[r] {
					continue
				}
				for _, k := range subtreeKeys(r) {
					reKeys[k.ccy] = append(reKeys[k.ccy], k)
				}
			}
		}
		for _, ccy := range ccys {
			row := make([]Cell, 0, len(cols))
			row = append(row, LabelCell("reports.income_statement.net"), TextCell(ccy))
			if !granNone {
				for i := range shownPeriods {
					var sum int64
					for _, k := range reKeys[ccy] {
						sum += shownCells[i][k]
					}
					row = append(row, MoneyCell(-sum, ccy)) // negate: net-debit -> surplus
				}
			}
			row = append(row, MoneyCell(-(revTotals[ccy] + expTotals[ccy]), ccy))
			t.Rows = append(t.Rows, Row{Cells: row, Kind: RowTotal})
		}
	}

	return t, nil
}
