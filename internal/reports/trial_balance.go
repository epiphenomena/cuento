package reports

import (
	"context"
	"sort"
)

// TrialBalanceReportID is the id (URL slug + registry key) of the trial-balance
// report (p15.3): the first real financial statement and the exemplar the rest of
// Phase 15 copies. A trial balance lists every account's as-of balance in the
// scope's descendant closure (D18), one row per (account, currency) so a
// multi-currency account (FX Clearing holds USD and MXN) is shown honestly, with a
// NATIVE amount column and a CONVERTED column (target currency at the as-of closing
// rate, D12). Balances are net-debit signed int64 minor units (D2): assets/expenses
// positive, revenue/liability/equity negative. The whole point of a trial balance is
// that it BALANCES — the per-currency native signed sum is exactly zero — so the
// report emits a total row per currency (native) plus a grand converted total, and
// the golden test asserts those totals are zero.
const TrialBalanceReportID = "trial_balance"

// registerTrialBalance registers the trial-balance report (p15.3) into reg under the
// "financial" group. It is the first report registered in Default(), replacing the
// p15.1 framework smoke report.
func registerTrialBalance(reg *Registry) {
	reg.Register(Report{
		ID:         TrialBalanceReportID,
		TitleKey:   "reports.trial_balance.title",
		Group:      "financial",
		ParamsSpec: ParamsSpec{AsOf: true, Currency: true},
		Run:        runTrialBalance,
		Tree:       true, // p26.26: nested account tree with rolled-up parent subtotals.
	})
}

// runTrialBalance computes the trial-balance Table. It reads the scope's per-account
// per-currency as-of balances through the toolkit (BalancesAsOf, RateNone), walks the
// account tree in pre-order for STABLE row order and resolved account NAMES (in the
// request language, p.LangOr), and emits one data row per (account, currency):
// account name, currency code, native signed amount, and the amount converted to the
// target currency at the as-of closing rate (ConvertMinorAt per cell — the exact D12
// rule the toolkit uses, applied per currency so a multi-currency account's cells stay
// aligned instead of being collapsed to one sum). It closes with a total row per
// currency (native signed sum — zero on a balanced ledger) and a grand converted
// total (the sum of every converted cell). Every row's amount columns carry the same
// currency the renderers format from, so both HTML (per-user) and CSV (machine-plain,
// which drops the currency code — hence the explicit currency column) are unambiguous.
func runTrialBalance(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	balances, err := tk.BalancesAsOf(ctx, Scope{Sub: p.Scope}, p.AsOf, ConvertOpts{Mode: RateNone})
	if err != nil {
		return Table{}, err
	}

	// Tree gives pre-order (stable) account order and resolved names in the request
	// language. The whole chart is walked so PLACEHOLDER PARENTS (Revenue, Expenses)
	// are emitted as nested subtotal rows rolling up their descendants (p26.26); only
	// accounts (or ancestors of accounts) that carry a balance in this scope surface.
	storeTree, err := tk.Store().Tree(ctx, p.LangOr(), nil)
	if err != nil {
		return Table{}, err
	}
	tree := toTreeNodes(storeTree)
	children, roots, isPlaceholder, name, depth, _ := indexTree(tree)

	// target is the converted-column currency. It is normally resolved by the web
	// layer from the scope base (ParamsSpec.Currency); an empty target (a hand-built
	// Params with no currency, e.g. the scope test) makes the converted column mirror
	// the native amount — handled per-cell below.
	target := p.TargetCurrency

	t := Table{
		Columns: []Column{
			{HeaderKey: "reports.trial_balance.col.account", Align: AlignLeft},
			{HeaderKey: "reports.trial_balance.col.currency", Align: AlignLeft},
			{HeaderKey: "reports.trial_balance.col.native", Align: AlignRight},
			{HeaderKey: "reports.trial_balance.col.converted", Align: AlignRight},
		},
	}

	// Pre-convert every leaf's per-currency native balance to the target currency (the
	// SINGLE column that rolls up cleanly: a parent whose subtree spans currencies has
	// no single native sum, but its CONVERTED subtotal is well-defined, D12). convOf
	// caches per (account, currency) so the leaf row and the parent fold agree exactly.
	type curConv struct {
		ccy  string
		nat  int64
		conv int64
	}
	leafCells := make(map[AccountID][]curConv, len(balances)) // acct -> sorted per-ccy
	convSubtree := make(map[AccountID]int64)                  // acct -> own+descendant converted
	for acctID, amts := range balances {
		cur := make([]CurAmt, len(amts))
		copy(cur, amts)
		sort.Slice(cur, func(i, j int) bool { return cur[i].Currency < cur[j].Currency })
		for _, a := range cur {
			conv := a.Minor
			if target != "" {
				conv, err = tk.ConvertMinorAt(ctx, a.Minor, a.Currency, target, p.AsOf)
				if err != nil {
					return Table{}, err
				}
			}
			leafCells[acctID] = append(leafCells[acctID], curConv{ccy: a.Currency, nat: a.Minor, conv: conv})
			convSubtree[acctID] += conv
		}
	}

	// Fold: a placeholder parent's converted subtotal = sum of its descendants'
	// converted amounts (mirrors the income-statement/balance-sheet rollup). hasAct
	// marks whether a node's subtree carries ANY in-scope balance, so empty
	// placeholders (a chart branch with no activity in this scope) drop out entirely.
	hasAct := make(map[AccountID]bool)
	var fold func(id AccountID) (int64, bool)
	fold = func(id AccountID) (int64, bool) {
		if !isPlaceholder[id] {
			_, ok := leafCells[id]
			hasAct[id] = ok
			return convSubtree[id], ok
		}
		var sum int64
		var any bool
		for _, c := range children[id] {
			cs, act := fold(c)
			sum += cs
			any = any || act
		}
		convSubtree[id] = sum
		hasAct[id] = any
		return sum, any
	}
	for _, r := range roots {
		fold(r)
	}

	// Native per-currency running totals (must net to zero — the balancing check) and
	// the converted grand total (sum of every LEAF converted cell — parents are
	// subtotals, not re-summed, so the grand total stays a leaf-only sum).
	nativeTotal := make(map[string]int64)
	var nativeOrder []string
	seenCcy := make(map[string]bool)
	var convertedTotal int64

	// Walk the tree in pre-order (parent immediately precedes its subtree — the
	// treetable data-depth contract). Each placeholder parent WITH activity emits ONE
	// nested subtotal row carrying its rolled-up converted total (native/currency
	// blank: a mixed-currency subtree has no single native figure). Each leaf emits one
	// data row per (account, currency), as before, at its tree depth.
	var walk func(id AccountID)
	walk = func(id AccountID) {
		if !hasAct[id] {
			return
		}
		if isPlaceholder[id] {
			t.Rows = append(t.Rows, Row{
				Indent: depth[id],
				Cells: []Cell{
					TextCell(name[id]),
					TextCell(""),
					BlankMoneyCell(),
					MoneyCell(convSubtree[id], convCurrency(target)),
				},
				Kind: RowSubtotal,
			})
			for _, c := range children[id] {
				walk(c)
			}
			return
		}
		for _, a := range leafCells[id] {
			// p15.3d: the NATIVE cell drills to the transactions behind this
			// (account, currency) as-of balance. The Drill mirrors the toolkit filter
			// that produced the cell -- the scope (descendant closure), this account,
			// this native currency, cumulative to AsOf -- so the drilled splits'
			// signed native sum reconciles to a.nat by construction. The converted
			// cell is left non-drillable in the retrofit (the native drill already
			// covers the underlying transactions); p15.4+ may drill converted columns
			// via the same Drill (which still lists native splits).
			nativeDrill := &Drill{
				Scope:      p.Scope,
				AccountIDs: []AccountID{id},
				Currency:   a.ccy,
				Mode:       DrillAsOf,
				AsOf:       p.AsOf,
			}
			convCcy := target
			if convCcy == "" {
				convCcy = a.ccy
			}
			t.Rows = append(t.Rows, Row{
				Indent: depth[id],
				Cells: []Cell{
					TextCell(name[id]),
					TextCell(a.ccy),
					MoneyCell(a.nat, a.ccy).WithDrill(nativeDrill),
					MoneyCell(a.conv, convCcy),
				},
				Kind: RowData,
			})
			if !seenCcy[a.ccy] {
				seenCcy[a.ccy] = true
				nativeOrder = append(nativeOrder, a.ccy)
			}
			nativeTotal[a.ccy] += a.nat
			convertedTotal += a.conv
		}
	}
	for _, r := range roots {
		walk(r)
	}
	sort.Strings(nativeOrder)

	// One native total row per currency (each is exactly zero on a balanced ledger —
	// that IS the trial balance). The native column carries the per-currency sum; the
	// converted column is blank (the converted grand total is the following row).
	for _, ccy := range nativeOrder {
		t.Rows = append(t.Rows, Row{
			Cells: []Cell{
				LabelCell("reports.trial_balance.total_native"),
				TextCell(ccy),
				MoneyCell(nativeTotal[ccy], ccy),
				BlankMoneyCell(),
			},
			Kind: RowSubtotal,
		})
	}

	// The converted grand total (target currency): the sum of every converted cell.
	// On the fixture the per-account rounding residuals cancel and this is exactly
	// zero, but that is a data property, not a guarantee — a report is a trial balance
	// because its NATIVE per-currency sums are zero; the converted total is presented
	// so a reviewer sees the whole statement reconciled in one currency.
	convCcy := target
	t.Rows = append(t.Rows, Row{
		Cells: []Cell{
			LabelCell("reports.trial_balance.total_converted"),
			TextCell(""),
			BlankMoneyCell(),
			MoneyCell(convertedTotal, convCcy),
		},
		Kind: RowTotal,
	})

	return t, nil
}

// convCurrency is the currency code a converted subtotal cell carries: the target
// currency when one is set (the normal case — the whole converted column is one
// currency), else empty. A mixed-currency parent subtotal is only meaningful under a
// real target (D12); the empty-target path (a bare native-mode Params, e.g. the scope
// test) does not assert parent arithmetic.
func convCurrency(target string) string { return target }
