package reports

import (
	"context"
	"sort"

	"cuento/internal/store"
)

// FundReportReportID is the id (URL slug + registry key) of the FUND REPORT: the
// grant-reporting COVER PAGE for ONE fund. Unlike the other fund reports (each a single
// facet -- fund_activity's Opening/Received/Applied/Closing statement, fund_statement's
// by-account line detail, fund_period's account x period matrix), this is a COMPOSITION
// of those builders into one funder-facing page, narrowed to just the accounts the fund
// touched, in five labeled sections within one Table:
//
//   - STATUS band (per currency): Received, Spent so far (expense applications),
//     Remaining (the fund's held assets), % spent, and a fully-spent indicator. The
//     headline a funder reads first.
//   - RECEIPTS: the revenue/contribution activity INTO the fund, as a collapsible account
//     tree (revenue accounts only) with per-line date/description/memo/amount detail.
//   - EXPENSES by account: what the fund was spent ON, a collapsible account tree (expense
//     accounts only) with account subtotals down to split-level detail.
//   - ASSETS held: where the fund's money currently SITS -- the non-expense/asset side, a
//     collapsible account tree (asset accounts) with per-line detail.
//   - A reconciliation line per currency: Closing (spendable) == the all-asset
//     FundBalancesAsOf, proving the page foots.
//
// It has a fund selector and NO date range: the window spans ALL of the fund's data
// (LedgerDateRange), so a funder sees the whole grant from inception. NATIVE currency (no
// conversion): a fund's flows read in the money they were received/spent in, so a multi-
// currency grant (Beca Agua: MXN + USD) shows one status band PER currency and every
// section carries the currency column. GROUP funds; not program-dimensioned (mirrors the
// other fund reports -- a fund's balance/receipt position carries no coherent program
// filter).
const FundReportReportID = "fund_report"

// registerFundReport registers the fund-report cover page into reg under the "funds"
// group. It offers ONLY the report-specific FUND selector (no period, no granularity);
// the window is the whole ledger (LedgerDateRange), computed in the run.
func registerFundReport(reg *Registry) {
	reg.Register(Report{
		ID:         FundReportReportID,
		TitleKey:   "reports.fund_report.title",
		Group:      "funds",
		ParamsSpec: ParamsSpec{Fund: true},
		Run:        runFundReport,
		// The RECEIPTS / EXPENSES / ASSETS sections are collapsible account trees (the same
		// chart-of-accounts hierarchy fund_statement renders): placeholder parents and leaf
		// account headers as nested rows, each leaf's detail lines one level deeper. Tree so
		// the generic template + treetable.js wire click-to-collapse from each row's Indent.
		Tree: true,
		// Not program-dimensioned (mirrors fund_activity / fund_statement / fund_period: a
		// fund's balance-and-receipt position carries no coherently program-filterable
		// dimension, and the "funds" group has no program-dimensioned report).
	})
}

// fundReportCols is the shared 6-column schema of the fund report's single Table (reusing
// fund_statement's shape): a Line/Date column (a section/status LABEL, or a detail line's
// DATE), Description, Memo, Currency, Amount, and a %-spent column (the status band's
// only non-money use; blank elsewhere). The account-tree sections fill Date/Description/
// Memo/Amount per line; the status and reconciliation rows fill the label + currency +
// amount and leave the rest blank.
func fundReportCols() []Column {
	return []Column{
		{HeaderKey: "reports.fund_report.col.item", Align: AlignLeft},
		{HeaderKey: "reports.fund_report.col.description", Align: AlignLeft},
		{HeaderKey: "reports.fund_report.col.memo", Align: AlignLeft},
		{HeaderKey: "reports.fund_report.col.currency", Align: AlignLeft},
		{HeaderKey: "reports.fund_report.col.amount", Align: AlignRight},
		{HeaderKey: "reports.fund_report.col.pct", Align: AlignRight},
	}
}

// runFundReport builds ONE fund's grant-reporting cover page. No fund chosen (p.Fund ==
// 0) yields an empty table (just the header), the framework's nothing-to-show rule, so a
// bare hit renders 200 with the params form.
func runFundReport(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	cols := fundReportCols()
	if p.Fund == 0 {
		return Table{Columns: cols}, nil
	}

	// Window = the whole ledger [lo, hi] (no from/to param): the full grant story from
	// inception. An EMPTY ledger has no data, so the report is just its header. Setting
	// From/To to the ledger bounds makes Opening ~ 0 and the footing clean (Received −
	// AppliedExpense == all-asset FundBalancesAsOf(To) == Remaining).
	lo, hi, ok, err := tk.Store().LedgerDateRange(ctx)
	if err != nil {
		return Table{}, err
	}
	if !ok {
		return Table{Columns: cols}, nil
	}
	p.From, p.To = lo, hi

	scope := Scope{Sub: p.Scope}

	// The fund's aggregate period statement gives the STATUS figures: Received (revenue
	// INTO the fund), AppliedExpense (spent so far), Closing (spendable) and Capitalized
	// (deployed non-cash), per currency. Over the full window Opening ~ 0.
	st, err := tk.FundPeriodStatement(ctx, scope, p.Fund, p.From, p.To)
	if err != nil {
		return Table{}, err
	}

	// The all-asset FundBalancesAsOf(To) per currency -- the fund's REMAINING held assets
	// (the fund_activity LIST figure) and the reconciliation target (Closing + Capitalized
	// == all-asset balance).
	allAsset, err := tk.FundBalancesAsOf(ctx, scope, p.To, ConvertOpts{Mode: RateNone})
	if err != nil {
		return Table{}, err
	}
	allByCcy := amtsByCurrency(allAsset[p.Fund])

	// The fund's raw line detail (every split tagged the fund, as-of hi), grouped by
	// account -- the source for the RECEIPTS / EXPENSES / ASSETS trees.
	rows, err := tk.Store().FundLedger(ctx, FundID(p.Fund), hi)
	if err != nil {
		return Table{}, err
	}

	// The chart-of-accounts tree (pre-order): placeholder parents, leaf accounts, names,
	// depths, and each account's TYPE (so a section can be narrowed to revenue / expense /
	// asset accounts).
	storeTree, err := tk.Store().Tree(ctx, p.LangOr(), nil)
	if err != nil {
		return Table{}, err
	}
	tree := toTreeNodes(storeTree)
	children, roots, isPlaceholder, name, depth, typeOf := indexTree(tree)

	// Group the fund's lines by leaf account (preserving FundLedger's (date, split_id)
	// order within each account), for the tree sections.
	byAcct := make(map[AccountID][]store.FundLedgerRow)
	for _, r := range rows {
		byAcct[AccountID(r.AccountID)] = append(byAcct[AccountID(r.AccountID)], r)
	}

	sec := &fundReportTree{
		byAcct: byAcct, children: children, roots: roots, isPlaceholder: isPlaceholder,
		name: name, depth: depth, typeOf: typeOf,
	}

	t := Table{Columns: cols}

	// --- STATUS band (per currency) --------------------------------------------------
	// Section header, then one Received / Spent / Remaining / %-spent / fully-spent row
	// SET per currency the fund uses. Spent = AppliedExpense (the EXPENSES section);
	// Remaining = the all-asset FundBalancesAsOf (the fund's held position -- capitalized
	// assets are "where the money sits", not "spent"). % spent = Spent / Received.
	appendSectionHeader(&t, "reports.fund_report.section.status")
	for _, ccy := range st.Currencies {
		received := st.Received[ccy]
		spent := st.AppliedExpense[ccy]
		remaining := allByCcy[ccy]
		t.Rows = append(t.Rows, statusMoneyRow("reports.fund_report.received", ccy, received))
		t.Rows = append(t.Rows, statusMoneyRow("reports.fund_report.spent", ccy, spent))
		t.Rows = append(t.Rows, statusMoneyRow("reports.fund_report.remaining", ccy, remaining))
		// % spent (an integer percent TEXT cell -- there is no percent money kind) and a
		// fully-spent indicator, both derived from Received/Spent. A zero-Received fund has
		// no meaningful ratio (blank %, not fully spent).
		pctText, fully := spentStatus(received, spent)
		t.Rows = append(t.Rows, Row{
			Cells: []Cell{
				LabelCell("reports.fund_report.pct_spent"),
				TextCell(""), TextCell(""),
				TextCell(ccy),
				BlankMoneyCell(),
				TextCell(pctText),
			},
			Kind: RowData,
		})
		if fully {
			t.Rows = append(t.Rows, Row{
				Cells: []Cell{
					LabelCell("reports.fund_report.fully_spent"),
					TextCell(""), TextCell(""),
					TextCell(ccy),
					BlankMoneyCell(),
					TextCell(""),
				},
				Kind: RowData,
			})
		}
	}

	// --- RECEIPTS (revenue accounts) -------------------------------------------------
	// sign = -1: revenue is a credit (negative net-debit); negate so the funder reads a
	// positive inflow that matches the Status "Received" magnitude.
	appendSectionHeader(&t, "reports.fund_report.section.receipts")
	sec.emit(&t, "revenue", -1)

	// --- EXPENSES by account (expense accounts) --------------------------------------
	// sign = +1: a spend is a positive debit already (matches Status "Spent so far").
	appendSectionHeader(&t, "reports.fund_report.section.expenses")
	sec.emit(&t, "expense", +1)

	// --- ASSETS held (asset accounts) ------------------------------------------------
	// sign = +1: an asset debit balance is the fund's held position (positive).
	appendSectionHeader(&t, "reports.fund_report.section.assets")
	sec.emit(&t, "asset", +1)

	// --- Reconciliation (per currency): Closing + Capitalized == all-asset balance ----
	// The point is to show the FLOW-derived spendable Closing (from FundPeriodStatement)
	// agreeing with the BALANCE-derived all-asset FundBalancesAsOf -- two INDEPENDENT
	// computations. For a cash-only fund Closing == Total (visibly reconciling); for a
	// fund that capitalized a non-cash asset (a Building), Closing + Capitalized == Total,
	// so the deployed (Capitalized) line is shown when nonzero to make the identity read.
	appendSectionHeader(&t, "reports.fund_report.section.reconciliation")
	for _, ccy := range st.Currencies {
		// Closing (spendable) -- the flow-derived side.
		t.Rows = append(t.Rows, Row{
			Cells: []Cell{
				LabelCell("reports.fund_report.closing"),
				TextCell(""), TextCell(""),
				TextCell(ccy),
				MoneyCell(st.Closing[ccy], ccy),
				TextCell(""),
			},
			Kind: RowSubtotal,
		})
		// Capitalized / deployed (the non-cash held assets), only when the fund holds any.
		if capd := st.Capitalized[ccy]; capd != 0 {
			t.Rows = append(t.Rows, Row{
				Cells: []Cell{
					LabelCell("reports.fund_report.capitalized"),
					TextCell(""), TextCell(""),
					TextCell(ccy),
					MoneyCell(capd, ccy),
					TextCell(""),
				},
				Kind: RowSubtotal,
			})
		}
		// Total fund assets -- the balance-derived side. Closing + Capitalized == this.
		t.Rows = append(t.Rows, Row{
			Cells: []Cell{
				LabelCell("reports.fund_report.total_assets"),
				TextCell(""), TextCell(""),
				TextCell(ccy),
				MoneyCell(allByCcy[ccy], ccy),
				TextCell(""),
			},
			Kind: RowTotal,
		})
	}

	return t, nil
}

// spentStatus returns the integer-percent display text ("75%") and whether the fund is
// FULLY spent (spent >= received, with received > 0). A zero-received fund yields a blank
// percent and not-fully-spent (no ratio to compute). The percent is Spent/Received rounded
// to the nearest whole percent; a negative received (unusual) is treated as no-ratio.
func spentStatus(received, spent int64) (pct string, fully bool) {
	if received <= 0 {
		return "", false
	}
	// Round-half-up integer percent (exact int64 arithmetic; no float).
	p := (spent*100 + received/2) / received
	return itoaPct(p), spent >= received
}

// itoaPct formats an integer percent as "<n>%". A tiny local formatter so the report never
// pulls fmt into a hot path; percents are small non-negative integers here.
func itoaPct(n int64) string {
	// Handle the (defensive) negative case, though spentStatus never passes one.
	neg := n < 0
	if neg {
		n = -n
	}
	if n == 0 {
		return "0%"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	s := string(buf[i:]) + "%"
	if neg {
		return "-" + s
	}
	return s
}

// statusMoneyRow builds one STATUS-band money row: a localized line LABEL, the currency,
// and the money amount (no drill -- the status figures are aggregates the sections below
// detail). The %-spent column is blank for the money rows.
func statusMoneyRow(labelKey, ccy string, minor int64) Row {
	return Row{
		Cells: []Cell{
			LabelCell(labelKey),
			TextCell(""), TextCell(""),
			TextCell(ccy),
			MoneyCell(minor, ccy),
			TextCell(""),
		},
		Kind: RowData,
	}
}

// appendSectionHeader appends a SECTION header row (Indent 0, a localized LABEL in the
// first column, the rest blank), a RowSubtotal so the web renderer emphasizes it as a band
// header. Indent 0 keeps it OUTSIDE the collapsible account subtrees (whose rows sit at
// Indent >= 1 within a section), so treetable.js never folds a section header away.
func appendSectionHeader(t *Table, labelKey string) {
	t.Rows = append(t.Rows, Row{
		Cells: []Cell{
			LabelCell(labelKey),
			TextCell(""), TextCell(""),
			TextCell(""),
			BlankMoneyCell(),
			TextCell(""),
		},
		Indent: 0,
		Kind:   RowSubtotal,
	})
}

// fundReportTree holds the indexed chart-of-accounts and the fund's per-account lines, so
// the RECEIPTS / EXPENSES / ASSETS sections each emit a collapsible account subtree
// NARROWED to one account type. It reuses fund_statement's tree-walk shape: placeholder
// parents and leaf-account headers as nested RowSubtotal rows, each leaf's detail lines and
// per-currency subtotal one level deeper.
type fundReportTree struct {
	byAcct        map[AccountID][]store.FundLedgerRow
	children      map[AccountID][]AccountID
	roots         []AccountID
	isPlaceholder map[AccountID]bool
	name          map[AccountID]string
	depth         map[AccountID]int
	typeOf        map[AccountID]string
}

// emit appends the collapsible account subtree for accounts of ONE type (revenue /
// expense / asset) into t, at Indent >= 1 (below the Indent-0 section header). Only
// accounts the fund touches surface (hasAct); a type with no activity in this fund emits
// nothing (the section header then stands alone -- an honest "no receipts/expenses/assets"
// line). The subtree is folded so a placeholder parent shows a single-currency native
// rollup (or blank when mixed), and each leaf account carries its detail lines + per-
// currency subtotal, mirroring fund_statement.
//
// sign is the DISPLAY sign applied to every money figure in the section (+1 leaves the
// raw net-debit amount; -1 negates it). RECEIPTS pass sign = -1 so a revenue CREDIT (a
// negative net-debit) reads as a POSITIVE inflow -- matching the Status band's "Received"
// magnitude -- so a funder never sees the same gift as +100,000 up top and -100,000 below.
// EXPENSES and ASSETS pass sign = +1 (a spend is a positive debit already).
func (s *fundReportTree) emit(t *Table, wantType string, sign int64) {
	// sectionShift = +1: every tree row sits one level BELOW the Indent-0 section header,
	// so the section header (like fund_statement's account-TYPE tier) is the collapse root.
	const sectionShift = 1

	// Fold subtree sums + activity flags over ONLY the accounts of wantType. A placeholder
	// parent's activity is the OR of its type-matching descendants; a leaf counts only when
	// its own type matches (so an asset leaf never appears in the EXPENSES tree).
	subtreeSum := make(map[AccountID]map[string]int64)
	hasAct := make(map[AccountID]bool)
	var fold func(id AccountID) (map[string]int64, bool)
	fold = func(id AccountID) (map[string]int64, bool) {
		if !s.isPlaceholder[id] {
			sum := map[string]int64{}
			act := false
			if s.typeOf[id] == wantType {
				for _, ln := range s.byAcct[id] {
					sum[ln.Currency] += sign * ln.Amount
				}
				act = len(s.byAcct[id]) > 0
			}
			subtreeSum[id] = sum
			hasAct[id] = act
			return sum, act
		}
		sum := map[string]int64{}
		any := false
		for _, c := range s.children[id] {
			cs, a := fold(c)
			for ccy, v := range cs {
				sum[ccy] += v
			}
			any = any || a
		}
		subtreeSum[id] = sum
		hasAct[id] = any
		return sum, any
	}
	for _, r := range s.roots {
		fold(r)
	}

	// rollup returns a placeholder/leaf parent's native rollup cell: the single-currency
	// subtree sum, else blank (a mixed-currency subtree has no honest single native figure,
	// mirroring the balance-sheet native convention). rollupCcy is the cell's currency.
	rollup := func(id AccountID) (Cell, string) {
		sum := subtreeSum[id]
		if len(sum) == 1 {
			for ccy, v := range sum {
				return MoneyCell(v, ccy), ccy
			}
		}
		return BlankMoneyCell(), ""
	}

	// Walk pre-order (parent immediately precedes its subtree -- the treetable data-depth
	// contract). A placeholder parent WITH type-matching activity emits one nested subtotal
	// row; a matching leaf emits a collapsible header, then its detail lines and per-
	// currency subtotal one level deeper.
	var walk func(id AccountID)
	walk = func(id AccountID) {
		if !hasAct[id] {
			return
		}
		if s.isPlaceholder[id] {
			cell, ccy := rollup(id)
			t.Rows = append(t.Rows, Row{
				Indent: s.depth[id] + sectionShift,
				Cells: []Cell{
					TextCell(s.name[id]),
					TextCell(""), TextCell(""),
					TextCell(ccy),
					cell,
					TextCell(""),
				},
				Kind: RowSubtotal,
			})
			for _, c := range s.children[id] {
				walk(c)
			}
			return
		}

		lines := s.byAcct[id]
		lineDepth := s.depth[id] + sectionShift + 1

		hdrCell, hdrCcy := rollup(id)
		t.Rows = append(t.Rows, Row{
			Indent: s.depth[id] + sectionShift,
			Cells: []Cell{
				TextCell(s.name[id]),
				TextCell(""), TextCell(""),
				TextCell(hdrCcy),
				hdrCell,
				TextCell(""),
			},
			Kind: RowSubtotal,
		})

		// Per-(account, currency) running total so each subtotal foots to the account's
		// line sum in that currency (int64, exact). The FundLedger running_balance is
		// fund-WIDE and account-order-blind, so it is NOT reused; each line shows the
		// per-account running total.
		running := make(map[string]int64)
		for _, ln := range lines {
			amt := sign * ln.Amount
			running[ln.Currency] += amt
			t.Rows = append(t.Rows, Row{
				Indent: lineDepth,
				Cells: []Cell{
					DateCell(ln.Date).WithTxn(ln.TxnID),
					TextCell(ln.Description),
					TextCell(ln.SplitMemo),
					TextCell(ln.Currency),
					MoneyCell(amt, ln.Currency),
					TextCell(""),
				},
				Kind: RowData,
			})
		}
		ccys := make([]string, 0, len(running))
		for c := range running {
			ccys = append(ccys, c)
		}
		sort.Strings(ccys)
		for _, c := range ccys {
			t.Rows = append(t.Rows, Row{
				Indent: lineDepth,
				Cells: []Cell{
					LabelCell("reports.fund_report.subtotal"),
					TextCell(""), TextCell(""),
					TextCell(c),
					MoneyCell(running[c], c),
					TextCell(""),
				},
				Kind: RowSectionTotal,
			})
		}
	}

	for _, r := range s.roots {
		walk(r)
	}
}
